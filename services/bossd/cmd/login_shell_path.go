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
	"time"

	"github.com/recurser/bossalib/contract/plugin/loginshell"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	loginShellNodeProbeTimeout     = 10 * time.Second
	loginShellNodeProbeWaitDelay   = 2 * time.Second
	loginShellNodeDiagnosticMaxLen = 256
)

type loginShellNodeResult struct {
	Shell      string
	Status     daemonstate.LoginShellNodeStatus
	NodePath   string
	Dir        string
	Diagnostic string
}

func (r loginShellNodeResult) probeRecord() *daemonstate.LoginShellNodeProbe {
	return &daemonstate.LoginShellNodeProbe{
		Shell:      r.Shell,
		Status:     r.Status,
		NodePath:   r.NodePath,
		Dir:        r.Dir,
		Diagnostic: r.Diagnostic,
	}
}

type loginShellProbe func(ctx context.Context, shell, dir string) ([]byte, error)

func probeLoginShellNode(ctx context.Context, shell, dir string) ([]byte, error) {
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = home
		}
	}
	args := append(loginshell.Flags(shell), loginshell.CommandLine(shell, "command -v node"))
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	cmd.WaitDelay = loginShellNodeProbeWaitDelay
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("login shell probe timed out after %s: %w", loginShellNodeProbeTimeout, ctx.Err())
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func resolveLoginShellNode(
	ctx context.Context,
	shell string,
	probe loginShellProbe,
	getenv func(string) string,
	setenv func(string, string) error,
	stat func(string) (os.FileInfo, error),
) loginShellNodeResult {
	shell = strings.TrimSpace(shell)
	if shell == "" || !loginshell.IsSupported(shell) {
		return loginShellNodeResult{
			Shell:  shell,
			Status: daemonstate.LoginShellNodeStatusSkipped,
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if probe == nil {
		probe = probeLoginShellNode
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if setenv == nil {
		setenv = os.Setenv
	}
	if stat == nil {
		stat = os.Stat
	}

	homeDir := getenv("HOME")
	if homeDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			homeDir = home
		}
	}

	out, probeErr, finished := runLoginShellProbe(ctx, shell, homeDir, probe)
	if !finished {
		return loginShellNodeResult{
			Shell:      shell,
			Status:     daemonstate.LoginShellNodeStatusFailed,
			Diagnostic: "login shell probe panicked",
		}
	}
	if probeErr != nil {
		status := daemonstate.LoginShellNodeStatusFailed
		if errors.Is(probeErr, context.DeadlineExceeded) {
			status = daemonstate.LoginShellNodeStatusTimedOut
		}
		return loginShellNodeResult{
			Shell:      shell,
			Status:     status,
			Diagnostic: shortDiagnostic(probeErr.Error()),
		}
	}

	candidate := parseLoginShellNodePath(out)
	nodePath, rejectDiag := validateLoginShellNodePath(candidate, stat)
	if rejectDiag != "" {
		return loginShellNodeResult{
			Shell:      shell,
			Status:     daemonstate.LoginShellNodeStatusNotFound,
			Diagnostic: rejectDiag,
		}
	}

	dir := filepath.Dir(nodePath)
	oldPath := getenv("PATH")
	if pathContainsDir(oldPath, dir) {
		return loginShellNodeResult{
			Shell:    shell,
			Status:   daemonstate.LoginShellNodeStatusOnPath,
			NodePath: nodePath,
			Dir:      dir,
		}
	}

	newPath := dir
	if oldPath != "" {
		newPath = dir + string(os.PathListSeparator) + oldPath
	}
	if err := setenv("PATH", newPath); err != nil {
		return loginShellNodeResult{
			Shell:      shell,
			Status:     daemonstate.LoginShellNodeStatusFailed,
			NodePath:   nodePath,
			Dir:        dir,
			Diagnostic: shortDiagnostic(err.Error()),
		}
	}
	return loginShellNodeResult{
		Shell:    shell,
		Status:   daemonstate.LoginShellNodeStatusPrepended,
		NodePath: nodePath,
		Dir:      dir,
	}
}

func runLoginShellProbe(ctx context.Context, shell, homeDir string, probe loginShellProbe) ([]byte, error, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, loginShellNodeProbeTimeout)
	defer cancel()

	var (
		out      []byte
		probeErr error
		finished bool
	)
	done := safego.Go(log.Logger, func() {
		out, probeErr = probe(probeCtx, shell, homeDir)
		finished = true
	})
	<-done
	if probeErr != nil && !errors.Is(probeErr, context.DeadlineExceeded) && errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		probeErr = fmt.Errorf("login shell probe timed out after %s: %w", loginShellNodeProbeTimeout, probeCtx.Err())
	}
	return out, probeErr, finished
}

