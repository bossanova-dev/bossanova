//go:build unix

package stderrcap

import (
	"os"

	"golang.org/x/sys/unix"
)

// isAppendOnly reports whether f was opened with O_APPEND, the only mode in
// which truncating the file under a live writer leaves no sparse hole.
func isAppendOnly(f *os.File) bool {
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
	return err == nil && flags&unix.O_APPEND != 0
}
