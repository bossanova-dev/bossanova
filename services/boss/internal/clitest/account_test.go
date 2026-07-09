package clitest_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testAccounts() []*pb.Account {
	return []*pb.Account{
		{
			Id:            "acct-aaa",
			Provider:      "claude",
			Label:         "primary",
			Status:        "active",
			Priority:      1,
			Health:        "ok",
			Tier:          "max",
			AllowedModels: []string{"opus", "sonnet"},
			CooldownUntil: timestamppb.New(timestampDaysAgo(-1)),
			LastUsedAt:    timestamppb.New(timestampDaysAgo(1)),
			CreatedAt:     timestamppb.New(timestampDaysAgo(10)),
			UpdatedAt:     timestamppb.New(timestampDaysAgo(1)),
		},
		{
			Id:       "acct-bbb",
			Provider: "codex",
			Label:    "secondary",
			Status:   "disabled",
			Priority: 5,
			Health:   "failed",
		},
	}
}

// accountJSONSchema mirrors the stable schema emitted by `boss account ls/test
// --json`. It deliberately has NO credential field — the test asserts the blob
// never appears in output.
type accountJSONSchema struct {
	ID            string   `json:"id"`
	Provider      string   `json:"provider"`
	Label         string   `json:"label"`
	Status        string   `json:"status"`
	Priority      int32    `json:"priority"`
	Health        string   `json:"health"`
	Tier          string   `json:"tier"`
	AllowedModels []string `json:"allowed_models"`
	CooldownUntil string   `json:"cooldown_until"`
	LastUsedAt    string   `json:"last_used_at"`
	LastTestOkAt  string   `json:"last_test_ok_at"`
	LastTestError string   `json:"last_test_error"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

func TestCLI_Account_Ls(t *testing.T) {
	h := clitest.New(t, clitest.WithAccounts(testAccounts()...))
	res := h.Run("account", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"acct-aaa", "primary", "claude", "acct-bbb", "secondary", "codex"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, "seed-credential") {
		t.Errorf("credential blob leaked into ls output:\n%s", res.Stdout)
	}
}

func TestCLI_Account_Ls_FullIDAndAlignedColumns(t *testing.T) {
	accounts := []*pb.Account{
		{
			Id:       "8078890d6b0affc7",
			Provider: "claude",
			Label:    "agent.yuki",
			Status:   "active",
			Health:   "ok",
		},
		{
			Id:       "6aaff35db711eee5",
			Provider: "codex",
			Label:    "dave@kamik.ai",
			Status:   "active",
			Health:   "ok",
		},
	}
	h := clitest.New(t, clitest.WithAccounts(accounts...))
	res := h.Run("account", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header plus 2 account rows, got %d lines:\n%s", len(lines), res.Stdout)
	}
	var yukiRow string
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "8078890d6b0affc7" {
			yukiRow = line
		}
	}
	if yukiRow == "" {
		t.Fatalf("full account id not found; stdout:\n%s", res.Stdout)
	}
	if !strings.Contains(yukiRow, "agent.yuki     active") {
		t.Fatalf("agent.yuki row does not align label before status:\n%s", res.Stdout)
	}
}

func TestCLI_Account_Ls_Empty(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("account", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "No accounts") {
		t.Errorf("expected empty-state message, got %q", res.Stdout)
	}
}

func TestCLI_Account_Ls_ProviderFilter(t *testing.T) {
	h := clitest.New(t, clitest.WithAccounts(testAccounts()...))
	res := h.Run("account", "ls", "--provider", "codex")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "secondary") {
		t.Errorf("stdout should contain codex account, got %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "primary") {
		t.Errorf("stdout should NOT contain claude account, got %q", res.Stdout)
	}
}

func TestCLI_Account_Ls_JSON(t *testing.T) {
	h := clitest.New(t, clitest.WithAccounts(testAccounts()...))
	res := h.Run("account", "ls", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var accounts []accountJSONSchema
	if err := json.Unmarshal([]byte(res.Stdout), &accounts); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
	}
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	var first accountJSONSchema
	for _, a := range accounts {
		if a.ID == "acct-aaa" {
			first = a
		}
	}
	if first.ID != "acct-aaa" {
		t.Fatalf("acct-aaa not present in JSON")
	}
	if first.Provider != "claude" || first.Label != "primary" {
		t.Errorf("unexpected core fields: %+v", first)
	}
	if first.Status != "active" || first.Priority != 1 || first.Health != "ok" || first.Tier != "max" {
		t.Errorf("unexpected status/priority/health/tier: %+v", first)
	}
	if len(first.AllowedModels) != 2 || first.AllowedModels[0] != "opus" {
		t.Errorf("unexpected allowed_models: %+v", first.AllowedModels)
	}
	if first.CooldownUntil == "" || first.LastUsedAt == "" || first.CreatedAt == "" {
		t.Errorf("expected RFC3339 timestamps, got %+v", first)
	}
	// No credential field is possible in the schema; assert the blob text is
	// nowhere in the raw JSON either.
	if strings.Contains(res.Stdout, "seed-credential") {
		t.Errorf("credential blob leaked into --json output:\n%s", res.Stdout)
	}
}

func TestCLI_Account_Add_Token(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("account", "add",
		"--provider", "claude",
		"--label", "my-account",
		"--priority", "3",
		"--token", "secret-token-xyz",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.AddAccountCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 add call, got %d", len(calls))
	}
	req := calls[0]
	if req.Provider != "claude" || req.Label != "my-account" || req.Priority != 3 {
		t.Errorf("unexpected fields: %+v", req)
	}
	if string(req.Credential) != "secret-token-xyz" {
		t.Errorf("expected credential forwarded to daemon, got %q", string(req.Credential))
	}
	// The credential must never be echoed back to the user.
	if strings.Contains(res.Stdout, "secret-token-xyz") || strings.Contains(res.Stderr, "secret-token-xyz") {
		t.Errorf("credential echoed in output: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestCLI_Account_Add_CredentialFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/cred.json"
	body := `{"access":"a","refresh":"r","id_token":"i"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	h := clitest.New(t)
	res := h.Run("account", "add",
		"--provider", "codex", "--label", "cx",
		"--credential-file", path,
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.AddAccountCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 add call, got %d", len(calls))
	}
	if string(calls[0].Credential) != body {
		t.Errorf("expected credential-file body forwarded, got %q", string(calls[0].Credential))
	}
}

func TestCLI_Account_Add_CredentialStdin(t *testing.T) {
	body := "token-from-stdin"
	h := clitest.New(t)
	res := h.RunWithStdin(body, "account", "add",
		"--provider", "claude", "--label", "cx",
		"--credential-file", "-",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.AddAccountCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 add call, got %d", len(calls))
	}
	if string(calls[0].Credential) != body {
		t.Errorf("expected stdin credential forwarded, got %q", string(calls[0].Credential))
	}
}

func TestCLI_Account_Add_MissingRequired(t *testing.T) {
	cases := [][]string{
		{"account", "add", "--label", "l", "--token", "t"},                        // no --provider
		{"account", "add", "--provider", "claude", "--token", "t"},                // no --label
		{"account", "add", "--provider", "claude", "--label", "l"},                // no credential source
		{"account", "add", "--provider", "bogus", "--label", "l", "--token", "t"}, // unknown provider
	}
	for _, args := range cases {
		h := clitest.New(t)
		res := h.Run(args...)
		if res.ExitCode == 0 {
			t.Errorf("expected non-zero exit for %v; stdout=%q", args, res.Stdout)
		}
		if len(h.Daemon.AddAccountCalls()) != 0 {
			t.Errorf("expected no add call for invalid args %v", args)
		}
	}
}

func TestCLI_Account_Add_CredentialMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/c.txt"
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := clitest.New(t)
	res := h.Run("account", "add", "--provider", "claude", "--label", "l",
		"--token", "t", "--credential-file", path)
	if res.ExitCode == 0 {
		t.Errorf("expected error when both --token and --credential-file set")
	}
}

