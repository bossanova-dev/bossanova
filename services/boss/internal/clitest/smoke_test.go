package clitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

func TestCLI_Version(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("version")

	if res.ExitCode != 0 {
		t.Fatalf("boss version: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "boss") {
		t.Errorf("expected stdout to contain 'boss', got: %q", res.Stdout)
	}
}
