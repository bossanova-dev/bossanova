package account

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestResolveAgentBinServiceFallback(t *testing.T) {
	caller, service := t.TempDir(), t.TempDir()
	t.Setenv("PATH", caller)
	for _, name := range []string{"codex", "claude"} {
		path := filepath.Join(service, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		g := &group{servicePath: func() string { return service }}
		cmd := &cobra.Command{}
		cmd.Flags().String("codex-bin", "", "")
		got, err := g.resolveAgentBin(cmd, name, "", "boss account add "+name)
		if err != nil || got.Path != path || len(got.Env) != 1 || got.Env[0] != "PATH="+caller+":"+service {
			t.Fatalf("%s: got=%+v err=%v", name, got, err)
		}
	}
}

func TestResolveAgentBinExplicitAndMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	g := &group{servicePath: func() string { t.Fatal("explicit must not read service PATH"); return "" }}
	for _, verb := range []string{"reauth", "add"} {
		account := New(nil, nil, nil, nil, nil)
		cmd, _, err := account.Find([]string{verb})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "chosen")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Flags().Set("codex-bin", path); err != nil {
			t.Fatal(err)
		}
		got, err := g.resolveAgentBin(cmd, "codex", "--codex-bin", "boss account "+verb)
		if err != nil || got.Path != path || len(got.Env) != 0 {
			t.Fatalf("got=%+v err=%v", got, err)
		}
		if err := cmd.Flags().Set("codex-bin", path+"-missing"); err != nil {
			t.Fatal(err)
		}
		if _, err := g.resolveAgentBin(cmd, "codex", "--codex-bin", "boss account "+verb); err == nil || !strings.Contains(err.Error(), path+"-missing") {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestClaudeRefusesCodexBinBeforeDial(t *testing.T) {
	cmd := New(func(*cobra.Command) (Client, error) { t.Fatal("unexpected daemon dial"); return nil, nil }, nil, func(*cobra.Command) string { return "" }, func(*cobra.Command) string { return "" }, nil)
	cmd.SetArgs([]string{"add", "claude", "--codex-bin", "/chosen/codex", "--token-stdin"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "codex only") {
		t.Fatalf("err=%v", err)
	}
}

func TestClaudeCredentialFlagsStillRefuseCodexBin(t *testing.T) {
	cmd := New(func(*cobra.Command) (Client, error) { t.Fatal("unexpected daemon dial"); return nil, nil }, nil, nil, nil, nil)
	cmd.SetArgs([]string{"add", "claude", "--codex-bin", "/chosen/codex", "--token", "unused", "--label", "example"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "codex only") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveAgentBinEmptyFlagDoesNotSearch(t *testing.T) {
	for _, verb := range []string{"add", "reauth"} {
		cmd, _, err := New(nil, nil, nil, nil, nil).Find([]string{verb})
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Flags().Set("codex-bin", ""); err != nil {
			t.Fatal(err)
		}
		g := &group{servicePath: func() string { t.Fatal("empty explicit must not read service PATH"); return "" }}
		if _, err := g.resolveAgentBin(cmd, "codex", "--codex-bin", "codex"); err == nil || !strings.Contains(err.Error(), `--codex-bin ""`) {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestMissingAgentBinRemediesPreserveShellArguments(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "boss"), []byte("#!/bin/sh\nprintf '%s\\0' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		verb, name, flag, arg string
		flags, want           []string
	}{
		{"add", "codex", "--codex-bin", "codex", []string{"--label", "work'; $(printf injected); '", "--priority", "2", "--timeout", "20m"}, []string{"account", "add", "codex", "--label=work'; $(printf injected); '", "--priority=2", "--timeout=20m0s"}},
		{"add", "claude", "", "claude", []string{"--label", "team account", "--priority", "3"}, []string{"account", "add", "claude", "--label=team account", "--priority=3"}},
		{"reauth", "codex", "--codex-bin", "account '$(printf injected)'", []string{"--timeout", "20m"}, []string{"account", "reauth", "account '$(printf injected)'", "--timeout=20m0s"}},
	} {
		t.Run(tc.verb+tc.name, func(t *testing.T) {
			cmd, _, err := New(nil, nil, nil, nil, nil).Find([]string{tc.verb})
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.ParseFlags(tc.flags); err != nil {
				t.Fatal(err)
			}
			_, err = (&group{}).resolveAgentBin(cmd, tc.name, tc.flag, tc.arg)
			if err == nil {
				t.Fatal("expected missing binary")
			}
			var remedy string
			for _, line := range strings.Split(err.Error(), "\n") {
				if strings.HasPrefix(line, "  env PATH=") {
					remedy = strings.TrimSpace(line)
				}
			}
			if remedy == "" {
				t.Fatalf("missing env remedy: %v", err)
			}
			child := exec.Command("/bin/sh", "-c", "env() { /usr/bin/env \"$@\"; }; "+remedy)
			child.WaitDelay = time.Second
			output, runErr := child.Output()
			if runErr != nil {
				t.Fatalf("remedy failed: %v (%s)", runErr, output)
			}
			got := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestMissingAgentBinRemedyOmitsCredentials(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cmd, _, err := New(nil, nil, nil, nil, nil).Find([]string{"add"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags([]string{"--token", "do-not-print-secret", "--credential-file", "private-credential-file"}); err != nil {
		t.Fatal(err)
	}
	_, err = (&group{}).resolveAgentBin(cmd, "codex", "--codex-bin", "codex")
	if err == nil || strings.Contains(err.Error(), "do-not-print-secret") || strings.Contains(err.Error(), "private-credential-file") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveAgentBinRelativePathsKeepErrDot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "codex"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, caller := range []string{"bin", t.TempDir()} {
		t.Setenv("PATH", caller)
		cmd, _, err := New(nil, nil, nil, nil, nil).Find([]string{"add"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := (&group{servicePath: func() string { return "bin" }}).resolveAgentBin(cmd, "codex", "--codex-bin", "codex")
		if !errors.Is(err, exec.ErrDot) || got.Path != "" {
			t.Fatalf("caller=%q got=%+v err=%v", caller, got, err)
		}
	}
}
