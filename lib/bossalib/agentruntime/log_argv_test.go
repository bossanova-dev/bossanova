package agentruntime_test

import (
	"strings"
	"testing"

	"github.com/recurser/bossalib/agentruntime"
)

func TestLogTeeArgvPreservesExitCode(t *testing.T) {
	out := agentruntime.LogTeeArgv([]string{"codex", "--session-id", "abc"}, "/tmp/x.log")
	joined := strings.Join(out, " ")
	if !strings.Contains(joined, "pipefail") {
		t.Errorf("argv missing pipefail: %v", out)
	}
	if !strings.Contains(joined, "/tmp/x.log") {
		t.Errorf("argv missing log path: %v", out)
	}
	if !strings.Contains(joined, "codex --session-id abc") {
		t.Errorf("argv missing inner command: %v", out)
	}
}

// TestLogTeeArgvUsesBashNotSh pins the shell choice. `set -o pipefail` is
// a bash extension; if the argv ever regresses to `sh`, interactive agent
// sessions break on Debian/Ubuntu (dash) and Alpine (ash) with
// "Illegal option -o pipefail". This test exists specifically to prevent
// that regression.
func TestLogTeeArgvUsesBashNotSh(t *testing.T) {
	out := agentruntime.LogTeeArgv([]string{"codex"}, "/tmp/x.log")
	if len(out) < 2 {
		t.Fatalf("argv too short: %v", out)
	}
	if out[0] != "bash" {
		t.Errorf("argv[0] = %q, want \"bash\" (pipefail requires bash, not POSIX sh)", out[0])
	}
	if out[1] != "-c" {
		t.Errorf("argv[1] = %q, want \"-c\"", out[1])
	}
}

func TestLogTeeArgvEscapesQuotesInLogPath(t *testing.T) {
	out := agentruntime.LogTeeArgv([]string{"codex"}, "/tmp/foo's bar.log")
	joined := strings.Join(out, " ")
	if !strings.Contains(joined, `'\''`) {
		t.Errorf("single-quote escape missing: %v", out)
	}
}

// TestLogTeeArgvEmptyLogPathReturnsInner pins the contract used by the
// user-attached interactive spawn path (Server.spawnChatTmux): callers
// without a log path receive the inner argv UNCHANGED so tmux execs the
// agent CLI directly. The pre-fix behaviour wrapped in `bash -c "… | tee ”"`
// which made tee fail with "tee: : No such file or directory" the moment
// tmux launched the pane — the codex pane died instantly and the boss UI
// bounced straight back to the chat list.
func TestLogTeeArgvEmptyLogPathReturnsInner(t *testing.T) {
	inner := []string{"codex", "resume", "abc-123"}
	out := agentruntime.LogTeeArgv(inner, "")
	if len(out) != len(inner) {
		t.Fatalf("empty logPath must return inner unchanged; got %v, want %v", out, inner)
	}
	for i := range inner {
		if out[i] != inner[i] {
			t.Errorf("argv[%d] = %q, want %q", i, out[i], inner[i])
		}
	}
	// Specifically guard against the regression that produced `tee ''`.
	joined := strings.Join(out, " ")
	if strings.Contains(joined, "tee") {
		t.Errorf("empty logPath must not invoke tee; got %v", out)
	}
	if strings.Contains(joined, "bash") || strings.Contains(joined, "pipefail") {
		t.Errorf("empty logPath must not wrap in bash -c; got %v", out)
	}
}
