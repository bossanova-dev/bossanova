package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	zlog "github.com/rs/zerolog/log"
)

func TestSetup(t *testing.T) {
	// Point XDG_STATE_HOME at a temp dir so we don't touch the developer's real state dir.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	closer := Setup("test-service")
	t.Cleanup(func() { _ = closer.Close() })

	// Logger should be usable and should also write to the file.
	zlog.Info().Msg("test log after Setup")

	path := LogPath("test-service")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected log file at %s, got error: %v", path, err)
	}
	if !strings.Contains(string(data), "test log after Setup") {
		t.Errorf("log file %s did not contain expected entry; got: %s", path, data)
	}
}

func TestSetupFileOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	closer := SetupFileOnly("test-service-fileonly")
	t.Cleanup(func() { _ = closer.Close() })

	zlog.Info().Msg("file-only log entry")

	path := LogPath("test-service-fileonly")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected log file at %s, got error: %v", path, err)
	}
	if !strings.Contains(string(data), "file-only log entry") {
		t.Errorf("log file %s did not contain expected entry; got: %s", path, data)
	}
}

func TestLogPathXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
	got := LogPath("bossd")
	want := filepath.Join("/tmp/xdg", "bossanova", "logs", "bossd.log")
	if got != want {
		t.Errorf("LogPath: got %q, want %q", got, want)
	}
}

func TestLogPathFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	got := LogPath("boss")
	want := filepath.Join(home, ".local", "state", "bossanova", "logs", "boss.log")
	if got != want {
		t.Errorf("LogPath fallback: got %q, want %q", got, want)
	}
}

func TestAgentLogsDirSitsBesideTheWorktreeRoot(t *testing.T) {
	for _, tc := range []struct {
		name, base, wantDir, wantPath string
	}{
		{
			name:     "default worktree base",
			base:     filepath.Join("/home/dev", ".bossanova", "worktrees"),
			wantDir:  filepath.Join("/home/dev", ".bossanova", "agent-logs"),
			wantPath: filepath.Join("/home/dev", ".bossanova", "agent-logs", "sess-1.log"),
		},
		{
			name:     "trailing separator is not a second component",
			base:     filepath.Join("/srv", "wt") + string(filepath.Separator),
			wantDir:  filepath.Join("/srv", "agent-logs"),
			wantPath: filepath.Join("/srv", "agent-logs", "sess-1.log"),
		},
		{name: "unconfigured base yields no path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentLogsDir(tc.base); got != tc.wantDir {
				t.Errorf("AgentLogsDir(%q): got %q, want %q", tc.base, got, tc.wantDir)
			}
			if got := AgentLogPath(tc.base, "sess-1"); got != tc.wantPath {
				t.Errorf("AgentLogPath(%q): got %q, want %q", tc.base, got, tc.wantPath)
			}
		})
	}
}

func TestAgentLogFileNamesOneSessionInsideAResolvedDir(t *testing.T) {
	dir := filepath.Join("/home/dev", ".bossanova", "agent-logs")
	for _, tc := range []struct {
		name, dir, id, want string
	}{
		{
			name: "agent session id",
			dir:  dir,
			id:   "sess-1",
			want: filepath.Join(dir, "sess-1.log"),
		},
		{
			// A repair run composes its own prefix onto the boss session id
			// rather than reaching for a second naming helper.
			name: "repair run prefix composes",
			dir:  dir,
			id:   "repair-abc123",
			want: filepath.Join(dir, "repair-abc123.log"),
		},
		{
			// The callers all guard on an empty dir; returning "" rather than a
			// stray relative "sess-1.log" is what keeps those guards working.
			name: "unconfigured dir yields no path",
			id:   "sess-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentLogFile(tc.dir, tc.id); got != tc.want {
				t.Errorf("AgentLogFile(%q, %q): got %q, want %q", tc.dir, tc.id, got, tc.want)
			}
		})
	}
}
