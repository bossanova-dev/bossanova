package plugin

import (
	"net/rpc"
	"testing"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"
)

// fakePlugin is a goplugin.Plugin whose only job is to be identity-comparable
// on the far side of servePlugin.
type fakePlugin struct{ name string }

func (f *fakePlugin) Server(*goplugin.MuxBroker) (interface{}, error) { return nil, nil }

func (f *fakePlugin) Client(*goplugin.MuxBroker, *rpc.Client) (interface{}, error) {
	return nil, nil
}

// TestServePluginArmsBeforeServing is the ordering pin every plugin main now
// depends on. goplugin.Serve never returns, so arming after it would leave the
// watchdog permanently unarmed while every other assertion in the tree still
// passed — which is exactly how the previous source-text guard failed.
func TestServePluginArmsBeforeServing(t *testing.T) {
	var order []string
	stopCalls := 0

	arm := func(zerolog.Logger) func() {
		order = append(order, "arm")
		return func() { stopCalls++ }
	}
	serve := func(*goplugin.ServeConfig) {
		order = append(order, "serve")
	}

	servePlugin(zerolog.Nop(), PluginTypeAgentRunner, &fakePlugin{name: "x"}, arm, serve)

	if len(order) != 2 {
		t.Fatalf("order = %v, want exactly one arm and one serve", order)
	}
	if order[0] != "arm" || order[1] != "serve" {
		t.Fatalf("order = %v, want [arm serve]; a watchdog armed after Serve never arms at all", order)
	}
	if stopCalls != 0 {
		t.Errorf("stop called %d times; the watcher must outlive ServePlugin and die with the process", stopCalls)
	}
}

// TestServePluginServesTheGivenPluginUnderTheNegotiatedProtocol pins the
// ServeConfig the eight mains used to spell out individually. A wrong kind or
// a dropped protocol version is a handshake failure at daemon start, so this
// is the assertion that replaces eight copies of the literal.
func TestServePluginServesTheGivenPluginUnderTheNegotiatedProtocol(t *testing.T) {
	impl := &fakePlugin{name: "the-impl"}

	var got *goplugin.ServeConfig
	servePlugin(
		zerolog.Nop(),
		PluginTypeWorkflow,
		impl,
		func(zerolog.Logger) func() { return func() {} },
		func(cfg *goplugin.ServeConfig) { got = cfg },
	)

	if got == nil {
		t.Fatal("servePlugin did not call serve")
	}
	if got.GRPCServer == nil {
		t.Error("GRPCServer is nil; go-plugin would reject the ServeConfig")
	}
	if want := NewHandshakeForPlugin(); got.HandshakeConfig != want {
		t.Errorf("HandshakeConfig = %+v, want %+v", got.HandshakeConfig, want)
	}
	if got.Plugins != nil {
		t.Errorf("Plugins = %v, want nil; setting it alongside VersionedPlugins makes go-plugin ignore the versioned map", got.Plugins)
	}

	set, ok := got.VersionedPlugins[ProtocolVersion]
	if !ok {
		t.Fatalf("VersionedPlugins has no entry for protocol %d, only %v", ProtocolVersion, got.VersionedPlugins)
	}
	if len(set) != 1 {
		t.Fatalf("PluginSet = %v, want exactly one entry", set)
	}
	if set[PluginTypeWorkflow] != impl {
		t.Errorf("PluginSet[%q] = %v, want the impl passed in", PluginTypeWorkflow, set[PluginTypeWorkflow])
	}
}

// TestServePluginPassesTheCallersLogger proves the logger reaches the arming
// step rather than being dropped for a fresh one, so a plugin's own "plugin"
// field survives onto the watchdog's warnings.
func TestServePluginPassesTheCallersLogger(t *testing.T) {
	want := zerolog.Nop().With().Str("plugin", "sentinel").Logger()

	var seen zerolog.Logger
	servePlugin(
		want,
		PluginTypeTaskSource,
		&fakePlugin{},
		func(l zerolog.Logger) func() { seen = l; return func() {} },
		func(*goplugin.ServeConfig) {},
	)

	if seen.GetLevel() != want.GetLevel() {
		t.Errorf("arm received a different logger level: %v vs %v", seen.GetLevel(), want.GetLevel())
	}
}
