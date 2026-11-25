package setupscript

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/plugin/loginshell"
)

const (
	loginShellLookPathTimeout   = 5 * time.Second
	loginShellLookPathWaitDelay = 2 * time.Second
	probeNotFoundExitCode       = 42
)

type probeOutcome int

const (
	probeFailed probeOutcome = iota
	probeFound
	probeNotFound
)

// LoginShellLookPath returns a LookPath function that resolves tool names
// through shell using the same login-shell wrapping as Spec.Execute. When
// shell is empty or unsupported, or the probe fails to run, it falls back to
// exec.LookPath. When the login shell runs the probe and reports the tool
// absent, it returns exec.ErrNotFound without falling back. Only lowercase
// ASCII tool names (^[a-z]+$) are accepted.
func LoginShellLookPath(shell string) func(string) (string, error) {
	return loginShellLookPath(shell, exec.LookPath)
}

func loginShellLookPath(shell string, fallback func(string) (string, error)) func(string) (string, error) {
	return func(tool string) (string, error) {
		if !isValidToolName(tool) {
			return "", fmt.Errorf("invalid tool name %q", tool)
		}
		if shell == "" || !loginshell.IsSupported(shell) {
			return fallback(tool)
		}
		switch resolved, outcome := probeToolInLoginShell(shell, tool); outcome {
		case probeFound:
			return resolved, nil
		case probeNotFound:
			return "", fmt.Errorf("%s: %w", tool, exec.ErrNotFound)
		default:
			return fallback(tool)
		}
	}
}

func isValidToolName(tool string) bool {
	if tool == "" {
		return false
	}
	for i := 0; i < len(tool); i++ {
		if tool[i] < 'a' || tool[i] > 'z' {
			return false
		}
	}
	return true
}

func probeToolInLoginShell(shell, tool string) (string, probeOutcome) {
	probeCmd := fmt.Sprintf(`command -v "$1" || exit %d`, probeNotFoundExitCode)
	argv := loginshell.Wrap(shell, loginshell.Flags(shell), []string{"sh", "-c", probeCmd, "sh", tool})
	if len(argv) == 0 {
		return "", probeFailed
	}

	ctx, cancel := context.WithTimeout(context.Background(), loginShellLookPathTimeout)
	defer cancel()

	// #nosec G204 -- probes tool resolution inside the user's configured login shell in $HOME; tool is validated ^[a-z]+$
	// owner=@recurser review-by=2027-05-25 issue=BOS-2713
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// nil Stdin connects os.DevNull (empty stdin) so interactive shells never block on input.
	cmd.Stdin = nil
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cmd.Dir = home
	} else {
		cmd.Dir = os.TempDir()
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	cmd.WaitDelay = loginShellLookPathWaitDelay

	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil || cmd.ProcessState == nil {
			return "", probeFailed
		}
		if cmd.ProcessState.ExitCode() == probeNotFoundExitCode {
			return "", probeNotFound
		}
		if !errors.Is(err, exec.ErrWaitDelay) || !cmd.ProcessState.Success() {
			return "", probeFailed
		}
	}

	firstLine, _, _ := strings.Cut(stdout.String(), "\n")
	resolved := strings.TrimSpace(firstLine)
	if resolved == "" {
		return "", probeFailed
	}
	return resolved, probeFound
}
