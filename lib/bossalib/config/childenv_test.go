package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestChildEnvPassthroughPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	settings := Settings{ChildEnvPassthrough: []string{"CUSTOM_SECRET", "PROVIDER_*"}}
	if err := SaveTo(path, settings); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ChildEnvPassthrough, settings.ChildEnvPassthrough) {
		t.Fatalf("ChildEnvPassthrough = %v, want %v", got.ChildEnvPassthrough, settings.ChildEnvPassthrough)
	}
}
