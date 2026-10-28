package scenario

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestSchemaCompleteness(t *testing.T) {
	schema := JSONSchema()
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatal(schema)
	}
	assertSchemaFields(t, reflect.TypeFor[Document](), schema)
}
func assertSchemaFields(t *testing.T, typ reflect.Type, node map[string]any) {
	t.Helper()
	properties := node["properties"].(map[string]any)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		child, ok := properties[name].(map[string]any)
		if !ok {
			t.Fatalf("missing %s.%s", typ.Name(), name)
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem()
			child = child["items"].(map[string]any)
		}
		if ft.Kind() == reflect.Struct {
			assertSchemaFields(t, ft, child)
		}
	}
}
func TestEnumAllowedMatchesProto(t *testing.T) {
	expected := []string{}
	for name, value := range pb.SessionState_value {
		if value != 0 {
			expected = append(expected, strings.TrimPrefix(name, "SESSION_STATE_"))
		}
	}
	sort.Strings(expected)
	_, ps := Decode([]byte(strings.Replace(validDocument, `"BLOCKED"`, `"BAD"`, 1)))
	got := requireProblem(t, ps, "sessions[0].state", "unsupported-value")
	if !slices.Equal(got.Allowed, expected) {
		t.Fatalf("got %v want %v", got.Allowed, expected)
	}
}
func TestCapabilities(t *testing.T) {
	c := Capabilities()
	if !slices.Equal(c.Operations, []string{"holdMerge", "holdArchive"}) {
		t.Fatal(c)
	}
	if len(c.UnsupportedInputs) != 3 {
		t.Fatal(c)
	}
	for _, p := range c.UnsupportedInputs {
		if p.Code != "unsupported-input" || p.Hint == "" {
			t.Fatal(p)
		}
	}
}