func parseLoginShellNodePath(stdout []byte) string {
	lines := strings.Split(string(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSuffix(lines[i], "\r")
		line = strings.Trim(line, " \t")
		if line == "" {
			continue
		}
		if filepath.IsAbs(line) && !strings.HasSuffix(line, string(os.PathSeparator)) && filepath.Base(line) == "node" {
			return line
		}
	}
	return ""
}

func validateLoginShellNodePath(candidate string, stat func(string) (os.FileInfo, error)) (string, string) {
	if candidate == "" || !filepath.IsAbs(candidate) {
		return "", "no absolute node path in login shell output"
	}
	if strings.ContainsAny(candidate, ":\x00\n\r") || strings.ContainsRune(candidate, os.PathListSeparator) {
		return "", "rejected node path containing invalid character"
	}
	clean := filepath.Clean(candidate)
	if filepath.Base(clean) != "node" {
		return "", "resolved path is not a node executable"
	}
	info, err := stat(candidate)
	if err != nil {
		return "", shortDiagnostic(err.Error())
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", "resolved node path is not an executable regular file"
	}
	return clean, ""
}

func pathContainsDir(pathEnv, dir string) bool {
	if pathEnv == "" {
		return false
	}
	want := filepath.Clean(dir)
	for _, entry := range filepath.SplitList(pathEnv) {
		if entry == "" {
			continue
		}
		if filepath.Clean(entry) == want {
			return true
		}
	}
	return false
}

func shortDiagnostic(msg string) string {
	msg = strings.TrimSpace(msg)
	if idx := strings.IndexAny(msg, "\r\n"); idx >= 0 {
		msg = strings.TrimSpace(msg[:idx])
	}
	if len(msg) > loginShellNodeDiagnosticMaxLen {
		msg = msg[:loginShellNodeDiagnosticMaxLen]
	}
	return msg
}

func logLoginShellNodeResult(logger zerolog.Logger, result loginShellNodeResult) {
	var ev *zerolog.Event
	switch result.Status {
	case daemonstate.LoginShellNodeStatusPrepended, daemonstate.LoginShellNodeStatusOnPath:
		ev = logger.Info()
	case daemonstate.LoginShellNodeStatusSkipped:
		ev = logger.Debug()
	default:
		ev = logger.Warn()
	}
	ev = ev.
		Str("login_shell", result.Shell).
		Str("node", result.NodePath).
		Str("dir", result.Dir).
		Str("status", string(result.Status))
	if result.Diagnostic != "" {
		ev = ev.Str("diagnostic", result.Diagnostic)
	}
	ev.Msg("login shell node PATH resolution")
}

func persistLoginShellNodeResult(
	logger zerolog.Logger,
	appDataDir string,
	metadata daemonstate.Metadata,
	result loginShellNodeResult,
	write daemonStateWriter,
) daemonstate.Metadata {
	metadata.LoginShellNode = result.probeRecord()
	if err := write(appDataDir, metadata); err != nil {
		logger.Warn().Err(err).Msg("startup could not persist login-shell node probe result")
	}
	return metadata
}

func (w *daemonWiring) wireLoginShellPath() error {
	result := resolveLoginShellNode(
		context.Background(),
		w.settings.LoginShell,
		probeLoginShellNode,
		os.Getenv,
		os.Setenv,
		os.Stat,
	)
	logLoginShellNodeResult(log.Logger, result)
	w.daemonMetadata = persistLoginShellNodeResult(log.Logger, w.appDataDir, w.daemonMetadata, result, daemonstate.Write)
	return nil
}
