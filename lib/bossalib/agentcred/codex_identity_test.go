package agentcred

import (
	"encoding/base64"
	"testing"
)

func TestCodexIdentityClaims(t *testing.T) {
	payload := `{"email":"seat@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"workspace","chatgpt_user_id":"seat"}}`
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	accountID, userID, email, ok := CodexIdentityClaims(token)
	if !ok || accountID != "workspace" || userID != "seat" || email != "seat@example.test" {
		t.Fatalf("unexpected claims: %q %q %q %v", accountID, userID, email, ok)
	}
	for _, invalid := range []string{"garbage", "h.!.s", "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":1}`)) + ".s"} {
		if _, _, _, ok := CodexIdentityClaims(invalid); ok {
			t.Fatalf("accepted malformed token")
		}
	}
}
