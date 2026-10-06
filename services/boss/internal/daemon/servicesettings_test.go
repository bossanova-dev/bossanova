package daemon

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestMain isolates the package from the BOSS_SETTINGS_PATH of the shell that
// runs it. A developer (or agent) shell commonly exports one, and since
// BOS-1368 every service renderer bakes it, so an inherited value would make
// the default-profile assertions in this package depend on who ran the suite.
// CI runs with it unset; this makes every other run match. Tests that exercise
// a non-default profile opt in with t.Setenv.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(ServiceSettingsPathEnv)
	os.Exit(m.Run())
}

// stubSettingsHome points HOME (and the Linux XDG override) at a temp dir so
// defaultSettingsPath resolves inside the test, and returns that default path.
func stubSettingsHome(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	def, err := defaultSettingsPath()
	if err != nil {
		t.Fatalf("defaultSettingsPath: %v", err)
	}
	return def
}

// captureSettingsNotices records every noticeServiceSettingsPathChanged call.
func captureSettingsNotices(t *testing.T) *[][2]string {
	t.Helper()
	var got [][2]string
	previous := noticeServiceSettingsPathChanged
	noticeServiceSettingsPathChanged = func(installed, next string) {
		got = append(got, [2]string{installed, next})
	}
	t.Cleanup(func() { noticeServiceSettingsPathChanged = previous })
	return &got
}

