//go:build darwin

package daemon

// supervisionModeConfigurable is true on macOS: the supervision substrate is a
// real choice there, because the default one — a LaunchAgent in the gui/<uid>
// Aqua domain — stops spawning new jobs whenever its user is backgrounded by
// fast user switching (BOS-1184).
const supervisionModeConfigurable = true
