package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/config"
)

// stubHomeDir is the deterministic home every service-path test expands against.
const stubHomeDir = "/stub/home"

// stubHome points the ~ expansion at stubHomeDir for the duration of the test.
func stubHome(t *testing.T) {
	t.Helper()
	previous := userHomeDir
	userHomeDir = func() (string, error) { return stubHomeDir, nil }
	t.Cleanup(func() { userHomeDir = previous })
}

func TestPathExtrasExpandsTilde(t *testing.T) {
	stubHome(t)

	got := pathExtras(config.Settings{DaemonPathExtra: []string{"~/.asdf/shims", "~/bin"}})

	want := []string{"/stub/home/.asdf/shims", "/stub/home/bin"}
	if len(got) != len(want) {
		t.Fatalf("pathExtras = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pathExtras[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPathExtrasPreservesDeclaredOrder(t *testing.T) {
	stubHome(t)

	got := pathExtras(config.Settings{DaemonPathExtra: []string{"/one", "/two", "/three"}})

	want := []string{"/one", "/two", "/three"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("pathExtras = %v, want %v", got, want)
		}
	}
}

func TestPathExtrasRejectsHostileAndUnusableEntries(t *testing.T) {
	stubHome(t)

	cases := []struct {
		name  string
		entry string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"relative", "relative/bin"},
		{"bare relative", "bin"},
		{"dot relative", "./bin"},
		{"parent relative", "../bin"},
		{"colon bearing", "/opt/a:/opt/b"},
		{"newline bearing", "/opt/a\nEnvironment=EVIL=1"},
		{"carriage return bearing", "/opt/a\rEnvironment=EVIL=1"},
		{"xml ampersand", "/opt/a&b"},
		{"xml less than", "/opt/a<b"},
		{"xml greater than", "/opt/a>b"},
		{"xml double quote", `/opt/a"b`},
		{"tilde only", "~"},
		{"tilde other user", "~someone/bin"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pathExtras(config.Settings{DaemonPathExtra: []string{tc.entry}})
			if len(got) != 0 {
				t.Errorf("pathExtras(%q) = %v, want no entries", tc.entry, got)
			}
		})
	}
}

func TestPathExtrasKeepsGoodEntriesAlongsideRejected(t *testing.T) {
	stubHome(t)

	got := pathExtras(config.Settings{DaemonPathExtra: []string{
		"relative/bin",
		"/good/one",
		"/opt/a&b",
		"~/.nodenv/shims",
	}})

	want := []string{"/good/one", "/stub/home/.nodenv/shims"}
	if strings.Join(got, ":") != strings.Join(want, ":") {
		t.Fatalf("pathExtras = %v, want %v", got, want)
	}
}

func TestPathExtrasDeduplicates(t *testing.T) {
	stubHome(t)

	got := pathExtras(config.Settings{DaemonPathExtra: []string{
		"/dup",
		"/other",
		"/dup/",
		"~/.asdf/shims",
		"/stub/home/.asdf/shims",
	}})

	want := []string{"/dup", "/other", "/stub/home/.asdf/shims"}
	if strings.Join(got, ":") != strings.Join(want, ":") {
		t.Fatalf("pathExtras = %v, want %v", got, want)
	}
}

func TestPathExtrasTrimsSurroundingWhitespace(t *testing.T) {
	stubHome(t)

	got := pathExtras(config.Settings{DaemonPathExtra: []string{"  /padded/bin  "}})

	if strings.Join(got, ":") != "/padded/bin" {
		t.Fatalf("pathExtras = %v, want [/padded/bin]", got)
	}
}

func TestPathExtrasEmptyWhenUnset(t *testing.T) {
	stubHome(t)

	if got := pathExtras(config.Settings{}); len(got) != 0 {
		t.Fatalf("pathExtras = %v, want no entries", got)
	}
}

func TestPathExtrasDropsAllWhenHomeUnavailable(t *testing.T) {
	previous := userHomeDir
	userHomeDir = func() (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { userHomeDir = previous })

	got := pathExtras(config.Settings{DaemonPathExtra: []string{"~/.asdf/shims", "/absolute/bin"}})

	// A ~-rooted entry is unresolvable without a home directory and is dropped;
	// an absolute entry is unaffected.
	if strings.Join(got, ":") != "/absolute/bin" {
		t.Fatalf("pathExtras = %v, want [/absolute/bin]", got)
	}
}

