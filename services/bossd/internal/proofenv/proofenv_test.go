package proofenv

import (
	"strings"
	"testing"
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
