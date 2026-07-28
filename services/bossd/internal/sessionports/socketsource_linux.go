//go:build linux

package sessionports

// newPlatformSocketSource selects the procfs socket source on Linux. The
// implementation (procfsSocketSource) is portable and lives in
// procfssocket.go; only this selection is Linux-gated so macOS gets lsof.
func newPlatformSocketSource() SocketSource {
	return &procfsSocketSource{}
}
