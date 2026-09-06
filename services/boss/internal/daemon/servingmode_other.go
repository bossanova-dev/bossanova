//go:build !darwin

package daemon

// standaloneServingSupported is false off macOS. BOS-1181's acceptance criteria
// require the Linux path to be behaviourally unchanged, so the standalone
// verdict is never reachable there and every caller keeps its previous branch.
//
// Note this is a scope boundary, not a claim that Linux has no direct-spawn
// fallback: platformEnsureRunning in systemd.go does start bossd directly when
// the unit will not start. The equivalent systemd-side hazard is left for
// separate tracking rather than widened into this fix.
const standaloneServingSupported = false
