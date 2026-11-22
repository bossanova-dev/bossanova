package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeDocumentFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDocumentSaveKeepsKeysItDidNotSet(t *testing.T) {
	path := writeDocumentFixture(t, `{
  "future_key": {"nested": [1, 2]},
  "cloud_control": "read-only",
  "repair": {"cooldown_minutes": 7},
  "big": 12345678901234567890,
  "daemon_name": "old"
}`)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	doc.Set("new", "daemon_name")
	doc.Set(true, "proof_upload", "pages_enabled")
	if err := doc.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"future_key":    map[string]any{"nested": []any{1.0, 2.0}},
		"cloud_control": "read-only",
		"repair":        map[string]any{"cooldown_minutes": 7.0},
		"big":           12345678901234567890.0,
		"daemon_name":   "new",
		"proof_upload":  map[string]any{"pages_enabled": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("saved %v, want %v", got, want)
	}
	if !bytes.Contains(data, []byte("12345678901234567890")) {
		t.Fatalf("an integer beyond float precision was rewritten: %s", data)
	}
}

func TestDocumentSetNilRemovesKey(t *testing.T) {
	path := writeDocumentFixture(t, `{"daemon_name": "studio", "other": 1}`)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	doc.Set(nil, "daemon_name")
	doc.Set(nil, "managed_accounts", "enabled")
	if err := doc.Save(); err != nil {
		t.Fatal(err)
	}
	got := readJSONFile(t, path)
	if !reflect.DeepEqual(got, map[string]any{"other": 1.0}) {
		t.Fatalf("saved %v, want only other", got)
	}
}

func TestDocumentManagedAccountsAdoptsLegacyRotation(t *testing.T) {
	path := writeDocumentFixture(t, `{"rotation": {"max_rotations_per_run": 9}}`)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	doc.Set(false, "managed_accounts", "enabled")
	settings, err := doc.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.ManagedAccounts.MaxRotations() != 9 || settings.ManagedAccounts.ManagedAgentAccountsEnabled() {
		t.Fatalf("managed accounts = %+v, want the legacy max_rotations kept and rotation off", settings.ManagedAccounts)
	}
}

func TestDocumentSetPluginConfig(t *testing.T) {
	path := writeDocumentFixture(t, `{"plugins": [
  {"name": "claude", "path": "/bin/claude-plugin", "enabled": true, "version": "1", "config": {"effort": "high", "model": "opus"}}
]}`)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	doc.SetPluginConfig("claude", "model", "")
	doc.SetPluginConfig("claude", "dangerously_skip_permissions", "true")
	doc.SetPluginConfig("codex", "sandbox", "read-only")
	doc.SetPluginConfig("opencode", "model", "")
	settings, err := doc.Settings()
	if err != nil {
		t.Fatal(err)
	}
	want := []PluginConfig{
		{Name: "claude", Path: "/bin/claude-plugin", Enabled: true, Version: "1", Config: map[string]string{"effort": "high", "dangerously_skip_permissions": "true"}},
		{Name: "codex", Config: map[string]string{"sandbox": "read-only"}},
	}
	if !reflect.DeepEqual(settings.Plugins, want) {
		t.Fatalf("plugins = %+v, want %+v", settings.Plugins, want)
	}
}

func TestDocumentSettingsMatchesLoadFrom(t *testing.T) {
	for name, body := range map[string]string{
		"empty agent": `{"default_agent": "", "poll_interval_seconds": 30}`,
		"legacy":      `{"rotation": {"enabled": false}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeDocumentFixture(t, body)
			want, err := LoadFrom(path)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := LoadDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := doc.Settings()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("document settings = %+v, want %+v", got, want)
			}
		})
	}
}

func TestDocumentSaveRefusesAfterConcurrentWrite(t *testing.T) {
	path := writeDocumentFixture(t, `{"daemon_name": "a"}`)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := `{"daemon_name": "a", "cloud_control": "read-only"}`
	if err := os.WriteFile(path, []byte(concurrent), 0o600); err != nil {
		t.Fatal(err)
	}
	doc.Set("b", "daemon_name")
	if err := doc.Save(); !errors.Is(err, ErrDocumentChanged) {
		t.Fatalf("Save = %v, want ErrDocumentChanged", err)
	}
	if data, _ := os.ReadFile(path); string(data) != concurrent {
		t.Fatalf("file = %s, want the concurrent write untouched", data)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("dir entries = %v (%v), want the temporary file removed", entries, err)
	}
}

func TestDocumentMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := doc.Settings(); err != nil || !reflect.DeepEqual(got, DefaultSettings()) {
		t.Fatalf("settings = %+v (%v), want defaults", got, err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	doc.Set("x", "daemon_name")
	if err := doc.Save(); !errors.Is(err, ErrDocumentChanged) {
		t.Fatalf("Save over a file created meanwhile = %v, want ErrDocumentChanged", err)
	}
}
