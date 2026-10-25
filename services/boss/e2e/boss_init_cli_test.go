package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	"github.com/recurser/boss/internal/realstack"
)

func TestJourney_BossInit_AC1_InitConfigOnly(t *testing.T) {
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
			if !strings.Contains(res.Stdout, ".boss-skills.json") {
				t.Fatalf("init did not name its config: %s", res.Stdout)
			}
			if len(h.Repos(t)) != 0 {
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

func TestJourney_BossInit_AC4_InitRegisterCredential(t *testing.T) {
	t.Run("linear", func(t *testing.T) {
		h := clitest.New(t, clitest.WithEnv("LINEAR_API_KEY="))
		dir := realstack.InitRepo(t, h.Stack.Home(), "init-project")
		key := "clitest-linear-secret"
		res := h.RunWithStdin(key+"\n", "init", "--dir", dir, "--register", "--linear-key-stdin")
		if strings.Contains(res.Stdout, key) || strings.Contains(res.Stderr, key) {
			t.Fatal("credential leaked into CLI output")
		}
		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
		}
		repos := h.Repos(t)
		if len(repos) != 1 || repos[0].GetLinearApiKey() != key {
			t.Fatal("repository credential was not stored")
		}
		if !strings.Contains(res.Stdout, "verified") {
			t.Fatal("credential was not verified")
		}
		if _, err := os.Stat(filepath.Join(dir, ".boss-skills.json")); err != nil {
			t.Fatal(err)
		}

	})
	t.Run("sentry", func(t *testing.T) {
		h := clitest.New(t, clitest.WithEnv("LINEAR_API_KEY="))
		dir := realstack.InitRepo(t, h.Stack.Home(), "sentry-project")
		token := "clitest-sentry-secret"
		res := h.RunWithStdin(token+"\n", "init", "--dir", dir, "--sentry-org", "example", "--sentry-token-stdin")
		if strings.Contains(res.Stdout, token) || strings.Contains(res.Stderr, token) {
			t.Fatal("token leaked")
		}
		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
		}
		repos := h.Repos(t)
		if len(repos) != 1 || repos[0].GetSentryApiKey() != token || repos[0].GetSentryOrg() != "example" {
			t.Fatal("Sentry credential pair was not stored")
		}
		if !strings.Contains(res.Stdout, "verified") {
			t.Fatal("Sentry credential pair was not verified")
		}

	})
}