func TestServiceSettingsPath(t *testing.T) {
	def := stubSettingsHome(t)

	cases := []struct {
		name    string
		env     string
		want    string
		wantErr string
	}{
		{name: "unset bakes nothing", env: "", want: ""},
		{name: "absolute path is baked", env: "/abs/x/settings.json", want: "/abs/x/settings.json"},
		{name: "absolute path is cleaned", env: "/abs/x/../y//settings.json", want: "/abs/y/settings.json"},
		{name: "a space is legal", env: "/abs/my profile/settings.json", want: "/abs/my profile/settings.json"},
		{name: "the OS default bakes nothing", env: def, want: ""},
		{name: "relative path is refused", env: "rel/settings.json", wantErr: "must be absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ServiceSettingsPathEnv, tc.env)
			got, err := serviceSettingsPath()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("serviceSettingsPath() error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("serviceSettingsPath(): %v", err)
			}
			if got != tc.want {
				t.Errorf("serviceSettingsPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestServiceSettingsPathKeepsAnXDGDerivedDefault: with XDG_CONFIG_HOME set,
// the "default" this shell computes is one no service definition reproduces
// (neither unit carries XDG_CONFIG_HOME), so an explicit value equal to it must
// still be baked rather than dropped.
func TestServiceSettingsPathKeepsAnXDGDerivedDefault(t *testing.T) {
	stubSettingsHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	want, err := defaultSettingsPath()
	if err != nil {
		t.Fatalf("defaultSettingsPath: %v", err)
	}
	t.Setenv(ServiceSettingsPathEnv, want)
	got, err := serviceSettingsPath()
	if err != nil {
		t.Fatalf("serviceSettingsPath(): %v", err)
	}
	if got != want {
		t.Errorf("serviceSettingsPath() = %q, want the explicit %q baked", got, want)
	}
}

// TestServiceSettingsPathRejectsHostileCharacters: text/template does not
// escape, so each of these would corrupt a plist or a quoted systemd
// Environment= line. The error must name the character so an operator can fix
// the path rather than guess.
func TestServiceSettingsPathRejectsHostileCharacters(t *testing.T) {
	stubSettingsHome(t)
	for _, char := range []string{"<", ">", "&", `"`, "'", "\n", "\r", `\`, "%"} {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(char, "\n", `\n`), "\r", `\r`), func(t *testing.T) {
			t.Setenv(ServiceSettingsPathEnv, "/abs/bad"+char+"dir/settings.json")
			_, err := serviceSettingsPath()
			if err == nil {
				t.Fatalf("serviceSettingsPath() accepted a path containing %q", char)
			}
			if !strings.Contains(err.Error(), ServiceSettingsPathEnv) {
				t.Errorf("error %q does not name %s", err, ServiceSettingsPathEnv)
			}
			if want := "contains " + strconv.Quote(char); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name the offending character (want %q)", err, want)
			}
		})
	}
}

func TestRewriteServiceSettingsPath(t *testing.T) {
	def := stubSettingsHome(t)

	cases := []struct {
		name       string
		env        string
		installed  string
		want       string
		wantNotice bool
		wantErr    string
	}{
		{name: "unset keeps the installed profile", env: "", installed: "/abs/a/settings.json", want: "/abs/a/settings.json"},
		{name: "unset keeps the installed default", env: "", installed: "", want: ""},
		{name: "set to the installed value is quiet", env: "/abs/a/settings.json", installed: "/abs/a/settings.json", want: "/abs/a/settings.json"},
		{name: "set to a different value wins and notices", env: "/abs/b/settings.json", installed: "/abs/a/settings.json", want: "/abs/b/settings.json", wantNotice: true},
		{name: "set over a default install wins and notices", env: "/abs/b/settings.json", installed: "", want: "/abs/b/settings.json", wantNotice: true},
		{name: "set to the OS default moves back to default", env: def, installed: "/abs/a/settings.json", want: "", wantNotice: true},
		{name: "a hostile installed value is refused, not re-baked", env: "", installed: "/abs/a&b/settings.json", wantErr: "contains"},
		{name: "an invalid explicit value is refused", env: "rel/settings.json", installed: "/abs/a/settings.json", wantErr: "must be absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notices := captureSettingsNotices(t)
			t.Setenv(ServiceSettingsPathEnv, tc.env)

			got, err := rewriteServiceSettingsPath(tc.installed)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("rewriteServiceSettingsPath() error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("rewriteServiceSettingsPath(): %v", err)
			}
			if got != tc.want {
				t.Errorf("rewriteServiceSettingsPath(%q) = %q, want %q", tc.installed, got, tc.want)
			}
			if gotNotice := len(*notices) > 0; gotNotice != tc.wantNotice {
				t.Errorf("notice printed = %v, want %v (%v)", gotNotice, tc.wantNotice, *notices)
			}
			if tc.wantNotice && (*notices)[0] != [2]string{tc.installed, tc.want} {
				t.Errorf("notice = %v, want installed %q -> next %q", (*notices)[0], tc.installed, tc.want)
			}
		})
	}
}

func TestDescribeServiceSettingsPath(t *testing.T) {
	if got := DescribeServiceSettingsPath(""); got != "default" {
		t.Errorf(`DescribeServiceSettingsPath("") = %q, want "default"`, got)
	}
	if got := DescribeServiceSettingsPath("/abs/x/settings.json"); got != "/abs/x/settings.json" {
		t.Errorf("DescribeServiceSettingsPath(path) = %q, want the path", got)
	}
}

// TestInstallRefusesAnUnbakeableSettingsPathBeforeWritingAnything covers both
// install entry points: the refusal must come before any platform path stages a
// binary or writes a service file, so the HOME tree stays empty.
func TestInstallRefusesAnUnbakeableSettingsPathBeforeWritingAnything(t *testing.T) {
	for _, env := range []string{"rel/settings.json", "/abs/a<b/settings.json"} {
		t.Run(env, func(t *testing.T) {
			stubSettingsHome(t)
			home := os.Getenv("HOME")
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
			t.Setenv(ServiceSettingsPathEnv, env)

			if err := Install("/usr/local/bin/bossd", true); err == nil || !strings.Contains(err.Error(), ServiceSettingsPathEnv) {
				t.Errorf("Install error = %v, want a %s refusal", err, ServiceSettingsPathEnv)
			}
			if err := McpInstall("/usr/local/bin/mcp", DefaultMcpPort, true); err == nil || !strings.Contains(err.Error(), ServiceSettingsPathEnv) {
				t.Errorf("McpInstall error = %v, want a %s refusal", err, ServiceSettingsPathEnv)
			}
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatalf("read HOME: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("a refused install wrote into HOME: %v", entries)
			}
		})
	}
}
