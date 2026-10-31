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
// daemon-only upload credential and the destination read from settings.json
// proof_upload. Failures are logged by name only.
package proofenvkeyring

import (
	"net/url"

	"github.com/99designs/keyring"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/daemon/keyringutil"
	"github.com/recurser/bossalib/kit/config"

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

// secretStore is the minimal read surface of a keyring the resolver needs.
// keyring.Keyring satisfies it; unit tests inject a fake with no real keyring.
type secretStore interface {
	Get(key string) (keyring.Item, error)
}

// Resolver reads the proof env overlay. The keyring and settings accessors are
// func fields so tests inject fakes; the defaults open the shared "bossanova"
// keyring and load settings.json.
type Resolver struct {
	open     func() (secretStore, error)
	settings func() (config.ProofUploadConfig, error)
	logger   zerolog.Logger
}

// New returns a Resolver backed by the real shared "bossanova" keyring and the
// daemon's settings.json.
func New(logger zerolog.Logger) *Resolver {
	return &Resolver{
		open:     openSharedKeyring,
		settings: loadProofUploadSettings,
		logger:   logger,
	}
}

func loadProofUploadSettings() (config.ProofUploadConfig, error) {
	s, err := config.Load()
	return s.ProofUpload, err
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

// UploadTarget reads the daemon upload credential and destination fresh on
// every call, never from ambient env or a cached copy. An incomplete
// destination reports not configured and is logged by key name only.
func (r *Resolver) UploadTarget() (domain.EvidenceUploadTarget, bool) {
	dest, ok := r.destination()
	if !ok {
		return domain.EvidenceUploadTarget{}, false
	}
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
	return domain.EvidenceUploadTarget{
		AccountID:     dest.AccountID,
		Bucket:        dest.Bucket,
		PublicBaseURL: dest.PublicBaseURL,
		PagesEnabled:  dest.PagesEnabled,
		Token:         token,
	}, true
}

// destination loads settings.json proof_upload and reports whether it names a
// complete destination. It logs only the names of missing or invalid keys.
func (r *Resolver) destination() (config.ProofUploadConfig, bool) {
	if r.settings == nil {
		r.logger.Warn().Msg("proofenvkeyring: proof_upload settings unavailable; upload not configured")
		return config.ProofUploadConfig{}, false
	}
	dest, err := r.settings()
	if err != nil {
		r.logger.Warn().Msg("proofenvkeyring: settings could not be loaded; upload not configured")
		return config.ProofUploadConfig{}, false
	}
	var missing []string
	if dest.AccountID == "" {
		missing = append(missing, "proof_upload.account_id")
	}
	if dest.Bucket == "" {
		missing = append(missing, "proof_upload.bucket")
	}
	if !isHTTPSBaseURL(dest.PublicBaseURL) {
		missing = append(missing, "proof_upload.public_base_url")
	}
	if len(missing) > 0 {
		r.logger.Warn().Strs("missing_or_invalid", missing).Msg("proofenvkeyring: proof upload destination incomplete; upload not configured")
		return config.ProofUploadConfig{}, false
	}
	return dest, true
}

// isHTTPSBaseURL accepts only an absolute https:// URL with a host and no
// credentials, query or fragment, so composed object URLs stay well-formed.
func isHTTPSBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
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
