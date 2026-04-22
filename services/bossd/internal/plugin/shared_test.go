package plugin

import (
	"testing"

	sharedplugin "github.com/recurser/bossalib/plugin"
)

// TestGeneratePluginCookieUnique asserts that two consecutive calls produce
// distinct cookies — the whole point of the per-startup cookie is that the
// value is not static across daemon runs.
func TestGeneratePluginCookieUnique(t *testing.T) {
	a, err := generatePluginCookie()
	if err != nil {
		t.Fatalf("generatePluginCookie: %v", err)
	}
	b, err := generatePluginCookie()
	if err != nil {
		t.Fatalf("generatePluginCookie: %v", err)
	}
	if a == b {
		t.Fatalf("expected distinct cookies, got %q twice", a)
	}
	// 32 random bytes → 64 hex chars.
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("unexpected cookie length: len(a)=%d len(b)=%d", len(a), len(b))
	}
}

// TestNewHandshakeWiresCookie asserts that NewHandshake threads the supplied
// cookie into MagicCookieValue while keeping the shared key/version.
func TestNewHandshakeWiresCookie(t *testing.T) {
	h := NewHandshake("cookie-abc")
	if h.MagicCookieValue != "cookie-abc" {
		t.Errorf("MagicCookieValue = %q, want %q", h.MagicCookieValue, "cookie-abc")
	}
	if h.MagicCookieKey != sharedplugin.MagicCookieKey {
		t.Errorf("MagicCookieKey = %q, want %q", h.MagicCookieKey, sharedplugin.MagicCookieKey)
	}
	if h.ProtocolVersion != uint(sharedplugin.ProtocolVersion) {
		t.Errorf("ProtocolVersion = %d, want %d", h.ProtocolVersion, sharedplugin.ProtocolVersion)
	}
}

// TestNewVersionedPluginMapHasAllTypes guards against the plugin set
// accidentally dropping a type when future versions are added.
func TestNewVersionedPluginMapHasAllTypes(t *testing.T) {
	m := NewVersionedPluginMap(nil)
	v1, ok := m[sharedplugin.ProtocolVersion]
	if !ok {
		t.Fatalf("expected entry for ProtocolVersion=%d", sharedplugin.ProtocolVersion)
	}
	for _, typ := range []string{
		sharedplugin.PluginTypeTaskSource,
		sharedplugin.PluginTypeEventSource,
		sharedplugin.PluginTypeScheduler,
		sharedplugin.PluginTypeWorkflow,
	} {
		if _, ok := v1[typ]; !ok {
			t.Errorf("plugin type %q missing from v%d set", typ, sharedplugin.ProtocolVersion)
		}
	}
}
