// Package buildinfo holds version metadata injected at build time via ldflags.
package buildinfo

// Set via -ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a human-readable version string.
func String() string {
	return Version + " (" + Commit + ") built " + Date
}
