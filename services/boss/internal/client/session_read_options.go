package client

// SessionReadOptions carries the per-call knobs for the session read RPCs
// (GetSession / ListSessions). It is an explicit, required argument on the
// BossClient interface so every call site states its intent — the machine-local
// endpoint boundary (BOS-473) is opt-in per call, never a client-wide mode.
//
// The zero value is the safe default: no endpoint hydration. Only the two local
// TUI reads (Home and ChatPicker) pass IncludeLocalHTTPEndpoints: true.
type SessionReadOptions struct {
	// IncludeLocalHTTPEndpoints asks the local daemon to hydrate each returned
	// session's verified machine-local HTTP endpoints. It is honored only by
	// LocalClient against a local Unix socket; RemoteClient ignores it and never
	// forwards an equivalent field to the orchestrator. Even set, the daemon
	// hydrates only when its own locality gates pass (see server.GetSession).
	IncludeLocalHTTPEndpoints bool
}
