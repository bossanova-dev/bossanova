package projectconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fixtureMap decodes the full fixture into a generic tree tests can mutate.
func fixtureMap(t *testing.T) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(readFixture(t, "full.json")))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// mutated returns the full fixture with edit applied, as JSON bytes.
func mutated(t *testing.T, edit func(m map[string]any)) []byte {
	t.Helper()
	m := fixtureMap(t)
	edit(m)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// obj walks a generic tree by keys (string) and indexes (int).
func obj(t *testing.T, root any, path ...any) map[string]any {
	t.Helper()
	cur := root
	for _, step := range path {
		switch s := step.(type) {
		case string:
			cur = cur.(map[string]any)[s]
		case int:
			cur = cur.([]any)[s]
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("path %v is not an object", path)
	}
	return m
}

func mustDecode(t *testing.T, data []byte) (*Config, []string) {
	t.Helper()
	cfg, warnings, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return cfg, warnings
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
