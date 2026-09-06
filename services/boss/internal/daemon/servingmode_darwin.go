//go:build darwin

package daemon

// standaloneServingSupported is true on macOS: platformEnsureRunning falls back
// to spawning bossd directly when the LaunchAgent is absent or cannot load, so
// a profile can legitimately be served by a process launchd knows nothing about.
const standaloneServingSupported = true
