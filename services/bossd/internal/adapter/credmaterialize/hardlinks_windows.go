//go:build windows

package credmaterialize

import (
	"io/fs"
	"os"
	"syscall"
)

// authFileHasMultipleLinks reports whether a regular auth.json has aliases that
// may belong to another account. If metadata cannot be read, refuse the read;
// replacing the leaf remains safe and restores account-local bytes.
func authFileHasMultipleLinks(path string, _ fs.FileInfo) bool {
	file, err := os.Open(path)
	if err != nil {
		return true
	}
	defer file.Close()

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &info); err != nil {
		return true
	}
	return info.NumberOfLinks > 1
}
