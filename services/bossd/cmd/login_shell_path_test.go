package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/daemonstate"
	"github.com/rs/zerolog"
)

func writeTestNode(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, "node")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod %s: %v", p, err)
	}
	return p
}

func TestResolveLoginShellNode(t *testing.T) {
	tmp := t.TempDir()
	nodeDir := filepath.Join(tmp, "nvm", "bin")
	nodeExe := writeTestNode(t, nodeDir, 0o755)
	nonExec := writeTestNode(t, filepath.Join(tmp, "nonexec"), 0o644)
	dirPath := filepath.Join(tmp, "dir-as-node", "node")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatalf("mkdir dirPath: %v", err)
	}
	otherBinDir := filepath.Join(tmp, "other", "bin")
	if err := os.MkdirAll(otherBinDir, 0o755); err != nil {
		t.Fatalf("mkdir otherBinDir: %v", err)
	}
	otherExe := filepath.Join(otherBinDir, "tool")
	if err := os.WriteFile(otherExe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write otherExe: %v", err)
	}

	symlinkTargetDir := filepath.Join(tmp, "cellar", "v22", "bin")
	symlinkTarget := writeTestNode(t, symlinkTargetDir, 0o755)
	symlinkDir := filepath.Join(tmp, "stable", "bin")
	if err := os.MkdirAll(symlinkDir, 0o755); err != nil {
		t.Fatalf("mkdir symlinkDir: %v", err)
	}
	symlinkNode := filepath.Join(symlinkDir, "node")
	if err := os.Symlink(symlinkTarget, symlinkNode); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	const basePath = "/usr/bin:/bin"

	tests := []struct {
		name           string
		shell          string
		initialPath    string
		probeOut       []byte
		probeErr       error
		setenvErr      error
		wantStatus     daemonstate.LoginShellNodeStatus
		wantPath       string
		wantNodePath   string
		wantDir        string
		wantSetenvCall bool
		wantProbeCall  bool
		wantDiagSubstr string
	}{
		{
			name:           "prepended",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte(nodeExe + "\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusPrepended,
			wantPath:       nodeDir + string(os.PathListSeparator) + basePath,
			wantNodePath:   nodeExe,
			wantDir:        nodeDir,
			wantSetenvCall: true,
			wantProbeCall:  true,
		},
		{
			name:           "on_path",
			shell:          "/bin/zsh",
			initialPath:    basePath + string(os.PathListSeparator) + nodeDir,
			probeOut:       []byte(nodeExe + "\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusOnPath,
			wantPath:       basePath + string(os.PathListSeparator) + nodeDir,
			wantNodePath:   nodeExe,
			wantDir:        nodeDir,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "on_path_trailing_slash",
			shell:          "/bin/bash",
			initialPath:    basePath + string(os.PathListSeparator) + nodeDir + "/",
			probeOut:       []byte(nodeExe + "\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusOnPath,
			wantPath:       basePath + string(os.PathListSeparator) + nodeDir + "/",
			wantNodePath:   nodeExe,
			wantDir:        nodeDir,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "empty_path",
			shell:          "/bin/zsh",
			initialPath:    "",
			probeOut:       []byte(nodeExe + "\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusPrepended,
			wantPath:       nodeDir,
			wantNodePath:   nodeExe,
			wantDir:        nodeDir,
			wantSetenvCall: true,
			wantProbeCall:  true,
		},
		{
			name:           "timeout",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeErr:       fmt.Errorf("shell timed out: %w", context.DeadlineExceeded),
			wantStatus:     daemonstate.LoginShellNodeStatusTimedOut,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
			wantDiagSubstr: "deadline exceeded",
		},
		{
			name:           "exec_failure",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeErr:       errors.New("exit status 1"),
			wantStatus:     daemonstate.LoginShellNodeStatusFailed,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
			wantDiagSubstr: "exit status 1",
		},
		{
			name:           "relative_path",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte("bin/node\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusNotFound,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "path_with_colon",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte("/tmp/bad:dir/node\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusNotFound,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "path_with_nul",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte("/tmp/bad\x00dir/node\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusNotFound,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "path_with_carriage_return",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte("/tmp/bad\rdir/node\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusNotFound,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "non_executable_file",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte(nonExec + "\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusNotFound,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "directory_instead_of_regular_file",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte(dirPath + "\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusNotFound,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "noisy_multi_line_output",
			shell:          "/opt/homebrew/bin/fish",
			initialPath:    basePath,
			probeOut:       []byte("Welcome to fish, the friendly interactive shell\n/some/earlier/banner/path\nLoaded nvm v22\n" + nodeExe + "\r\n\nlogout\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusPrepended,
			wantPath:       nodeDir + string(os.PathListSeparator) + basePath,
			wantNodePath:   nodeExe,
			wantDir:        nodeDir,
			wantSetenvCall: true,
			wantProbeCall:  true,
		},
		{
			name:           "shell_function_ignores_earlier_non_node_executable",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte(otherExe + "\nnode\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusNotFound,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "shell_alias_ignores_earlier_non_node_executable",
			shell:          "/bin/bash",
			initialPath:    basePath,
			probeOut:       []byte(otherExe + "\nalias node='nvm use default && node'\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusNotFound,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "trailing_teardown_non_node_executable_ignored",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte(nodeExe + "\n" + otherExe + "\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusPrepended,
			wantPath:       nodeDir + string(os.PathListSeparator) + basePath,
			wantNodePath:   nodeExe,
			wantDir:        nodeDir,
			wantSetenvCall: true,
			wantProbeCall:  true,
		},
		{
			name:           "empty_output",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte("   \n\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusNotFound,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  true,
		},
		{
			name:           "setenv_error",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte(nodeExe + "\n"),
			setenvErr:      errors.New("setenv failed"),
			wantStatus:     daemonstate.LoginShellNodeStatusFailed,
			wantPath:       basePath,
			wantNodePath:   nodeExe,
			wantDir:        nodeDir,
			wantSetenvCall: true,
			wantProbeCall:  true,
			wantDiagSubstr: "setenv failed",
		},
		{
			name:           "empty_shell",
			shell:          "",
			initialPath:    basePath,
			wantStatus:     daemonstate.LoginShellNodeStatusSkipped,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  false,
		},
		{
			name:           "unsupported_shell",
			shell:          "/bin/tcsh",
			initialPath:    basePath,
			wantStatus:     daemonstate.LoginShellNodeStatusSkipped,
			wantPath:       basePath,
			wantSetenvCall: false,
			wantProbeCall:  false,
		},
		{
			name:           "symlink_not_resolved",
			shell:          "/bin/zsh",
			initialPath:    basePath,
			probeOut:       []byte(symlinkNode + "\n"),
			wantStatus:     daemonstate.LoginShellNodeStatusPrepended,
			wantPath:       symlinkDir + string(os.PathListSeparator) + basePath,
			wantNodePath:   symlinkNode,
			wantDir:        symlinkDir,
			wantSetenvCall: true,
			wantProbeCall:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{
				"PATH": tt.initialPath,
				"HOME": tmp,
			}
			probeCalled := false
			probe := func(ctx context.Context, shell, dir string) ([]byte, error) {
				probeCalled = true
				if _, ok := ctx.Deadline(); !ok {
					t.Errorf("probe context has no deadline")
				}
				if shell != tt.shell {
					t.Errorf("probe shell = %q, want %q", shell, tt.shell)
				}
				if dir != tmp {
					t.Errorf("probe dir = %q, want %q", dir, tmp)
				}
				return tt.probeOut, tt.probeErr
			}
			setenvCalled := false
			setenv := func(k, v string) error {
				setenvCalled = true
				if tt.setenvErr != nil {
					return tt.setenvErr
				}
				env[k] = v
				return nil
			}
			getenv := func(k string) string {
				return env[k]
			}

			got := resolveLoginShellNode(context.Background(), tt.shell, probe, getenv, setenv, os.Stat)
			if got.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q (diagnostic=%q)", got.Status, tt.wantStatus, got.Diagnostic)
			}
			if probeCalled != tt.wantProbeCall {
				t.Errorf("probeCalled = %v, want %v", probeCalled, tt.wantProbeCall)
			}
			if setenvCalled != tt.wantSetenvCall {
				t.Errorf("setenvCalled = %v, want %v", setenvCalled, tt.wantSetenvCall)
			}
			if env["PATH"] != tt.wantPath {
				t.Errorf("PATH = %q, want %q", env["PATH"], tt.wantPath)
			}
			if got.NodePath != tt.wantNodePath {
				t.Errorf("NodePath = %q, want %q", got.NodePath, tt.wantNodePath)
			}
			if got.Dir != tt.wantDir {
				t.Errorf("Dir = %q, want %q", got.Dir, tt.wantDir)
			}
			if tt.wantDiagSubstr != "" && !strings.Contains(got.Diagnostic, tt.wantDiagSubstr) {
				t.Errorf("Diagnostic = %q, want substring %q", got.Diagnostic, tt.wantDiagSubstr)
			}
		})
	}
}

func TestProbeLoginShellNodeReadsShellRC(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available on PATH")
	}

	tmpHome := t.TempDir()
	binDir := filepath.Join(tmpHome, "custom-node-bin")
	fakeNode := writeTestNode(t, binDir, 0o755)
	bashrc := filepath.Join(tmpHome, ".bashrc")
	if err := os.WriteFile(bashrc, []byte(fmt.Sprintf("export PATH=%q:$PATH\n", binDir)), 0o600); err != nil {
		t.Fatalf("write .bashrc: %v", err)
	}

	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", "/usr/bin:/bin")

	env := map[string]string{
		"HOME": tmpHome,
		"PATH": "/usr/bin:/bin",
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginShellNodeProbeTimeout)
	defer cancel()

	got := resolveLoginShellNode(
		ctx,
		bashPath,
		probeLoginShellNode,
		func(k string) string { return env[k] },
		func(k, v string) error {
			env[k] = v
			return nil
		},
		os.Stat,
	)
	if got.Status != daemonstate.LoginShellNodeStatusPrepended {
		t.Fatalf("status = %q, want %q (diagnostic=%q)", got.Status, daemonstate.LoginShellNodeStatusPrepended, got.Diagnostic)
	}
	if got.NodePath != fakeNode {
		t.Errorf("NodePath = %q, want %q", got.NodePath, fakeNode)
	}
	if got.Dir != binDir {
		t.Errorf("Dir = %q, want %q", got.Dir, binDir)
	}
	wantPath := binDir + string(os.PathListSeparator) + "/usr/bin:/bin"
	if env["PATH"] != wantPath {
		t.Errorf("PATH = %q, want %q", env["PATH"], wantPath)
	}
}

func TestPersistLoginShellNodeResult(t *testing.T) {
	dir := t.TempDir()
	res := loginShellNodeResult{
		Shell:    "/bin/zsh",
		Status:   daemonstate.LoginShellNodeStatusPrepended,
		NodePath: "/opt/nvm/bin/node",
		Dir:      "/opt/nvm/bin",
	}

	meta := persistLoginShellNodeResult(zerolog.Nop(), dir, daemonstate.Metadata{PID: 42}, res, daemonstate.Write)
	if meta.LoginShellNode == nil || meta.LoginShellNode.Status != daemonstate.LoginShellNodeStatusPrepended {
		t.Fatalf("returned metadata = %#v, want prepended LoginShellNode", meta.LoginShellNode)
	}

	readBack, err := daemonstate.Read(dir)
	if err != nil {
		t.Fatalf("read back daemonstate: %v", err)
	}
	if readBack.LoginShellNode == nil || readBack.LoginShellNode.NodePath != res.NodePath || readBack.LoginShellNode.Dir != res.Dir {
		t.Fatalf("persisted LoginShellNode = %#v, want %#v", readBack.LoginShellNode, res)
	}

	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)
	_ = persistLoginShellNodeResult(logger, dir, daemonstate.Metadata{PID: 42}, res, func(string, daemonstate.Metadata) error {
		return errors.New("disk full")
	})
	if !strings.Contains(logBuf.String(), "disk full") {
		t.Errorf("expected write failure warning in log, got %q", logBuf.String())
	}
}

func TestValidateLoginShellNodePathRejectsNonNodeExecutable(t *testing.T) {
	tmp := t.TempDir()
	otherExe := filepath.Join(tmp, "python")
	if err := os.WriteFile(otherExe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write otherExe: %v", err)
	}
	gotPath, diag := validateLoginShellNodePath(otherExe, os.Stat)
	if gotPath != "" || diag == "" {
		t.Fatalf("validateLoginShellNodePath(%q) = (%q, %q), want rejection", otherExe, gotPath, diag)
	}
}
