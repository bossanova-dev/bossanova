//go:build !linux

package client

// networkNamespaceIdentity is a no-op off Linux. macOS has no /proc/self/ns/net
// and no per-process network namespaces, so there is nothing to send; the
// daemon treats a local Unix-socket request as sufficient there and never
// consults this value. Returning "" keeps the request field empty.
func networkNamespaceIdentity() string {
	return ""
}
