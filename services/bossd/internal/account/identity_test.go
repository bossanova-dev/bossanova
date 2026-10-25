package account

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
			got := DeriveIdentity(tc.provider, []byte(tc.credential))
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
