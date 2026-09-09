package plugin

import (
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"
)

// ServePlugin arms the parent-death watchdog and then serves impl under kind,
// blocking until the host tears this process down. Every bossd plugin main
// ends in exactly this call.
//
// Why it is one call rather than two. The arming step (StartParentWatch) is
// only correct immediately before goplugin.Serve: Serve never returns, so a
// watchdog armed after it is a watchdog never armed, and a main that omits the
// call entirely is a plugin that silently outlives a crashed bossd forever
// (BOS-1221). Eight mains previously pasted the same ServeConfig literal with
// the arming call above it, and the invariant was pinned only by a source-text
// assertion present in five of the eight packages — an assertion a
// commented-out call satisfied. Folding both steps into one constructor makes
// "this plugin serves but never armed" unrepresentable instead of merely
// asserted: a main that skips ServePlugin does not serve at all and fails at
// the handshake, which is loud.
func ServePlugin(logger zerolog.Logger, kind string, impl goplugin.Plugin) {
	servePlugin(logger, kind, impl, StartParentWatch, goplugin.Serve)
}

// servePlugin is the injectable core of ServePlugin, following the same shape
// as startParentWatch: the arming action and the serve action are parameters,
// so the ordering guarantee and the served configuration are both assertable
// without spawning a process that never returns.
//
// The returned stop function from arm is deliberately discarded: the watcher
// is meant to outlive every caller and die with the process.
func servePlugin(
	logger zerolog.Logger,
	kind string,
	impl goplugin.Plugin,
	arm func(zerolog.Logger) func(),
	serve func(*goplugin.ServeConfig),
) {
	arm(logger)

	serve(&goplugin.ServeConfig{
		HandshakeConfig: NewHandshakeForPlugin(),
		VersionedPlugins: map[int]goplugin.PluginSet{
			ProtocolVersion: {kind: impl},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}
