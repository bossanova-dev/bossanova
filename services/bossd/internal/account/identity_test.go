package account

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestDeriveIdentity(t *testing.T) {
	token := func(workspace, user, email string) string {
		payload := fmt.Sprintf(`{"email":%q,"https://api.openai.com/auth":{"chatgpt_account_id":%q,"chatgpt_user_id":%q}}`, email, workspace, user)
		return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	}
	for _, tc := range []struct{ name, provider, credential, identity string }{
		{"store", "codex", fmt.Sprintf(`{"id_token":%q}`, token("workspace", "user", "ignored")), "codex\x00workspace\x00user"},
		{"auth file", "codex", fmt.Sprintf(`{"tokens":{"id_token":%q}}`, token("workspace", "user", "ignored")), "codex\x00workspace\x00user"},
		{"email fallback", "codex", fmt.Sprintf(`{"id_token":%q}`, token("workspace", "", "USER@Example.com")), "codex\x00workspace\x00user@example.com"},
		{"other workspace", "codex", fmt.Sprintf(`{"id_token":%q}`, token("other", "user", "ignored")), "codex\x00other\x00user"},
		{"missing user", "codex", fmt.Sprintf(`{"id_token":%q}`, token("workspace", "", "")), ""},
		{"missing workspace", "codex", fmt.Sprintf(`{"id_token":%q}`, token("", "user", "")), ""},
		{"claude", "claude", "setup-token", ""},
		{"garbage", "codex", "garbage", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveIdentity(context.Background(), tc.provider, "label", []byte(tc.credential), nil)
			want := domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindNone}
			if tc.identity != "" {
				digest := sha256.Sum256([]byte(tc.identity))
				want = domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindCodexUser, Digest: hex.EncodeToString(digest[:])}
			}
			if got != want || got.IsConfident() != (tc.identity != "") {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

// stubbedOrganizations maps tokens to organizations; an unknown token fails.
type stubbedOrganizations struct {
	orgs  map[string]string
	calls []string
}

func (f *stubbedOrganizations) OrganizationID(_ context.Context, token string) (string, error) {
	f.calls = append(f.calls, token)
	org, ok := f.orgs[token]
	if !ok {
		return "", errors.New("lookup failed")
	}
	return org, nil
}

func TestDeriveClaudeIdentity_Accounts_AC225501_AC225502(t *testing.T) {
	orgs := &stubbedOrganizations{orgs: map[string]string{"token-a": "org-1", "token-b": "org-1", "token-c": "org-2", "blank": "  "}}
	derive := func(label, token string) domain.AgentAccountIdentity {
		return DeriveIdentity(context.Background(), "claude", label, []byte(token), orgs)
	}
	work := derive("Work", " token-a\n")
	if work.Kind != domain.AgentAccountIdentityKindClaudeOrganizationLabel || len(work.Digest) != 64 || !work.IsConfident() {
		t.Fatalf("confident identity: %+v", work)
	}
	digest := sha256.Sum256([]byte("claude\x00org-1\x00work"))
	if work.Digest != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest = %s", work.Digest)
	}
	if orgs.calls[0] != "token-a" {
		t.Fatalf("token not trimmed: %q", orgs.calls[0])
	}
	if other := derive("  work ", "token-b"); other != work {
		t.Fatalf("same org and normalized label differ: %+v vs %+v", other, work)
	}
	if personal := derive("personal", "token-b"); personal.Digest == work.Digest || !personal.IsConfident() {
		t.Fatalf("different label linked: %+v", personal)
	}
	if otherOrg := derive("work", "token-c"); otherOrg.Digest == work.Digest {
		t.Fatal("different org linked")
	}
	none := domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindNone}
	for name, got := range map[string]domain.AgentAccountIdentity{
		"resolver error": derive("work", "unknown"),
		"empty org":      derive("work", "blank"),
		"empty label":    derive(" \t ", "token-a"),
		"empty token":    derive("work", "  "),
		"nil resolver":   DeriveIdentity(context.Background(), "claude", "work", []byte("token-a"), nil),
		"other provider": DeriveIdentity(context.Background(), "opencode", "work", []byte("token-a"), orgs),
	} {
		if got != none || got.IsConfident() {
			t.Errorf("%s: got %+v, want none", name, got)
		}
	}
}
