package scenario

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyOutsideRun(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	settings := filepath.Join(home, "settings.json")
	socket := filepath.Join(home, "d.sock")
	db := filepath.Join(home, "data", "bossd.db")
	data, _ := json.Marshal(map[string]string{"app_data_dir": filepath.Join(home, "data"), "socket_path": socket})
	if err := os.WriteFile(settings, data, 0600); err != nil {
		t.Fatal(err)
	}
	doc := Document{Version: 1, Name: "empty"}
	for _, opts := range []ApplyOptions{{Home: home, DB: filepath.Join(outside, "bossd.db"), Socket: socket}, {Home: home, DB: db, Socket: filepath.Join(outside, "d.sock")}} {
		result := Apply(context.Background(), doc, opts)
		if result.OK || len(result.Problems) != 1 || result.Problems[0].Code != "outside-run" {
			t.Fatalf("%+v", result)
		}
	}
	if err := os.Symlink(outside, filepath.Join(home, "worktrees")); err != nil {
		t.Fatal(err)
	}
	result := Apply(context.Background(), doc, ApplyOptions{Home: home, DB: db, Socket: socket})
	if len(result.Problems) != 1 || result.Problems[0].Code != "outside-run" {
		t.Fatalf("symlink escape: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(home, "scenario-state.json")); !os.IsNotExist(err) {
		t.Fatal("failed validation wrote state")
	}
}
func TestInvalidApplyDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	result := Apply(context.Background(), Document{}, ApplyOptions{Home: home})
	if result.OK || len(result.Problems) == 0 {
		t.Fatal(result)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid scenario changed home: %v %v", entries, err)
	}
}

func TestExternalSocketRequiresRunRecord(t *testing.T) {
	home := t.TempDir()
	socket := filepath.Join(t.TempDir(), "bossd.sock")
	db := filepath.Join(home, "data", "bossd.db")
	settings := filepath.Join(home, "settings.json")
	data, _ := json.Marshal(map[string]string{"app_data_dir": filepath.Dir(db), "socket_path": socket})
	if err := os.WriteFile(settings, data, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := ApplyOptions{Home: home, DB: db, Socket: socket}
	if ps := checkRun(opts); len(ps) != 1 || ps[0].Code != "outside-run" {
		t.Fatalf("unrecorded external socket accepted: %+v", ps)
	}
	record, _ := json.Marshal(map[string]any{"stateDir": home, "sockets": map[string]string{"bossd": socket}})
	if err := os.WriteFile(filepath.Join(home, "state.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	if ps := checkRun(opts); len(ps) != 0 {
		t.Fatalf("run-owned external socket rejected: %+v", ps)
	}
}
