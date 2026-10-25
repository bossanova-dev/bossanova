// Package proofenv defines the allowlisted environment overlay that lets the
// verification evidence publisher (.claude/skills/verify) run inside managed sessions —
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
// Only the proof Anthropic key may enter an agent spawn. Upload credentials
// and destination settings belong exclusively to the daemon.
package proofenv

// Proof environment names. Only SpawnKeys may be forwarded; UploadKeys are
// daemon-only and must be shadowed in spawned processes.
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
	// guard-exception(entity-names): a Cloudflare account, not an agent account; expires 2027-10-20
	EnvCloudflareAccountID = "CLOUDFLARE_ACCOUNT_ID"
)

// Keys returns every allowlisted overlay name. A spawn for a repo that may not
// receive the overlay pins each of them to "" (dotenv.OverlayWithRepo) so the
// daemon's ambient environment cannot supply one either.
func Keys() []string { return append(SpawnKeys(), UploadKeys()...) }

// SpawnKeys lists the proof values permitted in an agent environment.
func SpawnKeys() []string { return []string{EnvAnthropicAPIKey} }

// UploadKeys lists daemon-only values pinned empty in every agent environment.
func UploadKeys() []string {
	return []string{EnvCloudflareAPIToken, EnvR2Bucket, EnvPublicBaseURL, EnvCloudflareAccountID}
}
