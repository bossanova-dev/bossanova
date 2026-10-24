package logtail

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustTime(t *testing.T, stamp string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatalf("parse %q: %v", stamp, err)
	}
	return at
}

// The seed stands in for the log file's modification time.
var agentSeed = time.Date(2026, 8, 5, 4, 59, 0, 0, time.UTC)

func TestAgentParserReadsBothFormats(t *testing.T) {
	first := mustTime(t, "2026-08-05T05:00:01Z")
	second := mustTime(t, "2026-08-05T05:00:02Z")

	for _, tc := range []struct {
		name     string
		lines    []string
		wantRaw  []string
		wantTime []time.Time
	}{
		{
			// A headless run: stdout, stderr and [runner] diagnostics all share
			// one writer, so they arrive in the same shape with no stream tag.
			name: "headless ndjson with runner diagnostics and stderr",
			lines: []string{
				`{"ts":"2026-08-05T05:00:01Z","text":"[runner] spawning claude"}`,
				`{"ts":"2026-08-05T05:00:02Z","text":"thinking about the task"}`,
				`{"ts":"2026-08-05T05:00:02Z","text":"error: rate limited"}`,
			},
			wantRaw:  []string{"[runner] spawning claude", "thinking about the task", "error: rate limited"},
			wantTime: []time.Time{first, second, second},
		},
		{
			// The runner's own encode-failure fallback writes an empty ts.
			name: "ndjson entry without a usable stamp carries the previous one",
			lines: []string{
				`{"ts":"2026-08-05T05:00:01Z","text":"first"}`,
				`{"ts":"","text":"<encode error>"}`,
			},
			wantRaw:  []string{"first", "<encode error>"},
			wantTime: []time.Time{first, first},
		},
		{
			name: "malformed line in an ndjson log degrades to raw, never dropped",
			lines: []string{
				`{"ts":"2026-08-05T05:00:01Z","text":"first"}`,
				`{"ts":"2026-08-05T05:00:02Z","text":`,
			},
			wantRaw:  []string{"first", `{"ts":"2026-08-05T05:00:02Z","text":`},
			wantTime: []time.Time{first, first},
		},
		{
			// An interactive chat: a raw tmux pipe-pane capture.
			name: "raw pipe-pane capture is stripped of ansi and anchored to the seed",
			lines: []string{
				"\x1b[32m✓\x1b[0m build passed\r",
				"\x1b]0;claude\x07\x1b[1mnext step\x1b[0m",
			},
			wantRaw:  []string{"✓ build passed", "next step"},
			wantTime: []time.Time{agentSeed, agentSeed},
		},
		{
			// Format is decided per file, so a JSON-looking line printed by an
			// agent into a raw pane stays verbatim rather than being decoded.
			name: "format detection sticks to the first non-empty line",
			lines: []string{
				"",
				"plain pane output",
				`{"ts":"2026-08-05T05:00:01Z","text":"not an entry"}`,
			},
			wantRaw:  []string{"", "plain pane output", `{"ts":"2026-08-05T05:00:01Z","text":"not an entry"}`},
			wantTime: []time.Time{agentSeed, agentSeed, agentSeed},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parser := NewAgentParser("agent-1", agentSeed)
			for index, line := range tc.lines {
				rec := parser.Parse(line)
				if rec.Raw != tc.wantRaw[index] {
					t.Errorf("line %d raw: got %q, want %q", index, rec.Raw, tc.wantRaw[index])
				}
				if !rec.Time.Equal(tc.wantTime[index]) {
					t.Errorf("line %d time: got %s, want %s", index, rec.Time, tc.wantTime[index])
				}
				if rec.Time.IsZero() {
					t.Errorf("line %d sorted to the epoch", index)
				}
				if rec.Parsed {
					t.Errorf("line %d marked parsed; agent output must stay filter-exempt", index)
				}
				if rec.Service != "agent-1" {
					t.Errorf("line %d service: got %q", index, rec.Service)
				}
			}
		})
	}
}

func TestAgentRecordsAlwaysPassFilters(t *testing.T) {
	parser := NewAgentParser("agent-1", agentSeed)
	rec := parser.Parse("some agent output")
	if !(Filter{Level: "error", Repo: "acme/core", Plugin: "claude"}).Match(rec) {
		t.Fatal("agent output was hidden by a filter")
	}
}

func TestAgentSeedTimeUsesModTimeAndNeverReturnsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.log")
	if err := os.WriteFile(path, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := AgentSeedTime(path); !got.Equal(info.ModTime()) {
		t.Errorf("existing file: got %s, want mod time %s", got, info.ModTime())
	}
	if got := AgentSeedTime(filepath.Join(t.TempDir(), "absent.log")); got.IsZero() {
		t.Error("absent file seeded the epoch")
	}
}
