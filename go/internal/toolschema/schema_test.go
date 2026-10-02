package toolschema

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// searchCode is a realistic MCP tool definition: nested objects, an array, a
// union of integer and string types, and a required argument. Nothing here is
// known to the Go compiler.
const searchCode = `{
  "type": "object",
  "properties": {
    "query":       {"type": "string",  "description": "what to search for"},
    "limit":       {"type": "integer", "description": "max results"},
    "include_docs":{"type": "boolean"},
    "paths":       {"type": "array", "items": {"type": "string"}},
    "offset":      {"type": "number"},
    "scope": {
      "type": "object",
      "properties": {
        "repo":   {"type": "string"},
        "branch": {"type": "string"},
        "depth":  {"type": "integer"}
      },
      "required": ["repo"]
    }
  },
  "required": ["query", "scope"]
}`

func compileSearch(t *testing.T) *Schema {
	t.Helper()
	doc, err := Parse([]byte(searchCode))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := Compile("search_code", doc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return s
}

func TestCompileProducesRealDescriptor(t *testing.T) {
	s := compileSearch(t)

	// The compiled schema must be a genuine descriptor, not a stand-in.
	if s.Message().Fields().Len() != 6 {
		t.Errorf("field count = %d, want 6", s.Message().Fields().Len())
	}
	if got := string(s.File().Package()); got != "tool_search_code" {
		t.Errorf("package = %q, want tool_search_code", got)
	}
	if got := s.Message().Fields().ByName("query").Kind(); got != protoreflect.StringKind {
		t.Errorf("query kind = %s, want string", got)
	}
	if got := s.Message().Fields().ByName("limit").Kind(); got != protoreflect.Int64Kind {
		t.Errorf("limit kind = %s, want int64", got)
	}
	if got := s.Message().Fields().ByName("paths"); !got.IsList() {
		t.Error("paths should be a repeated field")
	}
}

// A nested object must become a real nested message, not a flattened string.
func TestNestedObjectBecomesNestedMessage(t *testing.T) {
	s := compileSearch(t)
	fd := s.Message().Fields().ByName("scope")
	if fd == nil {
		t.Fatal("no scope field")
	}
	if fd.Kind() != protoreflect.MessageKind {
		t.Fatalf("scope kind = %s, want message", fd.Kind())
	}
	if fd.Message().Fields().Len() != 3 {
		t.Errorf("scope fields = %d, want 3", fd.Message().Fields().Len())
	}
	if fd.Message().Fields().ByName("repo") == nil {
		t.Error("scope.repo missing")
	}
}

func TestBindAndValidate(t *testing.T) {
	s := compileSearch(t)

	args, err := DecodeArgs([]byte(`{
		"query": "context",
		"limit": 25,
		"include_docs": true,
		"paths": ["src", "docs"],
		"offset": 1.5,
		"scope": {"repo": "pool", "branch": "main", "depth": 2}
	}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	msg, err := s.Bind(args)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := s.Validate(msg); err != nil {
		t.Fatalf("validate: %v", err)
	}

	if got := msg.Get(s.Message().Fields().ByName("query")).String(); got != "context" {
		t.Errorf("query = %q", got)
	}
	if got := msg.Get(s.Message().Fields().ByName("limit")).Int(); got != 25 {
		t.Errorf("limit = %d, want 25", got)
	}
	if got := msg.Get(s.Message().Fields().ByName("include_docs")).Bool(); !got {
		t.Error("include_docs should be true")
	}
	if got := msg.Get(s.Message().Fields().ByName("paths")).List().Len(); got != 2 {
		t.Errorf("paths len = %d, want 2", got)
	}
	scope := msg.Get(s.Message().Fields().ByName("scope")).Message()
	if got := scope.Get(scope.Descriptor().Fields().ByName("repo")).String(); got != "pool" {
		t.Errorf("scope.repo = %q, want pool", got)
	}
}

// A JSON number is a float64 in Go, so a tool declaring "integer" has to still
// accept 25. This is the coercion that makes dynamic binding usable.
func TestIntegerCoercion(t *testing.T) {
	s := compileSearch(t)
	for _, raw := range []string{
		`{"query":"x","scope":{"repo":"r"},"limit":25}`,
		`{"query":"x","scope":{"repo":"r"},"limit":25.0}`,
	} {
		args, err := DecodeArgs([]byte(raw))
		if err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		msg, err := s.Bind(args)
		if err != nil {
			t.Fatalf("bind %s: %v", raw, err)
		}
		if got := msg.Get(s.Message().Fields().ByName("limit")).Int(); got != 25 {
			t.Errorf("limit for %s = %d, want 25", raw, got)
		}
	}
}

// Callers write Go argument maps with int literals; JSON decoding produces
// float64. Both must land on the same field value.
func TestNativeGoTypesAreAccepted(t *testing.T) {
	s := compileSearch(t)
	msg, err := s.Bind(map[string]any{
		"query":  "ctx",
		"scope":  map[string]any{"repo": "pool", "depth": 3},
		"limit":  25, // int, not float64
		"offset": 1.5,
		"paths":  []any{"a", "b"},
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got := msg.Get(s.Message().Fields().ByName("limit")).Int(); got != 25 {
		t.Errorf("limit = %d, want 25", got)
	}
	if got := msg.Get(s.Message().Fields().ByName("offset")).Float(); got != 1.5 {
		t.Errorf("offset = %v, want 1.5", got)
	}
	scope := msg.Get(s.Message().Fields().ByName("scope")).Message()
	if got := scope.Get(scope.Descriptor().Fields().ByName("depth")).Int(); got != 3 {
		t.Errorf("nested int not bound: %d", got)
	}
}

func TestNonIntegerForIntegerFieldIsRejected(t *testing.T) {
	s := compileSearch(t)
	args, _ := DecodeArgs([]byte(`{"query":"x","scope":{"repo":"r"},"limit":25.5}`))
	if _, err := s.Bind(args); err == nil {
		t.Fatal("expected a fractional value to be rejected for an integer field")
	}
}

// Silently dropping an unrecognised argument is how a tool call quietly does
// nothing, so it has to be an error.
func TestUnknownArgumentIsRejected(t *testing.T) {
	s := compileSearch(t)
	args, _ := DecodeArgs([]byte(`{"query":"x","scope":{"repo":"r"},"repos":"typo"}`))
	_, err := s.Bind(args)
	if err == nil {
		t.Fatal("expected an unknown argument to be rejected")
	}
	if !strings.Contains(err.Error(), "repos") {
		t.Errorf("error should name the bad argument, got: %v", err)
	}
}

func TestMissingRequiredIsRejected(t *testing.T) {
	s := compileSearch(t)
	args, _ := DecodeArgs([]byte(`{"query":"x"}`))
	msg, err := s.Bind(args)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	err = s.Validate(msg)
	if err == nil {
		t.Fatal("expected missing required argument to be rejected")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should name the missing argument, got: %v", err)
	}
}

func TestEncodeJSONRoundTrips(t *testing.T) {
	s := compileSearch(t)
	args, _ := DecodeArgs([]byte(`{"query":"ctx","limit":5,"scope":{"repo":"pool"}}`))
	msg, err := s.Bind(args)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	encoded, err := s.EncodeJSON(msg)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(encoded), `"repo"`) {
		t.Errorf("encoded output lost the nested field: %s", encoded)
	}
	// The message must also survive a round trip on the binary wire format.
	raw, err := Bytes(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rt := dynamicpb.NewMessage(s.Message())
	if err := unmarshalInto(rt, raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := rt.Get(s.Message().Fields().ByName("query")).String(); got != "ctx" {
		t.Errorf("round-tripped query = %q, want ctx", got)
	}
}

// Two compiles of the same schema must agree, or a cached descriptor from one
// session would be meaningless in the next.
func TestCompileIsDeterministic(t *testing.T) {
	a, b := compileSearch(t), compileSearch(t)
	if a.File().Path() != b.File().Path() {
		t.Errorf("file paths differ: %q vs %q", a.File().Path(), b.File().Path())
	}
	if a.Describe() != b.Describe() {
		t.Errorf("descriptors differ between compiles:\n%s\n---\n%s", a.Describe(), b.Describe())
	}
}

func TestNonObjectRootIsRejected(t *testing.T) {
	doc, _ := Parse([]byte(`{"type":"string"}`))
	if _, err := Compile("bad", doc); err == nil {
		t.Fatal("expected a non-object root to be rejected")
	}
}

func TestEmptyToolNameIsRejected(t *testing.T) {
	doc, _ := Parse([]byte(`{"type":"object"}`))
	if _, err := Compile("", doc); err == nil {
		t.Fatal("expected an empty tool name to be rejected")
	}
}

// unmarshalInto keeps the round-trip test readable.
func unmarshalInto(m protoreflect.ProtoMessage, b []byte) error {
	return proto.Unmarshal(b, m)
}