func TestCLI_Account_Add_InteractiveNoTTY(t *testing.T) {
	// `boss account add claude` with no credential flag routes to the
	// interactive flow. The clitest harness has no TTY, so the guard must fail
	// fast with an actionable message and issue zero AddAccount RPCs.
	h := clitest.New(t)
	res := h.Run("account", "add", "claude")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit; stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "interactive") || !strings.Contains(combined, "terminal") {
		t.Errorf("expected interactive/terminal guard message, got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	if n := len(h.Daemon.AddAccountCalls()); n != 0 {
		t.Errorf("expected 0 add calls from the TTY guard, got %d", n)
	}
}

func TestCLI_Account_Add_PositionalToken(t *testing.T) {
	// The positional provider resolves for the non-interactive flag path too.
	h := clitest.New(t)
	res := h.Run("account", "add", "claude",
		"--label", "pos", "--token", "secret-pos-token",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.AddAccountCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 add call, got %d", len(calls))
	}
	if calls[0].Provider != "claude" || calls[0].Label != "pos" {
		t.Errorf("unexpected fields: %+v", calls[0])
	}
	if string(calls[0].Credential) != "secret-pos-token" {
		t.Errorf("expected credential forwarded, got %q", string(calls[0].Credential))
	}
}

func TestCLI_Account_Add_UnknownProvider(t *testing.T) {
	cases := [][]string{
		{"account", "add", "bogus"},
		{"account", "add", "--provider", "opencode"},
	}
	for _, args := range cases {
		h := clitest.New(t)
		res := h.Run(args...)
		if res.ExitCode == 0 {
			t.Errorf("expected non-zero exit for %v; stdout=%q", args, res.Stdout)
		}
		combined := res.Stdout + res.Stderr
		if !strings.Contains(combined, "claude") || !strings.Contains(combined, "codex") {
			t.Errorf("expected supported-provider list for %v, got %q", args, combined)
		}
		if len(h.Daemon.AddAccountCalls()) != 0 {
			t.Errorf("expected no add call for %v", args)
		}
	}
}

