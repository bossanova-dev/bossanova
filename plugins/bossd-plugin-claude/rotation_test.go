package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeOAuthToken is a synthetic (never-real) claude OAuth token used only to
// prove MaterializeAccount's plumbing and secret-hygiene behavior. It is not
// a live credential.
const fakeOAuthToken = "sk-fake-oauth-token-abc123"

func TestRotationCapabilityReportsEnvTokenSupport(t *testing.T) {
	s := newServer(nil, zerolog.Nop())
	resp, err := s.RotationCapability(context.Background(), &bossanovav1.RotationCapabilityRequest{})
	if err != nil {
		t.Fatalf("RotationCapability: %v", err)
	}
	if !resp.GetSupportsRotation() {
		t.Error("SupportsRotation = false, want true")
	}
	if resp.GetAuthKind() != bossanovav1.AuthKind_AUTH_KIND_ENV_TOKEN {
		t.Errorf("AuthKind = %v, want AUTH_KIND_ENV_TOKEN", resp.GetAuthKind())
	}
}

func TestMaterializeAccountMapsTokenToEnvOverlay(t *testing.T) {
	s := newServer(nil, zerolog.Nop())
	resp, err := s.MaterializeAccount(context.Background(), &bossanovav1.MaterializeAccountRequest{
		CredentialBlob: []byte("  " + fakeOAuthToken + "  \n"),
	})
	if err != nil {
		t.Fatalf("MaterializeAccount: %v", err)
	}
	if got := resp.GetEnv()["CLAUDE_CODE_OAUTH_TOKEN"]; got != fakeOAuthToken {
		t.Errorf("Env[CLAUDE_CODE_OAUTH_TOKEN] = %q, want %q (trimmed)", got, fakeOAuthToken)
	}
	if len(resp.GetFiles()) != 0 {
		t.Errorf("Files = %v, want empty (claude has no home-dir materialization)", resp.GetFiles())
	}
	if resp.GetHomeDirEnvKey() != "" {
		t.Errorf("HomeDirEnvKey = %q, want empty", resp.GetHomeDirEnvKey())
	}
}

// TestMaterializeAccountNeverLogsTheToken is the secret-hygiene regression:
// the raw OAuth token must never appear in any log line the server writes
// while servicing MaterializeAccount, however verbose logging gets in the
// future.
func TestMaterializeAccountNeverLogsTheToken(t *testing.T) {
	var buf bytes.Buffer
	s := newServer(nil, zerolog.New(&buf))

	if _, err := s.MaterializeAccount(context.Background(), &bossanovav1.MaterializeAccountRequest{
		CredentialBlob: []byte(fakeOAuthToken),
	}); err != nil {
		t.Fatalf("MaterializeAccount: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte(fakeOAuthToken)) {
		t.Fatalf("log output leaked the credential token: %s", buf.String())
	}
}
