package clitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

func TestCLI_Show_ValidID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("show", "sess-aaa-111")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{
		"Add dark mode",
		"boss/add-dark-mode",
		"my-app",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n---\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Show_UnknownID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("show", "nosuchsession")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for unknown session, got 0\nstdout=%q", res.Stdout)
	}
	if res.Stderr == "" {
		t.Errorf("expected error on stderr, got empty")
	}
}