func TestCLI_Account_Add_CodexTokenStdinRejected(t *testing.T) {
	// Codex is device-flow only; --token-stdin is a claude-only escape hatch.
	h := clitest.New(t)
	res := h.Run("account", "add", "codex", "--token-stdin")
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit; stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "codex") {
		t.Errorf("expected a codex-specific rejection message, got %q", combined)
	}
	if n := len(h.Daemon.AddAccountCalls()); n != 0 {
		t.Errorf("expected 0 add calls, got %d", n)
	}
}

func TestCLI_Account_Add_ClaudeTokenStdinHeadless(t *testing.T) {
	// `--token-stdin --label` must be fully non-interactive: the token is read
	// from the piped stdin and the label prompt is skipped (a real piped stdin
	// would EOF on any further prompt), so the account is stored.
	tok := "sk-ant-oat01-" + strings.Repeat("a", 40)
	h := clitest.New(t)
	res := h.RunWithStdin(tok+"\n", "account", "add", "claude",
		"--token-stdin", "--label", "hl",
	)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	calls := h.Daemon.AddAccountCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 add call, got %d", len(calls))
	}
	if calls[0].Provider != "claude" || calls[0].Label != "hl" {
		t.Errorf("unexpected fields: %+v", calls[0])
	}
	if string(calls[0].Credential) != tok {
		t.Errorf("expected token forwarded as credential, got %q", string(calls[0].Credential))
	}
}

