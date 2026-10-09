package credmaterialize

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/agentcred"
)

const synthChatGPTAccount = "synthetic-chatgpt-account"

// synthIDToken is a synthetic id_token carrying the ChatGPT account claim codex
// sends as its account header. A "" account omits the claim entirely.
func synthIDToken(t *testing.T, account string) string {
	t.Helper()
	claims := map[string]any{"email": "someone@example.com"}
	if account != "" {
		claims[openAIAuthClaim] = map[string]any{"chatgpt_account_id": account}
	}
	return synthToken(t, claims)
}

// synthAccessToken is a synthetic access token issued at iat with a ten-day
// lifetime, the shape measured from a real codex login.
func synthAccessToken(t *testing.T, iat time.Time) string {
	t.Helper()
	return synthToken(t, map[string]any{
		"iat": iat.Unix(),
		"exp": iat.Add(10 * 24 * time.Hour).Unix(),
	})
}

// codexWrittenAuth is an auth.json in the shape codex 0.159 itself writes after
// a ChatGPT device login.
func codexWrittenAuth(t *testing.T, access, id, refresh, account string, lastRefresh time.Time) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      id,
			"access_token":  access,
			"refresh_token": refresh,
			"account_id":    account,
		},
		"last_refresh": lastRefresh.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("marshal codex-written auth: %v", err)
	}
	return raw
}

// accountStoreBlob converts a codex-written auth.json exactly as `boss account
// add|reauth codex` does before handing it to the daemon.
func accountStoreBlob(t *testing.T, codexWritten []byte) []byte {
	t.Helper()
	auth, err := agentcred.ValidateCodexAuthJSON(codexWritten)
	if err != nil {
		t.Fatalf("ValidateCodexAuthJSON: %v", err)
	}
	stored, err := agentcred.CodexAccountStoreJSON(auth)
	if err != nil {
		t.Fatalf("CodexAccountStoreJSON: %v", err)
	}
	return stored
}

type materializedCodexAuth struct {
	AuthMode    string      `json:"auth_mode"`
	LastRefresh string      `json:"last_refresh"`
	Tokens      tokenFields `json:"tokens"`
}

func decodeMaterialized(t *testing.T, blob []byte) materializedCodexAuth {
	t.Helper()
	var got materializedCodexAuth
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("materialized auth.json is not JSON: %v", err)
	}
	return got
}

// TestCodexAuthForWriteRestoresWhatCodexWrote is the contract that broke codex
// reauthentication: a credential captured from a real codex login goes through
// the account-store shape (which drops account_id, auth_mode and last_refresh),
// and the auth.json materialized from it must carry them again. Without
// tokens.account_id codex 0.159 answers its first 401 with "you have since
// logged out or signed in to another account" and the credential never verifies.
func TestCodexAuthForWriteRestoresWhatCodexWrote(t *testing.T) {
	issued := time.Date(2026, 10, 5, 15, 29, 10, 0, time.UTC)
	access := synthAccessToken(t, issued)
	id := synthIDToken(t, synthChatGPTAccount)
	written := codexWrittenAuth(t, access, id, "synthetic-refresh", synthChatGPTAccount, issued)

	got := decodeMaterialized(t, codexAuthForWrite(accountStoreBlob(t, written)))
	want := decodeMaterialized(t, written)

	if got.Tokens != want.Tokens {
		t.Fatal("materialized tokens differ from the tokens codex wrote (account_id dropped?)")
	}
	if got.AuthMode != want.AuthMode {
		t.Fatalf("auth_mode = %q, want %q", got.AuthMode, want.AuthMode)
	}
	stamp, ok := authGeneration([]byte(`{"last_refresh":` + mustJSON(t, got.LastRefresh) + `}`))
	if !ok || !stamp.Equal(issued) {
		t.Fatalf("last_refresh = %q, want the access token's iat %s", got.LastRefresh, issued.Format(time.RFC3339))
	}
}

func TestCodexAuthForWriteLeavesCompleteCodexAuthByteIdentical(t *testing.T) {
	issued := time.Date(2026, 10, 5, 15, 29, 10, 0, time.UTC)
	written := codexWrittenAuth(t, synthAccessToken(t, issued), synthIDToken(t, synthChatGPTAccount),
		"synthetic-refresh", "account-codex-chose", issued.Add(time.Hour))

	if got := codexAuthForWrite(written); !bytes.Equal(got, written) {
		t.Fatal("a complete codex-written auth.json was rewritten; present values must win")
	}
}

func TestCodexAuthForWriteWithoutAccountClaimAddsNoAccountID(t *testing.T) {
	blob := []byte(`{"access":"a","refresh":"r","id_token":` + mustJSON(t, synthIDToken(t, "")) + `}`)

	got := decodeMaterialized(t, codexAuthForWrite(blob))
	if got.Tokens.AccountID != "" {
		t.Fatalf("account_id = %q, want none when the id_token carries no claim", got.Tokens.AccountID)
	}
	if got.AuthMode != codexChatGPTAuthMode {
		t.Fatalf("auth_mode = %q, want %q", got.AuthMode, codexChatGPTAuthMode)
	}
}

// TestMaterializeCodexKeepsOperatorReauthOverRotatedFile pins the ordering an
// operator reauth depends on. The reauth stores the account-store shape, which
// carries no last_refresh; before the materialized form was stamped from the
// access token's iat, that left the pair unordered and the stale, codex-rotated
// auth.json was folded back over the credential the operator had just captured.
func TestMaterializeCodexKeepsOperatorReauthOverRotatedFile(t *testing.T) {
	first := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{blob: codexWrittenAuth(t, synthAccessToken(t, first), synthIDToken(t, synthChatGPTAccount),
		"refresh-original", synthChatGPTAccount, first)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	// A session's codex rotates auth.json; that chain later dies.
	rotated := first.Add(24 * time.Hour)
	if err := os.WriteFile(authPath, codexWrittenAuth(t, synthAccessToken(t, rotated),
		synthIDToken(t, synthChatGPTAccount), "refresh-rotated-dead", synthChatGPTAccount, rotated), 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}

	// The operator then reauthenticates: a fresh login, stored in account-store shape.
	reauth := rotated.Add(24 * time.Hour)
	store.setBlob(accountStoreBlob(t, codexWrittenAuth(t, synthAccessToken(t, reauth),
		synthIDToken(t, synthChatGPTAccount), "refresh-reauth", synthChatGPTAccount, reauth)))
	savesBefore := store.saveCount()

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (after reauth): %v", err)
	}

	if got := store.saveCount(); got != savesBefore {
		t.Fatalf("SaveCredential calls = %d, want %d: the reauth credential must not be merged over", got, savesBefore)
	}
	onDisk, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	got := decodeMaterialized(t, onDisk)
	if got.Tokens.RefreshToken != "refresh-reauth" {
		t.Fatal("auth.json does not hold the reauthenticated refresh token")
	}
	if got.Tokens.AccountID != synthChatGPTAccount {
		t.Fatalf("auth.json account_id = %q, want %q", got.Tokens.AccountID, synthChatGPTAccount)
	}
}

func TestMaterializeCodexWarnsWhenNoAccountIDCanBeDerived(t *testing.T) {
	blob := []byte(`{"access":"a","refresh":"r","id_token":` + mustJSON(t, synthIDToken(t, "")) + `}`)
	var logs bytes.Buffer
	m := newTestMaterializerWithLogger(t, &fakeStore{blob: blob}, t.TempDir(), &logs)

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	if !strings.Contains(logs.String(), "no tokens.account_id") {
		t.Fatalf("expected a warning naming the missing account_id; logs:\n%s", logs.String())
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
