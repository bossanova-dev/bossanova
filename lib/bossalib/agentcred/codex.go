package agentcred

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	// ErrCodexAuthMalformed marks unparseable auth.json content.
	ErrCodexAuthMalformed = errors.New("agentcred: codex auth.json is not valid JSON")
	// ErrCodexAuthMissingField marks an auth.json missing a required token
	// field. id_token is REQUIRED: codex exchanges it during refresh and the
	// materializer must preserve it across merges (epic ticket 1.5).
	ErrCodexAuthMissingField = errors.New("agentcred: codex auth.json missing required field")

	codexDeviceURLRE  = regexp.MustCompile(`https://auth\.openai\.com/codex/device[^\s]*`)
	codexDeviceCodeRE = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{5}\b`)
)

// CodexDevicePrompt is the URL + user code the codex login device flow
// prints for the user to complete authentication in a browser.
type CodexDevicePrompt struct {
	URL  string
	Code string
}

// ParseCodexDeviceAuthPrompt scans accumulated CLI output for the device
// auth URL and code. ok is true only once BOTH are present (callers feed it
// the growing output buffer on every new chunk).
func ParseCodexDeviceAuthPrompt(output string) (CodexDevicePrompt, bool) {
	clean := StripANSI(output)
	url := codexDeviceURLRE.FindString(clean)
	code := codexDeviceCodeRE.FindString(clean)
	if url == "" || code == "" {
		return CodexDevicePrompt{}, false
	}
	return CodexDevicePrompt{URL: url, Code: code}, true
}

// CodexAuth is the validated token payload of a codex auth.json.
type CodexAuth struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
}

type codexAuthFile struct {
	Tokens *struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// ValidateCodexAuthJSON parses data as a codex auth.json and verifies the
// fields the rotation machinery depends on. account_id is optional.
func ValidateCodexAuthJSON(data []byte) (CodexAuth, error) {
	var f codexAuthFile
	if err := json.Unmarshal(data, &f); err != nil {
		return CodexAuth{}, fmt.Errorf("%w: %v", ErrCodexAuthMalformed, err)
	}
	if f.Tokens == nil {
		return CodexAuth{}, fmt.Errorf("%w: tokens", ErrCodexAuthMissingField)
	}
	auth := CodexAuth{
		AccessToken:  f.Tokens.AccessToken,
		RefreshToken: f.Tokens.RefreshToken,
		IDToken:      f.Tokens.IDToken,
		AccountID:    f.Tokens.AccountID,
	}
	for field, val := range map[string]string{
		"access_token":  auth.AccessToken,
		"refresh_token": auth.RefreshToken,
		"id_token":      auth.IDToken,
	} {
		if val == "" {
			return CodexAuth{}, fmt.Errorf("%w: %s", ErrCodexAuthMissingField, field)
		}
	}
	return auth, nil
}

// CodexAccountStoreJSON renders a validated CodexAuth into the canonical
// account-store credential shape: a top-level object with "access", "refresh"
// and "id_token" keys. This is the shape the daemon validates when a codex
// credential is stored (services/bossd/internal/server/account.go) and the one
// the credential materializer treats as the account-store form; the raw codex
// auth.json (nested under "tokens" with "*_token" keys) must be converted to it
// before storing so an interactive registration verifies instead of being kept
// unverified. account_id is intentionally omitted: it is not part of the
// account-store shape and is backfilled during materialization.
func CodexAccountStoreJSON(auth CodexAuth) ([]byte, error) {
	return json.Marshal(map[string]string{
		"access":   auth.AccessToken,
		"refresh":  auth.RefreshToken,
		"id_token": auth.IDToken,
	})
}

// EmailFromIDToken best-effort decodes the JWT payload of an OpenAI id_token
// and returns its email claim. The signature is NOT verified — the result is
// a display default only, never an authentication input.
func EmailFromIDToken(idToken string) (string, bool) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Email == "" {
		return "", false
	}
	return claims.Email, true
}
