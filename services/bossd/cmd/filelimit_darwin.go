//go:build darwin

package main

import "golang.org/x/sys/unix"

// maxFilesPerProc reads kern.maxfilesperproc on Darwin (the hard ceiling the
// kernel enforces on RLIMIT_NOFILE regardless of the rlimit). Returns (0,false)
// on error so callers fall back to maxFilesPerProcFallback. This lives in a
// darwin-only file because unix.SysctlUint32 is a BSD/Darwin symbol that does
// not exist in the Linux golang.org/x/sys/unix build (a runtime GOOS guard
// would not gate compile-time symbol resolution).
func maxFilesPerProc() (uint64, bool) {
	v, err := unix.SysctlUint32("kern.maxfilesperproc")
	if err != nil || v == 0 {
		return 0, false
	}
	return uint64(v), true
}
