//go:build !darwin

package config

// computerName reports that this platform keeps no operator-facing machine name
// separate from its OS hostname, so DefaultDisplayHostname falls back to the
// suffix-stripped hostname. No subprocess is spawned, and nothing outside the
// darwin build file names a macOS-only tool.
func computerName() (string, bool) { return "", false }
