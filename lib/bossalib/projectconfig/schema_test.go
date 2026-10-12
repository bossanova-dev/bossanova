package projectconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestSchemaParity walks the Go types and the embedded schema together and
// requires identical key sets, with additionalProperties: false, at every
// first-class object. Proto-backed objects must be open objects that name
// their proto message.
func TestSchemaParity(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatal("schema is not draft 2020-12")
	}
	defs, _ := schema["$defs"].(map[string]any)
	resolve := func(node map[string]any) map[string]any {
		ref, ok := node["$ref"].(string)
		if !ok {
			return node
		}
		def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if !ok {
			t.Fatalf("dangling $ref %s", ref)
		}
		return def
	}

	visited := map[reflect.Type]bool{}
	var walk func(path string, typ reflect.Type, node map[string]any)
	walk = func(path string, typ reflect.Type, node map[string]any) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		node = resolve(node)
		switch {
		case reflect.PointerTo(typ).Implements(protoDescribedType):
			msg := reflect.New(typ).Interface().(protoDescribed).descriptor().FullName()
			if node["type"] != "object" || node["additionalProperties"] == false {
				t.Errorf("%s: proto-backed object must be an open object", path)
			}
			if desc, _ := node["description"].(string); !strings.Contains(desc, string(msg)) {
				t.Errorf("%s: description must name %s", path, msg)
			}
			return
		case typ.Kind() == reflect.Slice:
			items, ok := node["items"].(map[string]any)
			if node["type"] != "array" || !ok {
				t.Errorf("%s: must be an array with items", path)
				return
			}
			walk(path+"[]", typ.Elem(), items)
			return
		case typ.Kind() != reflect.Struct:
			return
		}
		visited[typ] = true
		if node["type"] != "object" || node["additionalProperties"] != false {
			t.Errorf("%s: first-class object needs type object and additionalProperties: false", path)
		}
		props, _ := node["properties"].(map[string]any)
		goKeys := slices.Sorted(func(yield func(string) bool) {
			for k := range jsonFields(typ) {
				if !yield(k) {
					return
				}
			}
		})
		schemaKeys := slices.Sorted(func(yield func(string) bool) {
			for k := range props {
				if !yield(k) {
					return
				}
			}
		})
		if !slices.Equal(goKeys, schemaKeys) {
			t.Errorf("%s: Go keys %v != schema keys %v", path, goKeys, schemaKeys)
			return
		}
		fields := jsonFields(typ)
		for _, k := range goKeys {
			child, _ := props[k].(map[string]any)
			walk(joinKey(path, k), fields[k].Type, child)
		}
	}
	walk("", reflect.TypeFor[Config](), schema)

	for _, typ := range []reflect.Type{
		reflect.TypeFor[Repo](), reflect.TypeFor[RepoSettings](), reflect.TypeFor[RepoSecrets](),
		reflect.TypeFor[SecretRef](), reflect.TypeFor[Daemon](), reflect.TypeFor[Cron](),
		reflect.TypeFor[Cloud](), reflect.TypeFor[Trigger](), reflect.TypeFor[Placement](), reflect.TypeFor[Webhook](),
	} {
		if !visited[typ] {
			t.Errorf("%s never reached by the parity walk", typ)
		}
	}
}
