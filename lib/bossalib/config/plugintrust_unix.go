//go:build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
)

// ownerTrusted reports whether the file is owned by the current user or root.
func ownerTrusted(info os.FileInfo) (bool, string) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, "cannot read file ownership"
	}
	uid := uint32(os.Getuid())
	if st.Uid != uid && st.Uid != 0 {
		return false, fmt.Sprintf("owned by uid %d, expected %d or 0", st.Uid, uid)
	}
	return true, ""
}
