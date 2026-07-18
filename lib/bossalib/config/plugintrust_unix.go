//go:build !windows

package config

import (
	"fmt"
	"math"
	"os"
	"syscall"
)

// currentUID converts a raw os.Getuid() result to a uint32, rejecting values
// that cannot be represented (e.g. -1 on platforms where Getuid is
// unsupported, or an out-of-range value). Guarding the cast avoids a silent
// integer-overflow wrap (gosec G115).
func currentUID(raw int) (uint32, bool) {
	if raw < 0 || raw > math.MaxUint32 {
		return 0, false
	}
	return uint32(raw), true
}

// ownerTrusted reports whether the file is owned by the current user or root.
func ownerTrusted(info os.FileInfo) (bool, string) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, "cannot read file ownership"
	}
	uid, ok := currentUID(os.Getuid())
	if !ok {
		return false, "cannot determine current uid"
	}
	if st.Uid != uid && st.Uid != 0 {
		return false, fmt.Sprintf("owned by uid %d, expected %d or 0", st.Uid, uid)
	}
	return true, ""
}
