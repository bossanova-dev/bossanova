// Package proofenvkeyring holds the keyring-backed proof env resolver — the
// half of the proof overlay that must open the shared "bossanova" OS keyring to
// read the two secret values. It is deliberately split out from package
// proofenv so that only the daemon binary (cmd/bossd) links the keyring (and
// its transitive github.com/godbus/dbus dependency). The plugin and session
// packages depend only on the keyring-free proofenv package for the resolver
// interface and the hermetic no-op default, so their test binaries never link
// dbus and cannot leak the SecretService connection goroutine that goleak
// flags on Linux.
//
// The overlay is a FIXED five-key allowlist: two secrets read from the shared
// keyring (mirroring services/bossd/internal/upstream tokens.go's open pattern)
// and three non-secret daemon constants. A key whose lookup fails is omitted —
// partial injection is fine; the proof doctor reports the gap. Secret VALUES
// are never logged; resolution failures are logged by key NAME only.
package proofenvkeyring

import (
	"github.com/99designs/keyring"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/keyringutil"

	"github.com/recurser/bossd/internal/proofenv"
)

// Keyring item keys the two secrets are stored under in the shared "bossanova"
// keyring service. The boss CLI `proof set-secret` command writes these same
// keys (it lives in another module and cannot import this internal package, so
// it duplicates the constants — keep in sync).
const (
	ItemAnthropicAPIKey    = "proof-anthropic-api-key"
	ItemCloudflareAPIToken = "proof-cloudflare-api-token"
)

// Non-secret daemon-side constants. Account IDs are not credentials and this
// one is already published in committed plan docs; keeping it as a constant
// (not a keyring item) keeps provisioning to just the two real secrets.
const (
	constR2Bucket            = "bossanova-proof-production"
	constPublicBaseURL       = "https://proof.bossanova.dev"
	constCloudflareAccountID = "4457a89ee2fd7a6814606d156f8129ed"
)

// secretStore is the minimal read surface of a keyring the resolver needs.
// keyring.Keyring satisfies it; unit tests inject a fake with no real keyring.
type secretStore interface {
	Get(key string) (keyring.Item, error)
}

// Resolver reads the proof env overlay. The keyring accessor is a func field so
// tests inject a fake store without a real keyring; the default opens the
// shared "bossanova" keyring exactly as upstream/tokens.go does.
type Resolver struct {
	open   func() (secretStore, error)
	logger zerolog.Logger
}

// New returns a Resolver backed by the real shared "bossanova" keyring.
func New(logger zerolog.Logger) *Resolver {
	return &Resolver{
		open:   openSharedKeyring,
		logger: logger,
	}
}

// openSharedKeyring opens the shared bossanova keyring with the same config as
// services/bossd/internal/upstream/tokens.go. allowInsecure is hard-wired
// false: bossd is a daemon with no flag plumbing, so a broken keyring should
// surface a real error rather than silently reverting to a hardcoded
// passphrase.
func openSharedKeyring() (secretStore, error) {
	return keyring.Open(keyring.Config{
		ServiceName:              "bossanova",
		KeychainTrustApplication: true,
		FileDir:                  "~/.config/bossanova/keyring",
		FilePasswordFunc:         keyring.PromptFunc(keyringutil.New(false)),
		AllowedBackends:          keyringutil.Backends(),
	})
}

// Resolve returns the allowlisted proof env overlay. It resolves fresh on every
// call (no long-lived cache) so a re-provisioned secret is picked up without a
// daemon restart — keyring reads are cheap. Any key whose lookup fails is
// omitted; the three non-secret constants are always present. Secret values are
// never logged.
func (r *Resolver) Resolve() map[string]string {
	env := map[string]string{
		proofenv.EnvR2Bucket:            constR2Bucket,
		proofenv.EnvPublicBaseURL:       constPublicBaseURL,
		proofenv.EnvCloudflareAccountID: constCloudflareAccountID,
	}

	ring, err := r.open()
	if err != nil {
		// Keyring unavailable: omit both secrets, keep the constants. This
		// degrades to an honest "env-unavailable" outcome downstream rather
		// than a failure. Log by name only — never the (absent) values.
		r.logger.Warn().Err(err).
			Strs("missing", []string{proofenv.EnvAnthropicAPIKey, proofenv.EnvCloudflareAPIToken}).
			Msg("proofenvkeyring: keyring unavailable; secret keys omitted from proof env overlay")
		return env
	}

	r.readSecret(ring, env, proofenv.EnvAnthropicAPIKey, ItemAnthropicAPIKey)
	r.readSecret(ring, env, proofenv.EnvCloudflareAPIToken, ItemCloudflareAPIToken)
	return env
}

// readSecret reads one keyring item into env under envKey. On failure it omits
// the key and logs by NAME only — the secret value never reaches a log line or
// an error string.
func (r *Resolver) readSecret(ring secretStore, env map[string]string, envKey, itemKey string) {
	item, err := ring.Get(itemKey)
	if err != nil {
		r.logger.Warn().
			Str("env_key", envKey).
			Str("item_key", itemKey).
			Msg("proofenvkeyring: secret not provisioned; omitted from proof env overlay")
		return
	}
	if len(item.Data) == 0 {
		r.logger.Warn().
			Str("env_key", envKey).
			Str("item_key", itemKey).
			Msg("proofenvkeyring: secret is empty; omitted from proof env overlay")
		return
	}
	env[envKey] = string(item.Data)
}
