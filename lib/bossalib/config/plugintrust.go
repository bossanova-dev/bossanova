package config

import (
	"fmt"
	"os"
)

// isTrustedPath reports whether path is safe to load executable plugins from.
// A path is trusted only if it is owned by the current user (or root) and is
// not writable by group or other. This is the path-hardening control for
// BOS-27: it prevents a plugin planted in a group/world-writable directory
// from being exec'd with daemon privilege. On stat errors the path is
// untrusted (fail closed). On Windows this is a no-op (see _windows variant).
func isTrustedPath(path string) (bool, string) {
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Sprintf("stat failed: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return false, fmt.Sprintf("group/world-writable (%#o)", perm)
	}
	return ownerTrusted(info)
}
