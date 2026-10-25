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
// Resolve supplies only the spawn Anthropic key; UploadTarget supplies the
// daemon-only upload credential and destination. Failures are logged by name only.
package proofenvkeyring

import (
	"github.com/99designs/keyring"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/daemon/keyringutil"

	"github.com/recurser/bossd/internal/adapter/proofenv"
	"github.com/recurser/bossd/internal/domain"
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
// shared "bossanova" keyring.
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

// openSharedKeyring forbids an insecure fallback: a broken keyring should
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

// Resolve returns only the spawn-permitted proof Anthropic credential, read fresh.
func (r *Resolver) Resolve() map[string]string {
	env := make(map[string]string)
	ring, err := r.open()
	if err != nil {
		r.logger.Warn().Str("missing", proofenv.EnvAnthropicAPIKey).Msg("proofenvkeyring: keyring unavailable")
		return env
	}
	r.readSecret(ring, env, proofenv.EnvAnthropicAPIKey, ItemAnthropicAPIKey)
	return env
}

// UploadTarget reads the daemon upload credential fresh, never from ambient env.
func (r *Resolver) UploadTarget() (domain.EvidenceUploadTarget, bool) {
	ring, err := r.open()
	if err != nil {
		r.logger.Warn().Str("missing", ItemCloudflareAPIToken).Msg("proofenvkeyring: keyring unavailable")
		return domain.EvidenceUploadTarget{}, false
	}
	env := make(map[string]string)
	r.readSecret(ring, env, proofenv.EnvCloudflareAPIToken, ItemCloudflareAPIToken)
	token := env[proofenv.EnvCloudflareAPIToken]
	if token == "" {
		return domain.EvidenceUploadTarget{}, false
	}
	return domain.EvidenceUploadTarget{AccountID: constCloudflareAccountID, Bucket: constR2Bucket, PublicBaseURL: constPublicBaseURL, Token: token}, true
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
