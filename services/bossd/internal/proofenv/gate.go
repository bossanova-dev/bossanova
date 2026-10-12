package proofenv

import "strings"

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
