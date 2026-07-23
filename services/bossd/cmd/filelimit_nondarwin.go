//go:build unix && !darwin

package main

// maxFilesPerProc has no portable per-process file ceiling to read on non-Darwin
// unix (Linux enforces no kern.maxfilesperproc-style cap beyond RLIMIT_NOFILE
// itself), so it reports (0,false) and callers fall back to
// maxFilesPerProcFallback.
func maxFilesPerProc() (uint64, bool) { return 0, false }
