//go:build linux

package peercred

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func peerPID(fd uintptr) (int, error) {
	cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(cred.Pid), nil
}

// ParentPID returns the current parent of pid, or a process-gone error.
func ParentPID(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("process %d: %w", pid, os.ErrNotExist)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read process %d: %w", pid, err)
	}
	parent, err := parseParentPID(string(stat))
	if err != nil {
		return 0, fmt.Errorf("read process %d: %w", pid, err)
	}
	return parent, nil
}

func parseParentPID(stat string) (int, error) {
	// comm is parenthesized and can itself contain spaces and parentheses.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, fmt.Errorf("process stat has no command terminator")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("process stat has no parent PID")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("invalid parent PID: %w", err)
	}
	if parent < 0 {
		return 0, fmt.Errorf("invalid parent PID %d", parent)
	}
	return parent, nil
}
