package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettings_ProofUploadRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	raw := `{"worktree_base_dir":"/tmp/wt","proof_upload":{"account_id":"acct","bucket":"proof","public_base_url":"https://proof.example","pages_enabled":true}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	want := ProofUploadConfig{AccountID: "acct", Bucket: "proof", PublicBaseURL: "https://proof.example", PagesEnabled: true}
	if s.ProofUpload != want {
		t.Fatalf("ProofUpload = %+v, want %+v", s.ProofUpload, want)
	}
	if err := SaveTo(path, s); err != nil {
		t.Fatal(err)
	}
	again, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.ProofUpload != want {
		t.Fatalf("after save ProofUpload = %+v, want %+v", again.ProofUpload, want)
	}
}

func TestSettings_ProofUploadAbsentIsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"worktree_base_dir":"/tmp/wt","proof_upload":{"bucket":"proof"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ProofUpload.PagesEnabled {
		t.Fatal("pages_enabled must default false")
	}
	s.ProofUpload = ProofUploadConfig{}
	if err := SaveTo(path, s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "proof_upload") {
		t.Fatalf("zero proof_upload was written: %s", data)
	}
	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProofUpload != (ProofUploadConfig{}) {
		t.Fatalf("absent key decoded as %+v", loaded.ProofUpload)
	}
}
