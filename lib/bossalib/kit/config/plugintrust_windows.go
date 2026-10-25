//go:build windows

package config

import "os"

// ownerTrusted is a no-op on Windows: NTFS ACLs do not map to Unix mode bits,
// so permission/ownership hardening is best-effort only on this platform.
// Checksum verification (release builds) is the effective control here.
func ownerTrusted(info os.FileInfo) (bool, string) {
	return true, ""
}
