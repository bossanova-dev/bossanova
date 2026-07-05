package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/99designs/keyring"
	"github.com/spf13/cobra"

	"github.com/recurser/bossalib/keyringutil"
)

// Proof keyring item keys. These mirror
// services/bossd/internal/proofenv.Item* — bossd reads what this command
// writes. The two packages live in different modules and cannot share a
// constant, so keep them in sync.
const (
	proofItemAnthropicAPIKey    = "proof-anthropic-api-key"
	proofItemCloudflareAPIToken = "proof-cloudflare-api-token"
)

// proofKeyringService is the shared keyring service the daemon reads proof
// secrets from (same as WorkOS tokens).
const proofKeyringService = "bossanova"

// proofSecretKeys is the fixed set of provisionable proof secrets, kept
// sorted for deterministic --check output.
var proofSecretKeys = []string{proofItemAnthropicAPIKey, proofItemCloudflareAPIToken}

func proofCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proof",
		Short: "Provision credentials for the proof pipeline",
		Long: "Provision the secrets the agentic proof pipeline (scripts/proof.mjs) needs.\n" +
			"bossd injects these into managed agent sessions; provisioning is an ops action.",
	}
	cmd.AddCommand(proofSetSecretCmd())
	return cmd
}

func proofSetSecretCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "set-secret [" + proofItemAnthropicAPIKey + "|" + proofItemCloudflareAPIToken + "]",
		Short: "Store a proof secret in the keyring, read from stdin",
		Long: "Store a proof secret in the shared bossanova keyring the daemon reads.\n\n" +
			"The secret value is read from stdin and never echoed, logged, or printed back.\n" +
			"Provisionable keys:\n" +
			"  " + proofItemAnthropicAPIKey + "     Anthropic API key for the proof agent loop\n" +
			"  " + proofItemCloudflareAPIToken + "  Cloudflare R2 upload token\n\n" +
			"SECURITY (required): the Cloudflare token MUST be a proof-bucket-scoped,\n" +
			"write-only token dedicated to proof uploads. NEVER use the account-wide\n" +
			"Cloudflare API token — a leak from a proof worktree must not grant broad access.\n\n" +
			"Examples:\n" +
			"  printf %s \"$TOKEN\" | boss proof set-secret " + proofItemCloudflareAPIToken + "\n" +
			"  boss proof set-secret --check",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ring, err := openProofKeyring(allowInsecureKeyring(cmd))
			if err != nil {
				return err
			}
			if check {
				return runProofCheck(cmd, ring)
			}
			return runProofSetSecret(cmd, ring, args)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "Report which proof secrets are set (never prints values) and exit")
	return cmd
}

// openProofKeyring opens the shared bossanova keyring using the same config
// as the WorkOS token store and the daemon-side resolver.
func openProofKeyring(allowInsecure bool) (keyring.Keyring, error) {
	ring, err := keyring.Open(keyring.Config{
		ServiceName:              proofKeyringService,
		KeychainTrustApplication: true,
		FileDir:                  "~/.config/bossanova/keyring",
		FilePasswordFunc:         keyring.PromptFunc(keyringutil.New(allowInsecure)),
		AllowedBackends:          keyringutil.Backends(),
	})
	if err != nil {
		return nil, fmt.Errorf("open keyring: %w", err)
	}
	return ring, nil
}

func runProofSetSecret(cmd *cobra.Command, ring keyring.Keyring, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("expected exactly one secret key (%s), got %d", strings.Join(proofSecretKeys, " or "), len(args))
	}
	key := args[0]
	if !isProofSecretKey(key) {
		return fmt.Errorf("unknown proof secret %q; expected one of %s", key, strings.Join(proofSecretKeys, ", "))
	}

	// The value is read from stdin (meant to be piped). When stdin is an
	// interactive terminal, io.ReadAll blocks with no output — indistinguishable
	// from a hang — so print a one-line hint telling the operator to paste and
	// press Ctrl-D. A piped/redirected stdin (and the test's buffer reader) is
	// not a char device, so the hint is silent there.
	if f, ok := cmd.InOrStdin().(*os.File); ok {
		if info, statErr := f.Stat(); statErr == nil && info.Mode()&os.ModeCharDevice != 0 {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Reading %q from stdin — paste the value and press Ctrl-D:\n", key)
		}
	}

	raw, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return fmt.Errorf("read secret from stdin: %w", err)
	}
	// Trim a single trailing newline / surrounding whitespace so a piped
	// `printf`/`echo` doesn't store a stray newline in the token.
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return fmt.Errorf("refusing to store empty value for %q (read nothing on stdin)", key)
	}

	if err := ring.Set(keyring.Item{
		Key:         key,
		Data:        []byte(value),
		Label:       "Bossanova",
		Description: "Bossanova proof pipeline secret",
	}); err != nil {
		return fmt.Errorf("store %q: %w", key, err)
	}
	// Never print the value — only confirm the key was written.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "stored proof secret %q\n", key)
	return nil
}

func runProofCheck(cmd *cobra.Command, ring keyring.Keyring) error {
	out := cmd.OutOrStdout()
	keys := append([]string(nil), proofSecretKeys...)
	sort.Strings(keys)
	for _, key := range keys {
		status := "MISSING"
		if item, err := ring.Get(key); err == nil && len(item.Data) > 0 {
			status = "set"
		}
		_, _ = fmt.Fprintf(out, "%s: %s\n", key, status)
	}
	return nil
}

func isProofSecretKey(key string) bool {
	for _, k := range proofSecretKeys {
		if k == key {
			return true
		}
	}
	return false
}
