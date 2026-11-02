//go:build !unix

package stderrcap

import "os"

// isAppendOnly cannot be established without fcntl, so the cap never acts.
func isAppendOnly(*os.File) bool { return false }
