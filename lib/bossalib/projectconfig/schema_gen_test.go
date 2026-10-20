package projectconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/invopop/jsonschema"
)

// guard-id: projectconfig-schema-fresh

// importPath is this package's import path, which AddGoComments needs to key
// the doc comments it reads from source to the reflected types.
const importPath = "github.com/recurser/bossalib/projectconfig"

// TestSchemaGenerated generates schema/boss-project.schema.json from the Go
// types and requires the committed file to match. With -update it rewrites the
// file instead (`make projectconfig-schema`).
//
// It is a test, not a tool, so the generator can read unexported values (the
// slug and secret patterns) and invopop/jsonschema never links into a binary.
func TestSchemaGenerated(t *testing.T) {
	got, err := generateSchema()
	if err != nil {
		t.Fatal(err)
	}
	if *updateGolden {
		if err := os.WriteFile("schema/boss-project.schema.json", got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(got, Schema()) {
		t.Fatalf("%s is stale: the Go types changed without regenerating it; regenerate with `make projectconfig-schema`", SchemaPath)
	}
}

// generateSchema reflects Config into the draft 2020-12 schema. Descriptions
// come from Go doc comments, constraints with no Go value from `jsonschema`
// struct tags, and the version, patterns and example lists from the Go values
// decode and validation use, so the schema cannot drift from them.
func generateSchema() ([]byte, error) {
	r := &jsonschema.Reflector{
		Anonymous:      true,
		ExpandedStruct: true,
		Namer:          schemaName,
		Mapper:         protoObjectSchema,
	}
	// The package's .go sources sit in the test's working directory, natively
	// and under Bazel (the go_test lists them as data).
	if err := r.AddGoComments(importPath, "."); err != nil {
		return nil, err
	}
	r.LookupComment = func(typ reflect.Type, field string) string {
		key := typ.PkgPath() + "." + typ.Name()
		if field != "" {
			key += "." + field
		}
		comment := strings.Join(strings.Fields(r.CommentMap[key]), " ")
		if field == "" || typ.Kind() != reflect.Struct {
			return comment
		}
		// A field holding a proto-backed object: its description names the
		// message, then adds the field's own comment.
		if f, ok := typ.FieldByName(field); ok {
			if msg := protoMessageName(f.Type); msg != "" {
				return strings.TrimSpace("protojson " + msg + ". " + comment)
			}
		}
		return comment
	}

	s := r.Reflect(&Config{})
	s.ID = "boss-project.schema.json"
	s.Title = "Boss project config"

	props := func(typ reflect.Type) *jsonschema.Schema {
		if typ == reflect.TypeFor[Config]() {
			return s
		}
		return s.Definitions[schemaName(typ)]
	}
	prop := func(typ reflect.Type, key string) (*jsonschema.Schema, bool) {
		def := props(typ)
		if def == nil || def.Properties == nil {
			return nil, false
		}
		return def.Properties.Get(key)
	}

	type patch struct {
		typ   reflect.Type
		key   string
		apply func(*jsonschema.Schema)
	}
	examples := func(values []string) func(*jsonschema.Schema) {
		return func(p *jsonschema.Schema) {
			p.Examples = make([]any, len(values))
			for i, v := range values {
				p.Examples[i] = v
			}
		}
	}
	for _, p := range []patch{
		{reflect.TypeFor[Config](), "version", func(p *jsonschema.Schema) { p.Const = SchemaVersion }},
		{reflect.TypeFor[Config](), "project", func(p *jsonschema.Schema) { p.Pattern = projectSlug.String() }},
		{reflect.TypeFor[SecretRef](), "env", func(p *jsonschema.Schema) { p.Pattern = secretEnvName.String() }},
		{reflect.TypeFor[RepoSettings](), "mergeStrategy", examples(MergeStrategies)},
		{reflect.TypeFor[Cron](), "factoryStage", examples(FactoryStages)},
		{reflect.TypeFor[Cron](), "concurrencyPolicy", examples(CronConcurrencyPolicies)},
		{reflect.TypeFor[Trigger](), "type", examples(TriggerTypes)},
		{reflect.TypeFor[Trigger](), "concurrencyPolicy", examples(TriggerConcurrencyPolicy)},
		{reflect.TypeFor[Placement](), "mode", examples(PlacementModes)},
	} {
		target, ok := prop(p.typ, p.key)
		if !ok {
			return nil, fmt.Errorf("schema generator: %s has no JSON property %q", p.typ, p.key)
		}
		p.apply(target)
	}

	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return unescapeHTML(append(out, '\n'))
}

// unescapeHTML undoes encoding/json's HTML escaping of <, > and &, which
// invopop's MarshalJSON applies before any encoder setting can stop it, so
// descriptions such as "./<path>" stay readable. It refuses any result that
// does not decode to the same value.
func unescapeHTML(in []byte) ([]byte, error) {
	out := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(in))
	var before, after any
	if err := json.Unmarshal(in, &before); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(out), &after); err != nil || !reflect.DeepEqual(before, after) {
		return nil, fmt.Errorf("schema generator: unescaping HTML changed the schema (%v)", err)
	}
	return []byte(out), nil
}

// schemaName names a $defs entry: the Go type name with a lower-case first
// letter (SecretRef -> secretRef).
func schemaName(typ reflect.Type) string {
	name := typ.Name()
	if name == "" || typ.PkgPath() != importPath {
		return ""
	}
	first, size := utf8.DecodeRuneInString(name)
	return string(unicode.ToLower(first)) + name[size:]
}

// protoObjectSchema writes a proto-backed object as an open object named by
// its proto message: protojson keys flow through without a schema change.
func protoObjectSchema(typ reflect.Type) *jsonschema.Schema {
	msg := protoMessageName(typ)
	if msg == "" {
		return nil
	}
	return &jsonschema.Schema{Type: "object", Description: "protojson " + msg + "."}
}

// protoMessageName is the full proto message name behind a ProtoObject type
// (or a pointer to one), and "" for any other type.
func protoMessageName(typ reflect.Type) string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if !reflect.PointerTo(typ).Implements(protoDescribedType) {
		return ""
	}
	return string(reflect.New(typ).Interface().(protoDescribed).descriptor().FullName())
}
