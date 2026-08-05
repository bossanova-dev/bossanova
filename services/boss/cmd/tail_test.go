package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/logtail"
	bossalog "github.com/recurser/bossalib/log"
)

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func writeLogs(t *testing.T, lines map[string]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	logs := filepath.Join(dir, "bossanova", "logs")
	if err := os.MkdirAll(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	for service, content := range lines {
		if err := os.WriteFile(filepath.Join(logs, service+".log"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTailDefaultsToBossd(t *testing.T) {
	writeLogs(t, map[string]string{"bossd": `{"level":"info","message":"from bossd"}` + "\n", "bosso": `{"level":"info","message":"from bosso"}` + "\n"})
	var out bytes.Buffer
	cmd := tailCmd()
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "from bossd") || strings.Contains(out.String(), "from bosso") {
		t.Errorf("unexpected output: %q", out.String())
	}
}

func TestTailSelectsSourceAndRejectsUnknownSource(t *testing.T) {
	writeLogs(t, map[string]string{"bosso": `{"message":"from bosso"}` + "\n"})
	var out bytes.Buffer
	cmd := tailCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"bosso"})
	if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "from bosso") {
		t.Fatalf("got output %q, error %v", out.String(), err)
	}
	cmd = tailCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"nonsense"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "bossd") {
		t.Fatalf("want valid-source error, got %v", err)
	}
}

func TestTailAllMergesAndFiltersInnerPluginFields(t *testing.T) {
	writeLogs(t, map[string]string{
		"bossd": `{"time":"2026-08-05T05:00:02Z","component":"plugin-host","message":"{\"plugin\":\"dependabot\",\"repo\":\"acme/core\",\"message\":\"plugin line\"}"}` + "\n",
		"boss":  `{"time":"2026-08-05T05:00:01Z","message":"boss line"}` + "\n",
	})
	var out bytes.Buffer
	cmd := tailCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Index(out.String(), "boss line") > strings.Index(out.String(), "plugin line") {
		t.Errorf("records not merged: %q", out.String())
	}
	out.Reset()
	cmd = tailCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--plugin", "dependabot", "--repo", "acme/core"})
	if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "plugin line") {
		t.Fatalf("filter output %q, error %v", out.String(), err)
	}
}

func TestWriteLiveRecordsMergesConcurrentSourcesByTimestamp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	records := make(chan logtail.Record, 2)
	errs := make(chan error)
	records <- logtail.ParseLine("bossd", `{"time":"2026-08-05T05:00:02Z","message":"later"}`)
	records <- logtail.ParseLine("bosso", `{"time":"2026-08-05T05:00:01Z","message":"earlier"}`)

	var messages []string
	err := writeLiveRecords(ctx, []string{"bossd", "bosso"}, records, errs, func(rec logtail.Record) error {
		messages = append(messages, rec.Message)
		if len(messages) == 2 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(messages, ","), "earlier,later"; got != want {
		t.Fatalf("live records not merged by timestamp: got %q, want %q", got, want)
	}
}

func TestTailNoLogsAndJSONOutput(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "empty"))
	var out bytes.Buffer
	cmd := tailCmd()
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil || out.Len() != 0 {
		t.Fatalf("got output %q, error %v", out.String(), err)
	}
	writeLogs(t, map[string]string{"bossd": `{"level":"info","message":"ok"}` + "\n" + "panic: boom\n"})
	out.Reset()
	cmd = tailCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatalf("unparseable JSON %q: %v", line, err)
		}
	}
}

