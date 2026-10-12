package projectconfig

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/recurser/bossalib/daemonstate"
)

// LocalIdentityFromState is the identity a local caller (the CLI, the local
// MCP server) matches daemons[] entries against, from the daemon-state record
// bossd writes at startup. The record is trusted only when it was read, names
// a daemon id, and is bound to socketPath (the socket the caller talks to; an
// empty socket on either side is unverifiable, not a mismatch). A trusted
// record yields its id and display name (fallbackName when the record has
// none). Otherwise matching degrades to fallbackName alone, the name the
// caller's settings resolve to, and the warning says so: entries pinned by
// daemonId then never match until bossd restarts and records an id.
func LocalIdentityFromState(md daemonstate.Metadata, readErr error, socketPath, fallbackName string) (DaemonIdentity, string) {
	if readErr == nil && md.DaemonID != "" && sameSocket(md.SocketPath, socketPath) {
		name := md.DisplayName
		if strings.TrimSpace(name) == "" {
			name = fallbackName
		}
		return DaemonIdentity{ID: md.DaemonID, DisplayName: name}, ""
	}
	return DaemonIdentity{DisplayName: fallbackName}, fmt.Sprintf("this daemon's routing id is unknown (bossd has not recorded one for this socket); daemons[] entries match by name %q only and entries pinned by daemonId never match: restart bossd (`boss daemon restart`) to record it", fallbackName)
}

// sameSocket reports whether a record's socket is the caller's. An empty
// socket on either side predates the field (or is unknown) and counts as the
// same.
func sameSocket(recorded, caller string) bool {
	if recorded == "" || caller == "" {
		return true
	}
	return filepath.Clean(recorded) == filepath.Clean(caller)
}
