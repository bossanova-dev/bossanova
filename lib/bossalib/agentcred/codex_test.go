package agentcred

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

func TestParseCodexDeviceAuthPrompt(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   CodexDevicePrompt
		ok     bool
	}{
		{
			"url and code",
			"To sign in, open\nhttps://auth.openai.com/codex/device?user_code=ABCD-EFGHI\nand enter code ABCD-EFGHI",
			CodexDevicePrompt{URL: "https://auth.openai.com/codex/device?user_code=ABCD-EFGHI", Code: "ABCD-EFGHI"},
			true,
		},
		{
			"ansi styled",
			"\x1b[1mhttps://auth.openai.com/codex/device\x1b[0m code: \x1b[32mA1B2-C3D4E\x1b[0m",
			CodexDevicePrompt{URL: "https://auth.openai.com/codex/device", Code: "A1B2-C3D4E"},
			true,
		},
		{"url only (partial chunk)", "visit https://auth.openai.com/codex/device soon", CodexDevicePrompt{}, false},
		{"code only", "enter WXYZ-12345 to continue", CodexDevicePrompt{}, false},
		{"neither", "starting login…", CodexDevicePrompt{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseCodexDeviceAuthPrompt(tt.output)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("got (%+v,%v), want (%+v,%v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func authJSON(t *testing.T, tokens map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tokens": tokens, "last_refresh": "2026-07-03T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidateCodexAuthJSON(t *testing.T) {
	full := map[string]any{
		"access_token": "at", "refresh_token": "rt",
		"id_token": "it", "account_id": "acc",
	}
	got, err := ValidateCodexAuthJSON(authJSON(t, full))
	if err != nil {
		t.Fatalf("valid auth.json rejected: %v", err)
	}
	if got.IDToken != "it" || got.AccessToken != "at" || got.RefreshToken != "rt" || got.AccountID != "acc" {
		t.Fatalf("bad parse: %+v", got)
	}

	for _, missing := range []string{"access_token", "refresh_token", "id_token"} {
		tokens := map[string]any{}
		for k, v := range full {
			if k != missing {
				tokens[k] = v
			}
		}
		if _, err := ValidateCodexAuthJSON(authJSON(t, tokens)); !errors.Is(err, ErrCodexAuthMissingField) {
			t.Fatalf("missing %s: got %v, want ErrCodexAuthMissingField", missing, err)
		}
	}

	// account_id is optional — do not fail on its absence.
	tokens := map[string]any{"access_token": "at", "refresh_token": "rt", "id_token": "it"}
	if _, err := ValidateCodexAuthJSON(authJSON(t, tokens)); err != nil {
		t.Fatalf("missing account_id should be tolerated: %v", err)
	}

	if _, err := ValidateCodexAuthJSON([]byte("{not json")); !errors.Is(err, ErrCodexAuthMalformed) {
		t.Fatalf("malformed: got %v, want ErrCodexAuthMalformed", err)
	}
	if _, err := ValidateCodexAuthJSON([]byte(`{"no_tokens": true}`)); !errors.Is(err, ErrCodexAuthMissingField) {
		t.Fatalf("no tokens object: got %v, want ErrCodexAuthMissingField", err)
	}
}

func TestCodexAccountStoreJSON(t *testing.T) {
	blob, err := CodexAccountStoreJSON(CodexAuth{
		AccessToken: "at", RefreshToken: "rt", IDToken: "it", AccountID: "acc",
	})
	if err != nil {
		t.Fatalf("CodexAccountStoreJSON: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("output is not a flat JSON object: %v", err)
	}
	// The canonical account-store shape carries exactly access/refresh/id_token
	// (the keys the daemon validates); account_id is intentionally omitted.
	want := map[string]string{"access": "at", "refresh": "rt", "id_token": "it"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["account_id"]; ok {
		t.Fatalf("account_id must be omitted from the account-store shape: %v", got)
	}
}

func TestEmailFromIDToken(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"person@example.com"}`))
	if got, ok := EmailFromIDToken("hdr." + payload + ".sig"); !ok || got != "person@example.com" {
		t.Fatalf("got (%q,%v)", got, ok)
	}
	for _, bad := range []string{"", "one.part", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"no":"email"}`)) + ".c"} {
		if _, ok := EmailFromIDToken(bad); ok {
			t.Fatalf("EmailFromIDToken(%q) unexpectedly ok", bad)
		}
	}
}
