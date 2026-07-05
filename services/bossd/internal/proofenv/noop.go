package proofenv

// StaticResolver is a proof-env resolver that returns a fixed overlay WITHOUT
// ever opening a keyring. It exists so bossd tests can keep the session and
// repair-agent spawn paths hermetic: the production resolver (New) opens the
// real OS keyring, which is non-deterministic (it can prompt) and, on Linux,
// spawns a godbus/SecretService connection goroutine that is never closed —
// a leak the goleak check in services/bossd/internal/plugin's
// TestStopNoGoroutineLeak flags. Unit/lifecycle tests must never touch the
// real keyring, so they inject this via the SetProofEnvResolver seams on
// session.Lifecycle and plugin.HostServiceServer. Production wires New, never
// this.
type StaticResolver struct{ Env map[string]string }

// Resolve returns the fixed overlay (nil when Env is unset). It never opens a
// keyring.
func (r StaticResolver) Resolve() map[string]string { return r.Env }

// NewNoop returns a StaticResolver with an empty overlay: the simplest
// keyring-free resolver for tests whose spawn paths must not touch the real
// keyring but that do not care about the overlay's contents.
func NewNoop() StaticResolver { return StaticResolver{} }
