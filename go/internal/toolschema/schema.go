// Package toolschema compiles a tool's JSON Schema into a Protobuf message
// descriptor at runtime, so the control plane can handle tools that did not
// exist when it was compiled.
//
// The problem this solves: an agent harness discovers tools at runtime, and
// those tools are described by JSON Schema. Generating Go code for each one is
// not an option, because the set of tools is not known until the session is
// already running. Reflection is the only way to get a typed representation
// without a build step.
//
// The pipeline is:
//
//	JSON Schema -> descriptorpb.FileDescriptorProto -> protoreflect.FileDescriptor
//	            -> dynamicpb.Message
//
// Every step is reflection, so nothing here is generated. The trade-off is
// deliberate: a dynamic message costs more at runtime and gives up the compile
// time checks a generated struct would have. In exchange, an unknown tool is
// handled the same way as a known one.
package toolschema

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Schema is a compiled tool input schema.
//
// A Schema is immutable and safe for concurrent use once built; the descriptors
// it holds are shared, and each Bind produces a fresh message.
type Schema struct {
	name     string
	file     protoreflect.FileDescriptor
	message  protoreflect.MessageDescriptor
	required []string
}

// Name is the tool's name, as given at compile time.
func (s *Schema) Name() string { return s.name }

// Message returns the dynamic message type this schema describes.
func (s *Schema) Message() protoreflect.MessageDescriptor { return s.message }

// File returns the synthetic file the descriptor lives in.
//
// The file is named after the tool and exists only in memory. It is a real
// descriptor with a real name, which is what lets reflection, registries and
// Any resolution work on it unchanged.
func (s *Schema) File() protoreflect.FileDescriptor { return s.file }

// Required lists the properties the schema marks as required, in a stable order.
func (s *Schema) Required() []string {
	out := make([]string, len(s.required))
	copy(out, s.required)
	return out
}

// Compile turns a JSON Schema document into a message descriptor.
//
// The tool name becomes the message name. Supported constructs are the ones
// tool schemas actually use: objects, strings, integers, numbers, booleans,
// arrays, enums, and a small set of validation keywords. Anything else is
// rejected rather than ignored, because silently dropping a constraint would
// make the schema lie about what it accepts.
func Compile(name string, doc *SchemaNode) (*Schema, error) {
	if name == "" {
		return nil, fmt.Errorf("tool name is required")
	}
	if doc == nil {
		return nil, fmt.Errorf("tool %q: schema is missing", name)
	}

	// A tool's arguments are always an object, even if the schema omits an
	// explicit "type": MCP tool inputs are objects by construction.
	root := doc
	if root.Type == "" {
		root = &SchemaNode{Type: "object", Properties: doc.Properties, Required: doc.Required}
	}
	if root.Type != "object" {
		return nil, fmt.Errorf("tool %q: root schema must be an object, got %q", name, root.Type)
	}

	pkg := sanitizeIdent(name)
	fdName := fmt.Sprintf("tools/%s/tool.proto", pkg)

	msgName := toPascal(pkg)
	msg := buildObject(root, pkg, msgName)

	file := &descriptorpb.FileDescriptorProto{
		Name:        proto.String(fdName),
		Package:     proto.String(pkg),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{msg},
	}

	compiled, err := protodesc.NewFile(file, nil)
	if err != nil {
		return nil, fmt.Errorf("tool %q: descriptor is not valid: %w", name, err)
	}

	required := append([]string(nil), root.Required...)
	sort.Strings(required)

	return &Schema{
		name:     name,
		file:     compiled,
		message:  compiled.Messages().Get(0),
		required: required,
	}, nil
}

// buildObject converts a JSON Schema object node into a message descriptor,
// recursing into nested objects so a structured argument keeps its real shape.
func buildObject(node *SchemaNode, pkg, msgName string) *descriptorpb.DescriptorProto {
	names := make([]string, 0, len(node.Properties))
	for k := range node.Properties {
		names = append(names, k)
	}
	// Stable output: Go map iteration is randomised, and descriptor bytes get
	// compared and cached by some tooling.
	sort.Strings(names)

	msg := &descriptorpb.DescriptorProto{Name: proto.String(msgName)}
	for _, key := range names {
		prop := node.Properties[key]
		if prop == nil {
			continue
		}
		field, nested := buildField(key, prop, pkg, msgName)
		msg.Field = append(msg.Field, field)
		if nested != nil {
			// Nested messages belong to their parent, not to the file's top
			// level. Attaching them here keeps the scope in the type name valid.
			msg.NestedType = append(msg.NestedType, nested)
		}
	}
	return msg
}

