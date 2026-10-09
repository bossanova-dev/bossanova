package clitest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCLI_InitRegisterCredential(t *testing.T) {
	dir := t.TempDir()
	if b, err := exec.Command("git", "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, b)
	}
	h := clitest.New(t, clitest.WithEnv("LINEAR_API_KEY="))
	key := "clitest-linear-secret"
	res := h.RunWithStdin(key+"\n", "init", "--dir", dir, "--register", "--linear-key-stdin")
	if strings.Contains(res.Stdout, key) || strings.Contains(res.Stderr, key) {
		t.Fatal("credential leaked into CLI output")
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	registrations, updates := h.Daemon.RegisterRepoCalls(), h.Daemon.UpdateRepoCalls()
	if len(registrations) != 1 || len(updates) != 1 || updates[0].LinearKey.Action != pb.SecretAction_SECRET_ACTION_SET || updates[0].LinearKey.GetValue() != key {
		t.Fatal("missing credential registration requests")
	}
	if !strings.Contains(res.Stdout, "verified") {
		t.Fatal("credential was not verified")
	}
	if _, err := os.Stat(filepath.Join(dir, ".boss-skills.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCLI_InitSentryCredential(t *testing.T) {
	dir := t.TempDir()
	if b, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, b)
	}
	h := clitest.New(t, clitest.WithEnv("LINEAR_API_KEY="))
	token := "clitest-sentry-secret"
	res := h.RunWithStdin(token+"\n", "init", "--dir", dir, "--sentry-org", "example", "--sentry-token-stdin")
	if strings.Contains(res.Stdout, token) || strings.Contains(res.Stderr, token) {
		t.Fatal("token leaked")
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.UpdateRepoCalls()
	if len(calls) != 1 || calls[0].SentryKey.GetValue() != token || calls[0].GetSentryOrg() != "example" || !strings.Contains(res.Stdout, "verified") {
		t.Fatal("Sentry pair was not stored and verified")
	}
}

func TestCLI_InitConfigOnly(t *testing.T) {
	for _, mode := range []string{"default", "remote", "host"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			h := clitest.New(t, clitest.WithEnv("LINEAR_API_KEY="))
			args := []string{"init", "--dir", dir}
			if mode != "default" {
				args = append([]string{"--" + mode, "unreachable.example"}, args...)
				args = append(args, "--register", "--linear-key-stdin")
			}
			res := h.Run(args...)
			if res.ExitCode != 0 {
				t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
			}
			if len(h.Daemon.RegisterRepoCalls()) != 0 || len(h.Daemon.UpdateRepoCalls()) != 0 {
				t.Fatal("config-only init touched repository registrations")
			}
			if _, err := os.Stat(filepath.Join(dir, ".boss-skills.json")); err != nil {
				t.Fatal(err)
			}
			if mode != "default" && !strings.Contains(res.Stdout, "local path") {
				t.Fatal("missing remote explanation")
			}
		})
	}
}
