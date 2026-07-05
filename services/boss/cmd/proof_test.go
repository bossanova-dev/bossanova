package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/spf13/cobra"
)

// newProofTestCmd returns a bare command with buffered stdin/stdout so the
// runProof* helpers can be exercised without a real keyring or TTY.
func newProofTestCmd(stdin string) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader(stdin))
	return cmd, out
}

func TestProofSetSecret_StoresFromStdin(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	cmd, out := newProofTestCmd("  cf-secret-token\n")

	if err := runProofSetSecret(cmd, ring, []string{proofItemCloudflareAPIToken}); err != nil {
		t.Fatalf("runProofSetSecret: %v", err)
	}
	item, err := ring.Get(proofItemCloudflareAPIToken)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Surrounding whitespace/newline is trimmed before storing.
	if string(item.Data) != "cf-secret-token" {
		t.Errorf("stored %q, want trimmed value", item.Data)
	}
	// The command output must confirm the key but never echo the value.
	if strings.Contains(out.String(), "cf-secret-token") {
		t.Errorf("secret value leaked into stdout: %q", out.String())
	}
	if !strings.Contains(out.String(), proofItemCloudflareAPIToken) {
		t.Errorf("confirmation should name the key: %q", out.String())
	}
}

func TestProofSetSecret_RejectsEmpty(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	cmd, _ := newProofTestCmd("   \n")
	if err := runProofSetSecret(cmd, ring, []string{proofItemAnthropicAPIKey}); err == nil {
		t.Fatal("expected error for empty stdin value")
	}
}

func TestProofSetSecret_RejectsUnknownKey(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	cmd, _ := newProofTestCmd("value")
	if err := runProofSetSecret(cmd, ring, []string{"not-a-proof-key"}); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestProofSetSecret_RequiresExactlyOneKey(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	cmd, _ := newProofTestCmd("value")
	if err := runProofSetSecret(cmd, ring, nil); err == nil {
		t.Fatal("expected error when no key given")
	}
}

func TestProofCheck_ReportsSetAndMissingWithoutValues(t *testing.T) {
	ring := keyring.NewArrayKeyring([]keyring.Item{
		{Key: proofItemAnthropicAPIKey, Data: []byte("super-secret-anthropic")},
	})
	cmd, out := newProofTestCmd("")
	if err := runProofCheck(cmd, ring); err != nil {
		t.Fatalf("runProofCheck: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, proofItemAnthropicAPIKey+": set") {
		t.Errorf("expected anthropic reported set: %q", got)
	}
	if !strings.Contains(got, proofItemCloudflareAPIToken+": MISSING") {
		t.Errorf("expected cloudflare reported MISSING: %q", got)
	}
	// --check must never print a stored value.
	if strings.Contains(got, "super-secret-anthropic") {
		t.Errorf("check leaked a secret value: %q", got)
	}
}
