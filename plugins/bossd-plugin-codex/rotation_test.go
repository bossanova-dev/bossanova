package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeAuthBlob is a synthetic (never-real) codex auth.json payload used only
// to prove MaterializeAccount's plumbing and secret-hygiene behavior. It is
// not a live credential.
var fakeAuthBlob = []byte(`{"tokens":{"access_token":"fake-access","id_token":"fake-id"}}`)

func TestRotationCapabilityReportsHomeDirSupport(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.RotationCapability(context.Background(), &bossanovav1.RotationCapabilityRequest{})
	if err != nil {
		t.Fatalf("RotationCapability: %v", err)
	}
	if !resp.GetSupportsRotation() {
		t.Error("SupportsRotation = false, want true")
	}
	if resp.GetAuthKind() != bossanovav1.AuthKind_AUTH_KIND_HOME_DIR {
		t.Errorf("AuthKind = %v, want AUTH_KIND_HOME_DIR", resp.GetAuthKind())
	}
}

func TestMaterializeAccountWritesAuthJSONFileSpec(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.MaterializeAccount(context.Background(), &bossanovav1.MaterializeAccountRequest{
		CredentialBlob: fakeAuthBlob,
	})
	if err != nil {
		t.Fatalf("MaterializeAccount: %v", err)
	}
	files := resp.GetFiles()
	if len(files) != 1 {
		t.Fatalf("Files = %v, want exactly one entry", files)
	}
	f := files[0]
	if f.GetRelativePath() != "auth.json" {
		t.Errorf("RelativePath = %q, want auth.json", f.GetRelativePath())
	}
	if !bytes.Equal(f.GetContent(), fakeAuthBlob) {
		t.Errorf("Content = %q, want byte-identical to input blob %q", f.GetContent(), fakeAuthBlob)
	}
	if f.GetMode() != 0o600 {
		t.Errorf("Mode = %#o, want 0600", f.GetMode())
	}
	if resp.GetHomeDirEnvKey() != "CODEX_HOME" {
		t.Errorf("HomeDirEnvKey = %q, want CODEX_HOME", resp.GetHomeDirEnvKey())
	}
	if len(resp.GetEnv()) != 0 {
		t.Errorf("Env = %v, want empty (codex injects via home dir, not env)", resp.GetEnv())
	}
}

// TestMaterializeAccountNeverLogsTheAuthBlob is the secret-hygiene
// regression: neither token embedded in the fake auth.json blob may ever
// appear in any log line the server writes while servicing
// MaterializeAccount.
func TestMaterializeAccountNeverLogsTheAuthBlob(t *testing.T) {
	var buf bytes.Buffer
	s := newServer(nil, zerolog.New(&buf))

	if _, err := s.MaterializeAccount(context.Background(), &bossanovav1.MaterializeAccountRequest{
		CredentialBlob: fakeAuthBlob,
	}); err != nil {
		t.Fatalf("MaterializeAccount: %v", err)
	}
	for _, secret := range []string{"fake-access", "fake-id"} {
		if bytes.Contains(buf.Bytes(), []byte(secret)) {
			t.Fatalf("log output leaked credential material %q: %s", secret, buf.String())
		}
	}
}
