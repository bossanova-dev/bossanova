// Package proofenv defines the allowlisted environment overlay that lets the
// agentic proof pipeline (scripts/proof.mjs) run inside managed sessions —
// including unattended cron worktrees, where nothing else injects these values.
//
// This package is deliberately keyring-free: it holds only the allowlisted env
// variable NAMES and the hermetic no-op resolver (StaticResolver / NewNoop).
// The keyring-backed resolver that reads the real secrets lives in the sibling
// package proofenvkeyring, which only the daemon binary (cmd/bossd) imports.
// Keeping the OS-keyring dependency (and its transitive github.com/godbus/dbus
// connection goroutine) out of this package is what lets the plugin and session
// packages default to a hermetic resolver without their test binaries linking
// dbus — so goleak's TestStopNoGoroutineLeak cannot trip on a SecretService
// connection goroutine on Linux.
//
// The overlay is a FIXED five-key allowlist: two secrets (resolved from the
// shared keyring by proofenvkeyring) and three non-secret daemon constants.
package proofenv

// Allowlisted environment variable names injected into proof-capable agent
// spawns. These names are what scripts/proof.mjs (and wrangler, for the
// Cloudflare pair) read from the process environment.
const (
	// EnvAnthropicAPIKey is the Anthropic key the proof agent loop uses.
	EnvAnthropicAPIKey = "PROOF_ANTHROPIC_API_KEY"
	// EnvCloudflareAPIToken is the proof-bucket-scoped, write-only R2 upload
	// token (D11). Never the account-wide token.
	EnvCloudflareAPIToken = "CLOUDFLARE_API_TOKEN"
	// EnvR2Bucket is the R2 bucket proof media is uploaded to.
	EnvR2Bucket = "BOSS_PROOF_R2_BUCKET"
	// EnvPublicBaseURL is the public base URL proof links resolve under.
	EnvPublicBaseURL = "BOSS_PROOF_PUBLIC_BASE_URL"
	// EnvCloudflareAccountID is the Cloudflare account that owns the proof R2
	// bucket. Not a credential.
	EnvCloudflareAccountID = "CLOUDFLARE_ACCOUNT_ID"
)