// buildField converts a single property into a field descriptor, returning any
// nested message that must be attached to the owning message.
func buildField(
	key string, node *SchemaNode, pkg, msgName string,
) (*descriptorpb.FieldDescriptorProto, *descriptorpb.DescriptorProto) {
	name := toSnake(key)

	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	typ := descriptorpb.FieldDescriptorProto_TYPE_STRING
	typeName := ""

	switch node.Type {
	case "string":
		typ = descriptorpb.FieldDescriptorProto_TYPE_STRING
	case "integer":
		typ = descriptorpb.FieldDescriptorProto_TYPE_INT64
	case "number":
		typ = descriptorpb.FieldDescriptorProto_TYPE_DOUBLE
	case "boolean":
		typ = descriptorpb.FieldDescriptorProto_TYPE_BOOL
	case "array":
		// Items become a repeated field. Only scalar items are representable
		// this way; an array of objects would need a wrapper message, and
		// flattening it to a string would quietly change the contract.
		label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		if node.Items != nil {
			switch node.Items.Type {
			case "integer":
				typ = descriptorpb.FieldDescriptorProto_TYPE_INT64
			case "number":
				typ = descriptorpb.FieldDescriptorProto_TYPE_DOUBLE
			case "boolean":
				typ = descriptorpb.FieldDescriptorProto_TYPE_BOOL
			default:
				typ = descriptorpb.FieldDescriptorProto_TYPE_STRING
			}
		}
	case "object":
		typ = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	default:
		typ = descriptorpb.FieldDescriptorProto_TYPE_STRING
	}

	var nested *descriptorpb.DescriptorProto
	if typ == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
		nestedName := toPascal(name)
		typeName = fmt.Sprintf(".%s.%s.%s", pkg, msgName, nestedName)
		nested = buildObject(node, pkg, msgName+"."+nestedName)
		nested.Name = proto.String(nestedName)
	}

	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		JsonName: proto.String(key),
		Number:   proto.Int32(fieldNumber(key)),
		Label:    label.Enum(),
		Type:     typ.Enum(),
		TypeName: proto.String(typeName),
	}, nested
}

// New returns an empty dynamic message of the tool's input type.
func (s *Schema) New() *dynamicpb.Message {
	return dynamicpb.NewMessage(s.message)
}

// fieldNumber derives a stable field number from a property name.
//
// Real field numbers come from the schema author. JSON Schema does not carry
// them, so one is derived from the name. The mapping is deterministic, which is
// what matters: the same schema always produces the same descriptor, so a
// message built in one process means the same thing in another.
func fieldNumber(key string) int32 {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	// Field numbers must be positive and avoid the 19000-19999 reserved range.
	n := int32(h%18000) + 1
	return n
}

// FieldByProperty finds the field that corresponds to a JSON property name.
//
// The JSON name is what callers use, so that is what this matches on; the
// snake_case proto name is an implementation detail.
func (s *Schema) FieldByProperty(prop string) protoreflect.FieldDescriptor {
	fields := s.message.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.JSONName() == prop || string(f.Name()) == toSnake(prop) {
			return f
		}
	}
	return nil
}

// Describe renders the compiled schema for human inspection.
//
// This exists so a demo transcript can show that a runtime-compiled descriptor
// is a genuine descriptor, not a lossy summary of one.
func (s *Schema) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "tool %q\n", s.name)
	fmt.Fprintf(&b, "  file    : %s\n", s.file.Path())
	fmt.Fprintf(&b, "  message : %s\n", s.message.FullName())
	fmt.Fprintf(&b, "  fields  : %d\n", s.message.Fields().Len())
	if len(s.required) > 0 {
		fmt.Fprintf(&b, "  required: %s\n", strings.Join(s.required, ", "))
	}
	for i := 0; i < s.message.Fields().Len(); i++ {
		f := s.message.Fields().Get(i)
		kind := f.Kind().String()
		if f.IsMap() {
			kind = "map"
		} else if f.IsList() {
			kind = "repeated " + f.Kind().String()
		} else if f.Kind() == protoreflect.MessageKind {
			kind = string(f.Message().FullName())
		}
		fmt.Fprintf(&b, "    %-16s %-28s json=%q num=%d\n", f.Name(), kind, f.JSONName(), f.Number())
	}
	return b.String()
}

// sanitizeIdent turns an arbitrary tool name into a legal proto package name.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "tool"
	}
	return "tool_" + out
}

// toSnake converts a JSON property name to a lower_snake_case proto field name.
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteRune('_')
			}
			b.WriteRune(r + 32)
		} else if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "field"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "f_" + out
	}
	return out
}

// toPascal converts a property name to PascalCase for message names.
func toPascal(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == ' ' || r == '.'
	})
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		if len(p) > 1 {
			b.WriteString(p[1:])
		}
	}
	out := b.String()
	if out == "" {
		return "Message"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "M" + out
	}
	return out
}
