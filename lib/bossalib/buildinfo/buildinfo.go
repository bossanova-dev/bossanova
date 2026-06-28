// Package buildinfo holds version metadata injected at build time via ldflags.
package buildinfo

import "regexp"

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

// releaseVersionRE matches a clean semver release tag (e.g. v1.2.3). Anything
// produced by `git describe` for a non-tagged or dirty commit (v1.2.3-5-gabc,
// v1.2.3-dirty), or the "dev"/"unknown" defaults, is treated as a dev build.
var releaseVersionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// IsReleaseBuild reports whether this binary was built from a clean release
// tag. Release builds enforce plugin checksum verification; dev builds skip it
// (path hardening still applies). See docs/plans/BOS-27-*.md.
func IsReleaseBuild() bool {
	return releaseVersionRE.MatchString(Version)
}
