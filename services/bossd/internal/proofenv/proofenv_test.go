package proofenv

import "testing"

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
