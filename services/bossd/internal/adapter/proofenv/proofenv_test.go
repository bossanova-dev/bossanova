package proofenv

import (
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/models"
)

// TestNewNoop_HermeticEmptyOverlay documents the contract bossd tests rely on:
// the injectable no-op resolver never opens a keyring and resolves to an empty
// overlay. (The "never opens a keyring" property is what keeps spawn-path unit
// tests hermetic; a StaticResolver has no keyring accessor at all, so it cannot
// leak the godbus goroutine goleak flags on Linux.)
func TestNewNoop_HermeticEmptyOverlay(t *testing.T) {
	if got := NewNoop().Resolve(); len(got) != 0 {
		t.Errorf("NewNoop().Resolve() = %v, want empty overlay", got)
	}
}

// TestStaticResolver_ReturnsFixedOverlay confirms StaticResolver echoes its
// configured overlay verbatim without touching a keyring.
func TestStaticResolver_ReturnsFixedOverlay(t *testing.T) {
	want := map[string]string{EnvR2Bucket: "b", EnvAnthropicAPIKey: "k"}
	got := StaticResolver{Env: want}.Resolve()
	if len(got) != len(want) || got[EnvR2Bucket] != "b" || got[EnvAnthropicAPIKey] != "k" {
		t.Errorf("StaticResolver.Resolve() = %v, want %v", got, want)
	}
}

// TestGateEnv pins the gate environment both pre-session gates share: the
// Cloudflare/R2 upload variables are always withheld, and the ambient Anthropic
// pair is withheld only when a (trimmed, non-empty) proof key is injected.
func TestGateEnv(t *testing.T) {
	upload := []string{EnvCloudflareAPIToken, EnvR2Bucket, EnvPublicBaseURL, EnvCloudflareAccountID}
	withAnthropic := append(append([]string{}, upload...), "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL")
	for _, tc := range []struct {
		name      string
		resolver  Resolver
		wantKey   string
		wantUnset []string
	}{
		{name: "nil resolver", resolver: nil, wantUnset: upload},
		{name: "nil overlay", resolver: StaticResolver{}, wantUnset: upload},
		{name: "blank key", resolver: StaticResolver{Env: map[string]string{EnvAnthropicAPIKey: " \t\n"}}, wantUnset: upload},
		{
			name: "key injected",
			resolver: StaticResolver{Env: map[string]string{
				EnvAnthropicAPIKey: " proof-key ", EnvCloudflareAPIToken: "upload", EnvR2Bucket: "bucket",
			}},
			wantKey:   "proof-key",
			wantUnset: withAnthropic,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, unset := GateEnv(tc.resolver)
			if key != tc.wantKey {
				t.Fatalf("key = %q, want %q", key, tc.wantKey)
			}
			if strings.Join(unset, ",") != strings.Join(tc.wantUnset, ",") {
				t.Fatalf("unset = %v, want %v", unset, tc.wantUnset)
			}
		})
	}
}

// TestGateEnvFor pins that a gate for an untrusted repo gets no proof key and
// additionally drops any ambient PROOF_ANTHROPIC_API_KEY, while a trusted
// repo's gate gets exactly what GateEnv gives it.
func TestGateEnvFor(t *testing.T) {
	r := StaticResolver{Env: map[string]string{EnvAnthropicAPIKey: "proof-key"}}

	key, unset := GateEnvFor(r, &models.Repo{ID: "r"})
	if key != "" {
		t.Errorf("untrusted gate key = %q, want empty", key)
	}
	if !slices.Contains(unset, EnvAnthropicAPIKey) {
		t.Errorf("untrusted gate unset = %v, want it to drop %s", unset, EnvAnthropicAPIKey)
	}
	for _, k := range []string{EnvCloudflareAPIToken, EnvR2Bucket, EnvPublicBaseURL, EnvCloudflareAccountID} {
		if !slices.Contains(unset, k) {
			t.Errorf("untrusted gate unset = %v, missing %s", unset, k)
		}
	}
	if key, _ := GateEnvFor(r, nil); key != "" {
		t.Errorf("nil-repo gate key = %q, want empty", key)
	}

	wantKey, wantUnset := GateEnv(r)
	key, unset = GateEnvFor(r, &models.Repo{ID: "r", IsTrusted: true})
	if key != wantKey || !slices.Equal(unset, wantUnset) {
		t.Errorf("trusted gate = (%q, %v), want GateEnv's (%q, %v)", key, unset, wantKey, wantUnset)
	}
}

// TestOverlayFor pins the spawn-side grant (BOS-1562) shared by the session
// lifecycle and the repair run: only a trusted repo receives the resolved
// overlay; an untrusted repo, a failed lookup (nil) and an unwired resolver
// receive nothing.
func TestOverlayFor(t *testing.T) {
	r := StaticResolver{Env: map[string]string{EnvAnthropicAPIKey: "proof-key"}}
	if got := OverlayFor(r, &models.Repo{IsTrusted: true}); got[EnvAnthropicAPIKey] != "proof-key" {
		t.Errorf("trusted overlay = %v, want the resolved overlay", got)
	}
	for name, repo := range map[string]*models.Repo{"untrusted": {}, "unresolved": nil} {
		if got := OverlayFor(r, repo); got != nil {
			t.Errorf("%s overlay = %v, want nil", name, got)
		}
	}
	if got := OverlayFor(nil, &models.Repo{IsTrusted: true}); got != nil {
		t.Errorf("unwired-resolver overlay = %v, want nil", got)
	}
}
