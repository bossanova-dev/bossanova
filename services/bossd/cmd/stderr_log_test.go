package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bossalog "github.com/recurser/bossalib/kit/log"
	"github.com/recurser/bossd/internal/adapter/stderrcap"
	zlog "github.com/rs/zerolog/log"
)

func TestWriteFatalIsOneDatedBossdLine(t *testing.T) {
	var out bytes.Buffer
	writeFatal(&out, time.Now(), errors.New("run migrations: fail-loud migration 42"))

	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("fatal output is not one line: %q", got)
	}
	stamp, rest, _ := strings.Cut(strings.TrimSuffix(got, "\n"), " ")
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Errorf("first field %q is not RFC 3339: %v", stamp, err)
	}
	if !strings.HasPrefix(rest, "bossd: ") || !strings.Contains(rest, "fail-loud migration 42") {
		t.Errorf("fatal line %q does not carry the bossd: prefix and the error", got)
	}
}

// fakeStderr opens a file the way launchd opens StandardErrorPath and returns
// a getenv that declares it through BOSSD_STDERR_LOG_PATH when declared is true.
func fakeStderr(t *testing.T, declared bool) (*os.File, func(string) string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bossd.stderr.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, func(key string) string {
		if declared && key == stderrcap.EnvVar {
			return path
		}
		return ""
	}
}

// logThroughSupervision installs bossd's logger the way startup does and
// returns what reached the stderr file.
func logThroughSupervision(t *testing.T, stderr *os.File, supervision stderrSupervision) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	previous := zlog.Logger
	t.Cleanup(func() { zlog.Logger = previous })
	closer := bossalog.Setup("bossd-stderr-test", supervision.logOpts...)
	t.Cleanup(func() { _ = closer.Close() })
	zlog.Info().Msg("routine info line")
	zlog.Warn().Msg("operator warning line")
	data, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSupervisedStderrMirrorsOnlyWarnings_DaemonLifecycle_AC217102(t *testing.T) {
	stderr, getenv := fakeStderr(t, true)
	supervision := superviseStderr(getenv, stderr)
	if supervision.capper == nil {
		t.Fatal("a declared, matching stderr file was not treated as supervised")
	}

	got := logThroughSupervision(t, stderr, supervision)
	if strings.Contains(got, "routine info line") {
		t.Errorf("supervised stderr received an info line: %q", got)
	}
	if !strings.Contains(got, "operator warning line") {
		t.Errorf("supervised stderr lost a warning: %q", got)
	}
}

func TestUnsupervisedStderrKeepsTheFullMirror(t *testing.T) {
	stderr, getenv := fakeStderr(t, false)
	supervision := superviseStderr(getenv, stderr)
	if supervision.capper != nil {
		t.Fatal("an undeclared stderr was treated as supervised")
	}

	got := logThroughSupervision(t, stderr, supervision)
	for _, want := range []string{"routine info line", "operator warning line"} {
		if !strings.Contains(got, want) {
			t.Errorf("unsupervised stderr missing %q: %q", want, got)
		}
	}
}

func TestSupervisedStderrOverTheCapIsTrimmedBeforeTheLoggerStarts(t *testing.T) {
	stderr, getenv := fakeStderr(t, true)
	if err := stderr.Truncate(stderrcap.DefaultLimit + 1); err != nil {
		t.Fatal(err)
	}

	supervision := superviseStderr(getenv, stderr)
	if supervision.startupTrimErr != nil {
		t.Fatalf("startup trim: %v", supervision.startupTrimErr)
	}
	if info, err := os.Stat(stderr.Name()); err != nil || info.Size() != 0 {
		t.Errorf("live stderr after startup trim: (%v, %v), want empty", info, err)
	}
	if info, err := os.Stat(stderr.Name() + ".1"); err != nil || info.Size() != stderrcap.DefaultLimit+1 {
		t.Errorf("backup after startup trim: (%v, %v), want the old bytes", info, err)
	}
}

// TestSetupLoggingJoinsTheCapLoopAtShutdown: close returns only once the cap
// loop's done channel has closed, so a loop that ignored cancel would hang here.
func TestSetupLoggingJoinsTheCapLoopAtShutdown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	previous := zlog.Logger
	t.Cleanup(func() { zlog.Logger = previous })
	stderr, getenv := fakeStderr(t, true)
	w := &daemonWiring{}

	w.setupLogging(getenv, stderr)
	if len(w.cleanups) != 2 {
		t.Fatalf("supervised setupLogging registered %d cleanups, want the log closer and the cap-loop join", len(w.cleanups))
	}
	closed := make(chan struct{})
	go func() {
		w.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not join the stderr cap loop")
	}
}
