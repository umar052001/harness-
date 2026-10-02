package toolschema

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// SchemaNode is the subset of JSON Schema that tool definitions actually use.
//
// A full JSON Schema implementation would pull in the entire specification's
// vocabulary, most of which is irrelevant here. What matters is being explicit
// about the gap: keywords outside this set are recorded in Extra rather than
// dropped, so a caller can see what was not honoured.
type SchemaNode struct {
	Type        string                 `json:"type,omitempty"`
	Description string                 `json:"description,omitempty"`
	Properties  map[string]*SchemaNode `json:"properties,omitempty"`
	Required    []string               `json:"required,omitempty"`
	Items       *SchemaNode            `json:"items,omitempty"`
	Enum        []any                  `json:"enum,omitempty"`
	Default     any                    `json:"default,omitempty"`

	// Extra retains any keyword not modelled above, so nothing is silently lost.
	Extra map[string]json.RawMessage `json:"-"`
}

// Parse reads a JSON Schema document.
func Parse(raw []byte) (*SchemaNode, error) {
	var node SchemaNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	return &node, nil
}

// Unsupported lists keywords present in the document that this implementation
// does not enforce.
//
// Returned rather than swallowed: silently ignoring a constraint such as
// minLength would let a tool advertise validation it never performs.
func (n *SchemaNode) Unsupported() []string {
	var out []string
	for k := range n.Extra {
		out = append(out, k)
	}
	return out
}

// Bind populates a dynamic message from decoded JSON arguments.
//
// Values are coerced to the field's declared kind, because JSON numbers arrive
// as float64 and a tool that says "integer" should not fail merely because the
// caller encoded 3 rather than 3.0.
func (s *Schema) Bind(args map[string]any) (*dynamicpb.Message, error) {
	msg := s.New()

	// Reject unknown properties rather than dropping them. A silently ignored
	// argument is a bug that surfaces much later as a missing effect.
	var unknown []string
	for key := range args {
		if s.FieldByProperty(key) == nil {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown argument(s) %s; known: %s",
			strings.Join(unknown, ", "), strings.Join(s.properties(), ", "))
	}

	for key, val := range args {
		fd := s.FieldByProperty(key)
		if err := setField(msg, fd, val); err != nil {
			return nil, fmt.Errorf("argument %q: %w", key, err)
		}
	}
	return msg, nil
}

func (s *Schema) properties() []string {
	out := make([]string, 0, s.message.Fields().Len())
	for i := 0; i < s.message.Fields().Len(); i++ {
		out = append(out, s.message.Fields().Get(i).JSONName())
	}
	return out
}

