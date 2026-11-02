//go:build unix

package stderrcap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
)

const testLimit = 64

// openStderr opens path the way launchd opens StandardErrorPath.
func openStderr(t *testing.T, path string, flags int) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, flags|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func write(t *testing.T, f *os.File, s string) {
	t.Helper()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTrimCopiesThenTruncatesAndLaterAppendsStartAtZero_DaemonLifecycle_AC217102(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.stderr.log")
	f := openStderr(t, path, os.O_APPEND)
	old := strings.Repeat("x", testLimit+1)
	write(t, f, old)

	trimmed, err := Capper{Path: path, File: f, Limit: testLimit}.Trim()
	if err != nil || !trimmed {
		t.Fatalf("Trim = (%v, %v), want a trim", trimmed, err)
	}
	if got := read(t, path+".1"); got != old {
		t.Errorf("backup holds %q, want the pre-trim bytes", got)
	}
	if got := read(t, path); got != "" {
		t.Errorf("live file holds %q after trim, want empty", got)
	}
	if _, err := os.Stat(path + ".1.tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary backup left behind: %v", err)
	}

	write(t, f, "after\n")
	if got := read(t, path); got != "after\n" {
		t.Errorf("append through the same handle produced %q; a sparse hole means it kept its old offset", got)
	}
}

func TestTrimLeavesAFileAtOrUnderTheLimitAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.stderr.log")
	f := openStderr(t, path, os.O_APPEND)
	write(t, f, strings.Repeat("x", testLimit))

	if trimmed, err := (Capper{Path: path, File: f, Limit: testLimit}).Trim(); err != nil || trimmed {
		t.Fatalf("Trim = (%v, %v), want no trim at the limit", trimmed, err)
	}
	assertUntouched(t, path, testLimit)
}

func TestTrimRefusesANonAppendHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.stderr.log")
	f := openStderr(t, path, 0)
	write(t, f, strings.Repeat("x", testLimit+1))

	if trimmed, err := (Capper{Path: path, File: f, Limit: testLimit}).Trim(); err != nil || trimmed {
		t.Fatalf("Trim = (%v, %v), want a non-append handle left alone", trimmed, err)
	}
	assertUntouched(t, path, testLimit+1)
}

func TestTrimRefusesAPathThatIsNotTheHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bossd.stderr.log")
	other := filepath.Join(dir, "other.log")
	write(t, openStderr(t, path, os.O_APPEND), strings.Repeat("x", testLimit+1))
	f := openStderr(t, other, os.O_APPEND)
	write(t, f, strings.Repeat("y", testLimit+1))

	if trimmed, err := (Capper{Path: path, File: f, Limit: testLimit}).Trim(); err != nil || trimmed {
		t.Fatalf("Trim = (%v, %v), want a mismatched path left alone", trimmed, err)
	}
	assertUntouched(t, path, testLimit+1)
	assertUntouched(t, other, testLimit+1)
}

func assertUntouched(t *testing.T, path string, size int) {
	t.Helper()
	if got := len(read(t, path)); got != size {
		t.Errorf("%s is %d bytes, want it untouched at %d", path, got, size)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("%s.1 exists although nothing should have been trimmed", path)
	}
}

func TestSupervisedRequiresTheVariableToNameFd2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bossd.stderr.log")
	stderr := openStderr(t, path, os.O_APPEND)
	elsewhere := filepath.Join(dir, "elsewhere.log")
	openStderr(t, elsewhere, os.O_APPEND)
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == EnvVar {
				return value
			}
			return ""
		}
	}

	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "variable names the stderr file", value: path, want: true},
		{name: "variable unset", value: ""},
		{name: "variable names a different file", value: elsewhere},
		{name: "variable names a missing file", value: filepath.Join(dir, "missing.log")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Supervised(env(tc.value), stderr)
			if ok != tc.want {
				t.Fatalf("Supervised = (%q, %v), want supervised=%v", got, ok, tc.want)
			}
			if ok && got != path {
				t.Errorf("Supervised path = %q, want %q", got, path)
			}
		})
	}
}

func TestStartTrimsOnTheIntervalAndItsDoneClosesAfterCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bossd.stderr.log")
		f := openStderr(t, path, os.O_APPEND)
		var logged bytes.Buffer
		ctx, cancel := context.WithCancel(context.Background())
		done := Capper{Path: path, File: f, Limit: testLimit}.Start(ctx, time.Minute, zerolog.New(&logged))

		write(t, f, strings.Repeat("x", testLimit+1))
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := read(t, path); got != "" {
			t.Errorf("live file holds %d bytes one interval after passing the cap, want a trim", len(got))
		}

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("done did not close after cancel")
		}
	})
}

func TestTrimAndReportWarnsOnceAndNeverPanicsOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.stderr.log")
	f := openStderr(t, path, os.O_APPEND)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer

	Capper{Path: path, File: f, Limit: testLimit}.TrimAndReport(zerolog.New(&logged))

	if got := strings.Count(logged.String(), `"level":"warn"`); got != 1 {
		t.Errorf("logged %d warnings for a failed trim, want 1: %s", got, logged.String())
	}
}
