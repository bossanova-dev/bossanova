package plugin

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/models"
)

// fakeHostAccountEnv is an accountEnvResolver returning a fixed overlay.
type fakeHostAccountEnv struct {
	env map[string]string
	err error
}

func (f fakeHostAccountEnv) Resolve(context.Context, *models.Session) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.env, nil
}

// TestHostServiceResolveAccountEnvNil confirms a host service with no account
// resolver wired degrades to nil rather than panicking.
func TestHostServiceResolveAccountEnvNil(t *testing.T) {
	s := &HostServiceServer{}
	if got, _ := s.resolveAccountEnv(context.Background(), &models.Session{}); got != nil {
		t.Errorf("expected nil overlay with no resolver, got %v", got)
	}
}

// TestHostServiceResolveAccountEnvInjected confirms SetAccountEnvResolver wires
// the fake and the session is threaded through.
func TestHostServiceResolveAccountEnvInjected(t *testing.T) {
	s := &HostServiceServer{}
	want := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x"}
	s.SetAccountEnvResolver(fakeHostAccountEnv{env: want})
	if got, _ := s.resolveAccountEnv(context.Background(), &models.Session{AccountID: strp("a1")}); got["CLAUDE_CODE_OAUTH_TOKEN"] != "x" {
		t.Errorf("resolveAccountEnv = %v, want token x", got)
	}
}

// TestMergeAccountOverProof proves account overrides proof, disjoint keys
// survive from both, and an all-empty merge is nil (byte-identical to the
// pre-account behaviour dotenv.Overlay(proof) saw).
func TestMergeAccountOverProof(t *testing.T) {
	if got := mergeAccountOverProof(nil, nil); got != nil {
		t.Errorf("empty merge = %v, want nil", got)
	}
	proof := map[string]string{"PROOF_KEY": "p", "SHARED": "proof"}
	account := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x", "SHARED": "account"}
	got := mergeAccountOverProof(proof, account)
	if got["PROOF_KEY"] != "p" {
		t.Errorf("proof-only key lost: %v", got)
	}
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "x" {
		t.Errorf("account key missing: %v", got)
	}
	if got["SHARED"] != "account" {
		t.Errorf("SHARED = %q, want account (account overrides proof)", got["SHARED"])
	}
}

func strp(s string) *string { return &s }
