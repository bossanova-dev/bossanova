//go:build !darwin && !linux

package peercred

import "fmt"

func peerPID(_ uintptr) (int, error) {
	return 0, fmt.Errorf("peer PID lookup is unsupported on this platform")
}

// ParentPID fails closed on platforms without a supported process lookup.
func ParentPID(_ int) (int, error) {
	return 0, fmt.Errorf("parent PID lookup is unsupported on this platform")
}