func TestCLI_Account_Add_RemoteRefused(t *testing.T) {
	// Interactive registration mints credentials via a local subprocess, so it
	// must refuse a remote (--remote) daemon before any dial.
	for _, provider := range []string{"claude", "codex"} {
		h := clitest.New(t)
		res := h.Run("account", "add", provider, "--remote", "https://example.invalid")
		if res.ExitCode == 0 {
			t.Errorf("provider %s: expected non-zero exit for --remote", provider)
		}
		combined := res.Stdout + res.Stderr
		if !strings.Contains(combined, "local") || !strings.Contains(combined, "remote") {
			t.Errorf("provider %s: expected local-daemon-only message, got %q", provider, combined)
		}
		if n := len(h.Daemon.AddAccountCalls()); n != 0 {
			t.Errorf("provider %s: expected 0 add calls, got %d", provider, n)
		}
	}
}

func TestCLI_Account_Update(t *testing.T) {
	h := clitest.New(t, clitest.WithAccounts(testAccounts()...))
	res := h.Run("account", "update", "acct-aaa",
		"--label", "renamed", "--status", "disabled", "--priority", "7",
		"--allowed-models", "opus,haiku",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.UpdateAccountCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(calls))
	}
	req := calls[0]
	if req.Id != "acct-aaa" {
		t.Errorf("expected id acct-aaa, got %q", req.Id)
	}
	if req.Label == nil || *req.Label != "renamed" {
		t.Errorf("expected Label set, got %v", req.Label)
	}
	if req.Status == nil || *req.Status != "disabled" {
		t.Errorf("expected Status set, got %v", req.Status)
	}
	if req.Priority == nil || *req.Priority != 7 {
		t.Errorf("expected Priority set, got %v", req.Priority)
	}
	if len(req.AllowedModels) != 2 || req.AllowedModels[0] != "opus" || req.AllowedModels[1] != "haiku" {
		t.Errorf("expected allowed_models set, got %v", req.AllowedModels)
	}
}

func TestCLI_Account_Update_NoFlags(t *testing.T) {
	h := clitest.New(t, clitest.WithAccounts(testAccounts()...))
	res := h.Run("account", "update", "acct-aaa")
	if res.ExitCode == 0 {
		t.Errorf("expected non-zero exit when no update flags provided")
	}
}

func TestCLI_Account_Remove(t *testing.T) {
	h := clitest.New(t, clitest.WithAccounts(testAccounts()...))
	res := h.Run("account", "remove", "acct-aaa")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if h.Daemon.RemoveAccountCallCount() != 1 {
		t.Errorf("expected 1 remove call, got %d", h.Daemon.RemoveAccountCallCount())
	}
	// The keyring credential is purged along with the metadata row.
	if cred := h.Daemon.AccountCredential("acct-aaa"); cred != nil {
		t.Errorf("expected credential purged after remove, got %q", string(cred))
	}
}

func TestCLI_Account_Test(t *testing.T) {
	h := clitest.New(t, clitest.WithAccounts(testAccounts()...))
	res := h.Run("account", "test", "acct-aaa")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if h.Daemon.TestAccountCallCount() != 1 {
		t.Errorf("expected 1 test call, got %d", h.Daemon.TestAccountCallCount())
	}
	for _, want := range []string{"acct-aaa", "ok", "Provider check ran"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, "seed-credential") {
		t.Errorf("credential leaked into test output:\n%s", res.Stdout)
	}
}

func TestCLI_Account_Test_JSON(t *testing.T) {
	h := clitest.New(t, clitest.WithAccounts(testAccounts()...))
	res := h.Run("account", "test", "acct-aaa", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var out struct {
		Account      accountJSONSchema `json:"account"`
		LiveSmokeRan bool              `json:"live_smoke_ran"`
		Detail       string            `json:"detail"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
	}
	if out.Account.ID != "acct-aaa" {
		t.Errorf("unexpected account: %+v", out.Account)
	}
	if out.LiveSmokeRan {
		t.Errorf("expected live_smoke_ran=false from the mock, got true")
	}
	if strings.Contains(res.Stdout, "seed-credential") {
		t.Errorf("credential leaked into test --json output:\n%s", res.Stdout)
	}
}
