package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// processFiles lists the files a process tree holds open. The production
// implementation shells out to ps and lsof (or reads /proc on Linux).
type processFiles interface {
	OpenFiles(rootPID int) ([]string, error)
}

const processInspectTimeout = 3 * time.Second

// inspectToolDirs covers launchd and systemd PATHs that omit /usr/sbin, where
// macOS keeps lsof.
var inspectToolDirs = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"}

func inspectTool(name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, dir := range inspectToolDirs {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return name
}

type osProcessFiles struct{}

func (osProcessFiles) OpenFiles(rootPID int) ([]string, error) {
	if rootPID <= 0 {
		return nil, fmt.Errorf("invalid pid %d", rootPID)
	}
	pids, err := processTree(rootPID)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "linux" {
		return openFilesFromProc(pids), nil
	}
	return openFilesFromLsof(pids)
}

func runTool(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), processInspectTimeout)
	defer cancel()
	// #nosec G204 -- fixed system tool resolved by inspectTool; arguments are integer pids and constant flags, no shell.
	cmd := exec.CommandContext(ctx, inspectTool(name), args...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// processTree returns rootPID and all its descendants.
func processTree(rootPID int) ([]int, error) {
	out, err := runTool("ps", "-Ao", "pid=,ppid=")
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	children := map[int][]int{}
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 == nil && err2 == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	seen := map[int]bool{}
	queue := []int{rootPID}
	var tree []int
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		tree = append(tree, pid)
		queue = append(queue, children[pid]...)
	}
	return tree, nil
}

func openFilesFromProc(pids []int) []string {
	var files []string
	for _, pid := range pids {
		fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if target, err := os.Readlink(filepath.Join(fdDir, e.Name())); err == nil && filepath.IsAbs(target) {
				files = append(files, target)
			}
		}
	}
	return files
}

func openFilesFromLsof(pids []int) ([]string, error) {
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	out, err := runTool("lsof", "-p", strings.Join(list, ","), "-Fn")
	// lsof exits 1 when some descriptors are unreadable yet still prints the rest.
	var exitErr *exec.ExitError
	if err != nil && (len(out) == 0 || !errors.As(err, &exitErr)) {
		return nil, fmt.Errorf("lsof: %w", err)
	}
	var files []string
	for line := range bytes.SplitSeq(out, []byte{'\n'}) {
		if len(line) > 1 && line[0] == 'n' && line[1] == '/' {
			files = append(files, string(line[1:]))
		}
	}
	return files, nil
}
