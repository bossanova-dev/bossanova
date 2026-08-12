package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
	err := writeLiveRecords(ctx, serviceSources([]string{"bossd", "bosso"}), records, errs, func(rec logtail.Record) error {
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
	err := followSources(ctx, serviceSources([]string{"bossd"}), 10, func(rec logtail.Record) error {
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
	err := followSources(ctx, serviceSources([]string{"bossd"}), 10, func(rec logtail.Record) error {
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
	err := followSources(ctx, serviceSources([]string{"bossd"}), 10, func(rec logtail.Record) error {
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
	err := followSources(ctx, serviceSources([]string{"bossd"}), 10, func(rec logtail.Record) error {
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

// writeAgentLogs points config at a temp worktree base and fills the
// agent-logs directory bossd derives from it. Returns that directory.
func writeAgentLogs(t *testing.T, files map[string]string) string {
	t.Helper()
	base := t.TempDir()
	settings := filepath.Join(base, "settings.json")
	body := `{"worktree_base_dir":` + strconv.Quote(filepath.Join(base, "worktrees")) + `}`
	if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settings)
	dir := bossalog.AgentLogsDir(filepath.Join(base, "worktrees"))
	if files == nil {
		return dir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".log"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const agentUUID = "3f2a1b4c-5d6e-4f70-8a91-b2c3d4e5f607"

func runTail(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := tailCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("boss tail %v: %v (output %q)", args, err, out.String())
	}
	return out.String()
}

func TestTailReadsHeadlessAgentLogByAgentSessionID(t *testing.T) {
	writeAgentLogs(t, map[string]string{agentUUID: `{"ts":"2026-08-05T05:00:01Z","text":"[runner] spawning claude"}` + "\n" +
		`{"ts":"2026-08-05T05:00:02Z","text":"error: rate limited"}` + "\n"})
	got := runTail(t, agentUUID)
	for _, want := range []string{"[runner] spawning claude", "error: rate limited"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestTailReadsRawAgentCaptureAndKeepsUntimestampedLinesOffTheEpoch(t *testing.T) {
	dir := writeAgentLogs(t, map[string]string{agentUUID: "\x1b[32m✓\x1b[0m build passed\n"})
	info, err := os.Stat(filepath.Join(dir, agentUUID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	got := runTail(t, agentUUID)
	if !strings.Contains(got, "✓ build passed") || strings.Contains(got, "\x1b[32m") {
		t.Errorf("ansi not stripped: %q", got)
	}
	// Rule: an untimestamped raw line is anchored to the log's mod time, never
	// to the zero time that Merge would sort ahead of every other source.
	if !strings.Contains(got, info.ModTime().Format(time.TimeOnly)) {
		t.Errorf("raw line not anchored to the log mod time %s: %q", info.ModTime().Format(time.TimeOnly), got)
	}
	if strings.Contains(got, time.Time{}.Format(time.TimeOnly)) {
		t.Errorf("raw line sorted to the epoch: %q", got)
	}
}

func TestTailInterleavesAgentAndServiceRecordsByTimestamp(t *testing.T) {
	writeLogs(t, map[string]string{
		"bossd": `{"time":"2026-08-05T05:00:01Z","message":"service first"}` + "\n" +
			`{"time":"2026-08-05T05:00:03Z","message":"service last"}` + "\n",
	})
	writeAgentLogs(t, map[string]string{agentUUID: `{"ts":"2026-08-05T05:00:02Z","text":"agent middle"}` + "\n"})
	got := runTail(t, "bossd", agentUUID)
	first, middle := strings.Index(got, "service first"), strings.Index(got, "agent middle")
	last := strings.Index(got, "service last")
	if first < 0 || middle < 0 || last < 0 {
		t.Fatalf("missing a record: %q", got)
	}
	if first >= middle || middle >= last {
		t.Errorf("records not interleaved by timestamp: %q", got)
	}
}

func TestTailFollowsAnAgentLogThatDoesNotExistYet(t *testing.T) {
	dir := writeAgentLogs(t, map[string]string{})
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sources, notice, err := resolveSources([]string{agentUUID}, false, dir)
	if err != nil || notice != "" {
		t.Fatalf("got notice %q, error %v", notice, err)
	}
	if len(sources) != 1 || !sources[0].Agent || sources[0].Seed.IsZero() {
		t.Fatalf("unresolved future agent log: %+v", sources)
	}
}

// followSources is the only production site that installs logtail.Source.Parse,
// so a test that builds its own AgentParser cannot notice the wiring going
// away. Follow a real raw pane capture instead and assert two things about a
// line the follower emitted live: the ANSI is stripped, and the record carries
// the seeded time. The default service parser does neither — it keeps the
// escapes and leaves Time zero — so this fails if the hook is dropped.
func TestTailFollowParsesAgentLogLinesWithTheAgentParser(t *testing.T) {
	dir := writeAgentLogs(t, map[string]string{agentUUID: "\x1b[32m✓\x1b[0m backlog line\n"})
	path := bossalog.AgentLogFile(dir, agentUUID)
	sources, notice, err := resolveSources([]string{agentUUID}, false, dir)
	if err != nil || notice != "" {
		t.Fatalf("got notice %q, error %v", notice, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var live logtail.Record
	err = followSources(ctx, sources, 10, func(rec logtail.Record) error {
		if strings.Contains(rec.Raw, "live line") {
			live = rec
			cancel()
		}
		return nil
	}, func() {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = file.Close() }()
		if _, err := file.WriteString("\x1b[31m✗\x1b[0m live line\n"); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if live.Raw == "" {
		t.Fatal("the appended line was never emitted")
	}
	if live.Raw != "✗ live line" {
		t.Errorf("ansi not stripped by the follow parser: %q", live.Raw)
	}
	if live.Time.IsZero() {
		t.Error("untimestamped raw line sorted to the epoch: the follow parser is not seeded")
	}
}

func TestTailReportsAnAbsentAgentLogsDirectoryWithoutFailing(t *testing.T) {
	dir := writeAgentLogs(t, nil)
	got := runTail(t, agentUUID)
	if !strings.Contains(got, "no agent logs") || !strings.Contains(got, dir) {
		t.Errorf("want a clean no-agent-logs notice naming %s, got %q", dir, got)
	}
}

func TestTailStillRejectsAnUnknownSourceWithNoAgentLogsDirectory(t *testing.T) {
	writeAgentLogs(t, nil)
	cmd := tailCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"daemon"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("want an unknown-source error")
	}
	for _, want := range []string{`unknown log source "daemon"`, "bossd", "agent-session id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %q", want, err.Error())
		}
	}
}

func TestTailRejectsASourceAlongsideAll(t *testing.T) {
	if _, _, err := resolveSources([]string{"bossd"}, true, ""); err == nil {
		t.Fatal("want an error for --all with a positional source")
	}
	sources, _, err := resolveSources(nil, true, "")
	if err != nil || len(sources) != len(bossalog.Services()) {
		t.Fatalf("--all must stay service-only: %+v, %v", sources, err)
	}
	for _, src := range sources {
		if src.Agent {
			t.Errorf("--all pulled in an agent log: %+v", src)
		}
	}
}

// Under -f the live path merges per source group. An agent source is keyed by
// its agent-session id, so its records must land in their own group rather than
// defaulting into the first one, which would reorder them against the services.
func TestWriteLiveRecordsInterleavesAgentAndServiceRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	records := make(chan logtail.Record, 3)
	errs := make(chan error)
	seed := time.Date(2026, 8, 5, 5, 0, 0, 0, time.UTC)
	agent := logtail.NewAgentParser(agentUUID, seed)
	records <- logtail.ParseLine("bossd", `{"time":"2026-08-05T05:00:03Z","message":"service last"}`)
	records <- agent.Parse(`{"ts":"2026-08-05T05:00:02Z","text":"agent middle"}`)
	records <- logtail.ParseLine("bossd", `{"time":"2026-08-05T05:00:01Z","message":"service first"}`)

	sources := append(serviceSources([]string{"bossd"}), tailSource{Name: agentUUID, Agent: true, Seed: seed})
	var seen []string
	err := writeLiveRecords(ctx, sources, records, errs, func(rec logtail.Record) error {
		text := rec.Message
		if !rec.Parsed {
			text = rec.Raw
		}
		seen = append(seen, text)
		if len(seen) == 3 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(seen, ","), "service first,agent middle,service last"; got != want {
		t.Fatalf("live agent/service merge: got %q, want %q", got, want)
	}
}
