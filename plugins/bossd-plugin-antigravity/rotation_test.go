package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeOAuthBlob is a synthetic (never-real) antigravity OAuth token payload
// used only to prove MaterializeAgentAccount's plumbing and secret-hygiene
// behavior. It is not a live credential.
var fakeOAuthBlob = []byte(`{"token":{"access_token":"fake-agy-access","token_type":"Bearer","refresh_token":"fake-agy-refresh"},"auth_method":"consumer","id_token_email":"user@example.com"}`)

func TestRotationCapabilityReportsHomeDirSupport(t *testing.T) {
	s, _ := newTestServer(t)
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

func TestMaterializeAccountWritesOAuthTokenFileSpec(t *testing.T) {
	s, _ := newTestServer(t)
	resp, err := s.MaterializeAgentAccount(context.Background(), &bossanovav1.MaterializeAgentAccountRequest{
		CredentialBlob: fakeOAuthBlob,
	})
	if err != nil {
		t.Fatalf("MaterializeAgentAccount: %v", err)
	}
	files := resp.GetFiles()
	if len(files) != 1 {
		t.Fatalf("Files = %v, want exactly one entry", files)
	}
	f := files[0]
	const wantRel = ".gemini/antigravity-cli/antigravity-oauth-token"
	if f.GetRelativePath() != wantRel {
		t.Errorf("RelativePath = %q, want %q", f.GetRelativePath(), wantRel)
	}
	if !bytes.Equal(f.GetContent(), fakeOAuthBlob) {
		t.Errorf("Content = %q, want byte-identical to input blob", f.GetContent())
	}
	if f.GetMode() != 0o600 {
		t.Errorf("Mode = %#o, want 0600", f.GetMode())
	}
	if resp.GetHomeDirEnvKey() != "HOME" {
		t.Errorf("HomeDirEnvKey = %q, want HOME", resp.GetHomeDirEnvKey())
	}
	if got := resp.GetEnv()["SSH_CLIENT"]; got != "1" {
		t.Errorf("Env[SSH_CLIENT] = %q, want 1", got)
	}
}

func TestMaterializeAccountNeverLogsTheOAuthBlob(t *testing.T) {
	var buf bytes.Buffer
	s := newServer(nil, zerolog.New(&buf))

	if _, err := s.MaterializeAgentAccount(context.Background(), &bossanovav1.MaterializeAgentAccountRequest{
		CredentialBlob: fakeOAuthBlob,
	}); err != nil {
		t.Fatalf("MaterializeAgentAccount: %v", err)
	}
	for _, secret := range []string{"fake-agy-access", "fake-agy-refresh"} {
		if bytes.Contains(buf.Bytes(), []byte(secret)) {
			t.Fatalf("log output leaked credential material %q: %s", secret, buf.String())
		}
	}
}
