package sessionports

import "github.com/rs/zerolog"

// NewDetector builds a Detector wired to the live platform sources: a `ps`
// process snapshot with sysctl/procfs identity reads, and an lsof (macOS) or
// procfs (Linux) socket source. Off macOS/Linux the adapters degrade every
// scan to incomplete rather than panicking, so the package builds and links on
// every platform CI targets.
func NewDetector(logger zerolog.Logger) *Detector {
	return NewDetectorWithSources(logger, newCommandProcessSource(), newPlatformSocketSource())
}