func TestJoinServicePathPrependsExtrasAndDedupes(t *testing.T) {
	got := joinServicePath(
		[]string{"/extra/one", "/usr/bin"},
		[]string{"/usr/local/bin", "/usr/bin", "/bin"},
	)

	want := "/extra/one:/usr/bin:/usr/local/bin:/bin"
	if got != want {
		t.Fatalf("joinServicePath = %q, want %q", got, want)
	}
}

func TestJoinServicePathWithoutExtrasIsBaseline(t *testing.T) {
	got := joinServicePath(nil, []string{"/usr/local/bin", "/usr/bin"})

	if got != "/usr/local/bin:/usr/bin" {
		t.Fatalf("joinServicePath = %q, want the baseline unchanged", got)
	}
}

func TestServiceEnvPathIncludesShimDirectories(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{})

	got := ServiceEnvPath()

	for _, want := range []string{"/.nodenv/shims", "/.local/bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("ServiceEnvPath() = %q, want it to contain %q", got, want)
		}
	}
}

func TestServiceEnvPathPlacesConfiguredExtrasFirst(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{"~/.asdf/shims"}})

	got := ServiceEnvPath()

	if !strings.HasPrefix(got, "/stub/home/.asdf/shims:") {
		t.Fatalf("ServiceEnvPath() = %q, want it to start with the configured extra", got)
	}
}

func TestServiceEnvPathOmitsHostileEntries(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{DaemonPathExtra: []string{
		"/opt/evil\nEnvironment=EVIL=1",
		"/opt/a&b",
	}})

	got := ServiceEnvPath()

	if strings.ContainsAny(got, "\n\r&<>\"") {
		t.Fatalf("ServiceEnvPath() = %q, want no hostile characters", got)
	}
}

// stubServiceSettings injects the settings the service-path helpers read, so a
// test never depends on the developer's real settings.json.
func stubServiceSettings(t *testing.T, settings config.Settings) {
	t.Helper()
	previous := loadServiceSettings
	loadServiceSettings = func() (config.Settings, error) { return settings, nil }
	t.Cleanup(func() { loadServiceSettings = previous })
}

func TestServiceEnvPathFallsBackWhenSettingsUnreadable(t *testing.T) {
	stubHome(t)
	previous := loadServiceSettings
	loadServiceSettings = func() (config.Settings, error) {
		return config.Settings{}, os.ErrPermission
	}
	t.Cleanup(func() { loadServiceSettings = previous })

	got := ServiceEnvPath()

	// An unreadable settings file must never yield an empty PATH: the baseline
	// is what keeps the daemon able to run git.
	if got == "" {
		t.Fatal("ServiceEnvPath() = \"\", want the baseline PATH")
	}
	if !strings.Contains(got, "/usr/bin") {
		t.Fatalf("ServiceEnvPath() = %q, want it to contain /usr/bin", got)
	}
}

func TestLookPathInFindsExecutable(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "node")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}

	got, ok := LookPathIn(dir+":/nonexistent", "node")
	if !ok {
		t.Fatal("LookPathIn() ok = false, want true")
	}
	if got != binary {
		t.Errorf("LookPathIn() = %q, want %q", got, binary)
	}
}

func TestLookPathInIgnoresNonExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node"), []byte("text"), 0o644); err != nil {
		t.Fatalf("write stub file: %v", err)
	}

	if _, ok := LookPathIn(dir, "node"); ok {
		t.Error("LookPathIn() ok = true for a non-executable file, want false")
	}
}

func TestLookPathInIgnoresDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "node"), 0o755); err != nil {
		t.Fatalf("mkdir stub: %v", err)
	}

	if _, ok := LookPathIn(dir, "node"); ok {
		t.Error("LookPathIn() ok = true for a directory, want false")
	}
}

func TestLookPathInMissing(t *testing.T) {
	if _, ok := LookPathIn(t.TempDir(), "definitely-not-here"); ok {
		t.Error("LookPathIn() ok = true, want false")
	}
}