func TestTailFollowKeepsRecordsAppendedWhileFlushingBacklog(t *testing.T) {
	writeLogs(t, map[string]string{"bossd": `{"message":"backlog"}` + "\n"})
	path := bossalog.LogPath("bossd")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out bytes.Buffer
	appended := false
	cmd := tailCmd()
	cmd.SetContext(ctx)
	cmd.SetOut(writerFunc(func(p []byte) (int, error) {
		n, err := out.Write(p)
		if err != nil {
			return n, err
		}
		if !appended && strings.Contains(string(p), "backlog") {
			appended = true
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return n, err
			}
			_, err = file.WriteString(`{"message":"during handoff"}` + "\n")
			closeErr := file.Close()
			if err != nil {
				return n, err
			}
			if closeErr != nil {
				return n, closeErr
			}
		}
		if strings.Contains(string(p), "during handoff") {
			cancel()
		}
		return n, nil
	}))
	cmd.SetArgs([]string{"--follow"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !appended || !strings.Contains(out.String(), "during handoff") {
		t.Fatalf("handoff record was lost: %q", out.String())
	}
}

func TestTailFollowDoesNotDuplicateRecordsAppendedAfterSnapshot(t *testing.T) {
	writeLogs(t, map[string]string{"bossd": `{"message":"backlog"}` + "\n"})
	path := bossalog.LogPath("bossd")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := followSources(ctx, []string{"bossd"}, 10, func(rec logtail.Record) error {
		if err := logtail.FormatPretty(&out, rec); err != nil {
			return err
		}
		if rec.Message == "after snapshot" {
			cancel()
		}
		return nil
	}, func() {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = file.Close() }()
		if _, err := file.WriteString(`{"message":"after snapshot"}` + "\n"); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "after snapshot"); got != 1 {
		t.Fatalf("after-snapshot record appeared %d times: %q", got, out.String())
	}
}

func TestTailFollowPreservesReplacementRotatedBeforeReopen(t *testing.T) {
	writeLogs(t, map[string]string{"bossd": `{"message":"backlog"}` + "\n"})
	path := bossalog.LogPath("bossd")
	logs := filepath.Dir(path)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := followSources(ctx, []string{"bossd"}, 10, func(rec logtail.Record) error {
		if err := logtail.FormatPretty(&out, rec); err != nil {
			return err
		}
		if rec.Message == "second replacement" {
			cancel()
		}
		return nil
	}, func() {
		if err := os.Rename(path, filepath.Join(logs, "bossd-2026-08-04T20-44-47.080.log")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"message":"first replacement"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, filepath.Join(logs, "bossd-2026-08-04T20-44-48.080.log")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"message":"second replacement"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"backlog", "first replacement", "second replacement"} {
		if got := strings.Count(out.String(), message); got != 1 {
			t.Fatalf("%q appeared %d times: %q", message, got, out.String())
		}
	}
}

func TestTailFollowPreservesReplacementAfterPartialSnapshotRotates(t *testing.T) {
	writeLogs(t, map[string]string{"bossd": `{"message":"partial`})
	path := bossalog.LogPath("bossd")
	logs := filepath.Dir(path)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := followSources(ctx, []string{"bossd"}, 10, func(rec logtail.Record) error {
		if err := logtail.FormatPretty(&out, rec); err != nil {
			return err
		}
		if rec.Message == "second replacement" {
			cancel()
		}
		return nil
	}, func() {
		if err := os.Rename(path, filepath.Join(logs, "bossd-2026-08-04T20-44-47.080.log")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"message":"first replacement"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, filepath.Join(logs, "bossd-2026-08-04T20-44-48.080.log")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"message":"second replacement"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "partial") {
		t.Fatalf("partial snapshot appeared in output: %q", out.String())
	}
	for _, message := range []string{"first replacement", "second replacement"} {
		if got := strings.Count(out.String(), message); got != 1 {
			t.Fatalf("%q appeared %d times: %q", message, got, out.String())
		}
	}
}

func TestTailFollowCompletesPartialLinePresentAtStartup(t *testing.T) {
	writeLogs(t, map[string]string{"bossd": `{"message":"partial`})
	path := bossalog.LogPath("bossd")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := followSources(ctx, []string{"bossd"}, 10, func(rec logtail.Record) error {
		if err := logtail.FormatPretty(&out, rec); err != nil {
			return err
		}
		if rec.Message == "partial startup" {
			cancel()
		}
		return nil
	}, func() {
		if strings.Contains(out.String(), "partial") {
			t.Fatalf("partial startup line appeared in backlog: %q", out.String())
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = file.Close() }()
		if _, err := file.WriteString(` startup"}` + "\n"); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "partial"); got != 1 {
		t.Fatalf("partial startup record appeared %d times: %q", got, out.String())
	}
}
