package proofenv

import (
	"strings"

	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/domain/repotrust"
)

// OverlayFor is the proof overlay a managed agent spawn (session or repair
// run) of repo receives: r.Resolve() for a trusted repo, nil for any other
// (BOS-1562) and when no resolver is wired. It is the grant half of the trust
// gate — the only way a spawn obtains the overlay. The pin half, which keeps an
// ambient proof key out of an untrusted spawn, lives in dotenv.OverlayWithRepo.
// Values are secrets and must never be logged.
func OverlayFor(r Resolver, repo *models.Repo) map[string]string {
	if !repotrust.Allowed(repo) || r == nil {
		return nil
	}
	return r.Resolve()
}

// GateEnvFor is GateEnv gated on repotrust.Allowed: a trusted repo's gate gets
// exactly GateEnv(r); any other gate gets no proof key, and unset names every
// allowlisted proof key (Keys) — including PROOF_ANTHROPIC_API_KEY — so the
// daemon's own environment cannot supply one.
func GateEnvFor(r Resolver, repo *models.Repo) (anthropicKey string, unset []string) {
	if repotrust.Allowed(repo) {
		return GateEnv(r)
	}
	return "", Keys()
}

// Resolver resolves the allowlisted proof overlay. proofenvkeyring.Resolver
// backs it in production; StaticResolver backs it in tests.
type Resolver interface {
	Resolve() map[string]string
}

// GateEnv composes the proof-derived part of a pre-session gate's environment,
// shared by the cron gate and the trigger gate so the two cannot drift. A gate
// receives ONLY the proof Anthropic key (as PROOF_ANTHROPIC_API_KEY, empty when
// r is nil or resolves none) and must not inherit the proof upload variables,
// so unset always names the Cloudflare/R2 four. When a proof key is injected,
// unset also drops the ambient ANTHROPIC_API_KEY and ANTHROPIC_BASE_URL, so the
// gate cannot mix the proof key with a different endpoint or credential.
func GateEnv(r Resolver) (anthropicKey string, unset []string) {
	if r != nil {
		if overlay := r.Resolve(); overlay != nil {
			anthropicKey = strings.TrimSpace(overlay[EnvAnthropicAPIKey])
		}
	}
	unset = []string{
		EnvCloudflareAPIToken,
		EnvR2Bucket,
		EnvPublicBaseURL,
		EnvCloudflareAccountID,
	}
	if anthropicKey != "" {
		unset = append(unset, "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL")
	}
	return anthropicKey, unset
}