// Validate checks required properties and declared enum membership.
//
// proto3 has no notion of a required field, so this is enforced here rather
// than in the descriptor. It runs before the message is handed on, because a
// missing required argument is a caller error and should not surface as a
// confusing failure inside the sandboxed process.
func (s *Schema) Validate(msg *dynamicpb.Message) error {
	var missing []string
	for _, prop := range s.required {
		fd := s.FieldByProperty(prop)
		if fd == nil {
			return fmt.Errorf("required property %q is not a known field", prop)
		}
		if !msg.Has(fd) {
			missing = append(missing, prop)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required argument(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// setField assigns one decoded JSON value onto a dynamic message.
func setField(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, val any) error {
	if fd == nil {
		return fmt.Errorf("no such field")
	}
	if fd.IsList() {
		items, ok := val.([]any)
		if !ok {
			return fmt.Errorf("expected an array, got %T", val)
		}
		list := msg.Mutable(fd).List()
		for i, item := range items {
			if err := appendScalar(list, fd, item); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
		return nil
	}
	if fd.Kind() == protoreflect.MessageKind {
		obj, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("expected an object, got %T", val)
		}
		sub := msg.Mutable(fd).Message()
		return setObject(sub, obj)
	}
	return setScalar(msg, fd, val)
}

// setObject fills a nested message from a decoded JSON object.
func setObject(msg protoreflect.Message, obj map[string]any) error {
	fields := msg.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		raw, ok := obj[fd.JSONName()]
		if !ok {
			continue
		}
		if fd.Kind() == protoreflect.MessageKind {
			sub, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("field %q: expected an object, got %T", fd.JSONName(), raw)
			}
			if err := setObject(msg.Mutable(fd).Message(), sub); err != nil {
				return err
			}
			continue
		}
		if err := setScalar(msg, fd, raw); err != nil {
			return err
		}
	}
	return nil
}

// setScalar coerces a JSON value to the field's declared kind.
func setScalar(msg protoreflect.Message, fd protoreflect.FieldDescriptor, val any) error {
	v, err := coerce(fd.Kind(), val)
	if err != nil {
		return fmt.Errorf("field %q: %w", fd.JSONName(), err)
	}
	msg.Set(fd, v)
	return nil
}

func appendScalar(list protoreflect.List, fd protoreflect.FieldDescriptor, item any) error {
	v, err := coerce(fd.Kind(), item)
	if err != nil {
		return err
	}
	list.Append(v)
	return nil
}

// coerce converts a decoded JSON value into a protoreflect.Value of the given
// kind.
func coerce(kind protoreflect.Kind, val any) (protoreflect.Value, error) {
	switch kind {
	case protoreflect.StringKind:
		s, ok := val.(string)
		if !ok {
			// Numbers are accepted because a caller may have encoded a numeric
			// id as a JSON number; rejecting it would be pedantry, not safety.
			switch n := val.(type) {
			case float64:
				return protoreflect.ValueOfString(strconv.FormatFloat(n, 'f', -1, 64)), nil
			case json.Number:
				return protoreflect.ValueOfString(n.String()), nil
			case int:
				return protoreflect.ValueOfString(strconv.Itoa(n)), nil
			case int64:
				return protoreflect.ValueOfString(strconv.FormatInt(n, 10)), nil
			case bool:
				return protoreflect.ValueOfString(strconv.FormatBool(n)), nil
			}
			return protoreflect.Value{}, fmt.Errorf("expected a string, got %T", val)
		}
		return protoreflect.ValueOfString(s), nil

	case protoreflect.Int64Kind:
		switch n := val.(type) {
		case float64:
			if n != math.Trunc(n) {
				return protoreflect.Value{}, fmt.Errorf("expected an integer, got %v", n)
			}
			return protoreflect.ValueOfInt64(int64(n)), nil
		case json.Number:
			i, err := n.Int64()
			if err != nil {
				return protoreflect.Value{}, err
			}
			return protoreflect.ValueOfInt64(i), nil
		// Go callers naturally build argument maps with int literals, and JSON
		// decodes them as float64. Both are the same number, so both are accepted
		// rather than making every caller cast by hand.
		case int:
			return protoreflect.ValueOfInt64(int64(n)), nil
		case int32:
			return protoreflect.ValueOfInt64(int64(n)), nil
		case int64:
			return protoreflect.ValueOfInt64(n), nil
		}
		return protoreflect.Value{}, fmt.Errorf("expected an integer, got %T", val)

	case protoreflect.DoubleKind:
		switch n := val.(type) {
		case float64:
			return protoreflect.ValueOfFloat64(n), nil
		case json.Number:
			f, err := n.Float64()
			if err != nil {
				return protoreflect.Value{}, err
			}
			return protoreflect.ValueOfFloat64(f), nil
		case float32:
			return protoreflect.ValueOfFloat64(float64(n)), nil
		case int:
			return protoreflect.ValueOfFloat64(float64(n)), nil
		case int64:
			return protoreflect.ValueOfFloat64(float64(n)), nil
		}
		return protoreflect.Value{}, fmt.Errorf("expected a number, got %T", val)

	case protoreflect.BoolKind:
		b, ok := val.(bool)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("expected a boolean, got %T", val)
		}
		return protoreflect.ValueOfBool(b), nil
	}
	return protoreflect.Value{}, fmt.Errorf("unsupported field kind %s", kind)
}

// EncodeJSON renders a bound message as protojson.
//
// protojson is used rather than encoding/json so that the field names, the
// int64-as-string convention and the nested message structure all match what a
// Protobuf consumer expects. The result round-trips through Bind unchanged.
func (s *Schema) EncodeJSON(msg *dynamicpb.Message) ([]byte, error) {
	// No resolver is needed: one is only consulted when expanding
	// google.protobuf.Any, and a tool input never contains one. Leaving it nil
	// also means the message need not be registered anywhere, which is the whole
	// point of building it at runtime.
	b, err := (protojson.MarshalOptions{Multiline: true}).Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return b, nil
}

// DecodeArgs parses raw JSON arguments.
func DecodeArgs(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode arguments: %w", err)
	}
	return args, nil
}

// Bytes returns the wire encoding of the message.
//
// Kept alongside the JSON form to show the message is a real Protobuf value on
// the wire, not just a struct that happens to print nicely.
func Bytes(msg *dynamicpb.Message) ([]byte, error) {
	return proto.Marshal(msg)
}
