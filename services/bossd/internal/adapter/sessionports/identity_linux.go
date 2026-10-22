//go:build linux

package sessionports

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// readProcessIdentity reads a process's birth identity on Linux from
// /proc/<pid>/stat field 22 (starttime, in clock ticks since boot). A missing
// /proc/<pid> directory is an authoritative absence (Present=false, nil error);
// any other read/parse error is transient and makes only the owning sessions
// incomplete.
//
// The stat-field parsing lives in the portable parseProcStatStartTime so it is
// unit-tested on any host; this file only performs the Linux-specific file
// read.
func readProcessIdentity(_ context.Context, pid int) (Identity, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Identity{Present: false}, nil
		}
		return Identity{}, err
	}
	start, err := parseProcStatStartTime(data)
	if err != nil {
		return Identity{}, err
	}
	return Identity{StartTime: start, Present: true}, nil
}
