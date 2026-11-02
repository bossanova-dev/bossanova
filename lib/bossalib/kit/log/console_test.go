package log

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

// setupCaptured runs Setup against a buffer standing in for stderr and returns
// the buffer and a reader for the rotated file.
func setupCaptured(t *testing.T, service string, opts ...Option) (*bytes.Buffer, func() string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	previous := zlog.Logger
	t.Cleanup(func() { zlog.Logger = previous })
	var stderr bytes.Buffer
	closer := Setup(service, append(opts, WithStderr(&stderr))...)
	t.Cleanup(func() { _ = closer.Close() })
	return &stderr, func() string {
		data, err := os.ReadFile(Path(service))
		if err != nil {
			t.Fatalf("read rotated log: %v", err)
		}
		return string(data)
	}
}

func TestSetupConsoleLinesCarryADatedTimestampAndNoColour_DaemonLifecycle_AC217102(t *testing.T) {
	stderr, _ := setupCaptured(t, "console-format")

	zlog.Info().Msg("dated console line")

	line := strings.TrimSpace(stderr.String())
	if !strings.Contains(line, "dated console line") {
		t.Fatalf("console line missing message: %q", line)
	}
	if strings.Contains(line, "\x1b[") {
		t.Errorf("console line to a non-terminal carries ANSI escapes: %q", line)
	}
	stamp, _, _ := strings.Cut(line, " ")
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("console line does not start with an RFC 3339 timestamp: %q: %v", line, err)
	}
	if since := time.Since(at); since < -time.Minute || since > time.Minute {
		t.Errorf("console timestamp %v is not the time the line was written", at)
	}
}

func TestWithStderrLevelKeepsInfoInTheFileOnly(t *testing.T) {
	stderr, file := setupCaptured(t, "stderr-level", WithStderrLevel(zerolog.WarnLevel))

	zlog.Info().Msg("routine info line")
	zlog.Warn().Msg("operator warning line")

	if got := stderr.String(); strings.Contains(got, "routine info line") {
		t.Errorf("info line reached the filtered stderr leg: %q", got)
	}
	if got := stderr.String(); !strings.Contains(got, "operator warning line") {
		t.Errorf("warn line missing from stderr: %q", got)
	}
	logged := file()
	for _, want := range []string{"routine info line", "operator warning line"} {
		if !strings.Contains(logged, want) {
			t.Errorf("rotated file missing %q: %s", want, logged)
		}
	}
}

func TestSetupWithoutStderrLevelMirrorsEveryLevel(t *testing.T) {
	stderr, _ := setupCaptured(t, "stderr-full")

	zlog.Debug().Msg("debug mirrored")
	zlog.Info().Msg("info mirrored")
	zlog.Warn().Msg("warn mirrored")

	for _, want := range []string{"debug mirrored", "info mirrored", "warn mirrored"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q without WithStderrLevel: %q", want, stderr.String())
		}
	}
}

func TestWithStderrLevelDoesNotFilterWhenStderrIsTheOnlySink(t *testing.T) {
	// A regular file where the log directory should be makes MkdirAll fail, so
	// stderr is the only record and must not withhold any level.
	state := t.TempDir()
	if err := os.WriteFile(state+"/bossanova", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", state)
	previous := zlog.Logger
	t.Cleanup(func() { zlog.Logger = previous })
	var stderr bytes.Buffer
	closer := Setup("no-file-sink", WithStderrLevel(zerolog.WarnLevel), WithStderr(&stderr))
	t.Cleanup(func() { _ = closer.Close() })

	zlog.Info().Msg("only record of this line")

	if !strings.Contains(stderr.String(), "only record of this line") {
		t.Errorf("info line dropped although stderr is the only sink: %q", stderr.String())
	}
}
