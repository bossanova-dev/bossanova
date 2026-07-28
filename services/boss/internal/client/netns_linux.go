//go:build linux

package client

import "os"

// networkNamespaceIdentity returns this process's network-namespace identity on
// Linux: the target of the /proc/self/ns/net magic symlink (e.g.
// "net:[4026531840]"). The daemon compares it byte-for-byte against its own
// before hydrating machine-local endpoints, so a Unix socket bridged across
// namespaces cannot make a localhost link resolve to a different service.
//
// Any read failure returns "" so the daemon fails closed: an empty identity can
// never equal the daemon's non-empty one on Linux.
func networkNamespaceIdentity() string {
	target, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return ""
	}
	return target
}
