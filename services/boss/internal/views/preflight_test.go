package views

import (
	"regexp"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/preflight"
)

func TestPreflightViewDaemonWaitAddsBlankLineBeforeWaiting(t *testing.T) {
	model := PreflightModel{
		issue: preflight.Issue{
			Title:  "Cannot connect to the bossd daemon",
			Detail: "daemon failed\nTry one of:\n\n  bossd",
		},
		retryCheck: func() bool { return false },
	}

	rendered := trimLineRightSpace(stripANSI(model.View().Content))
	if !strings.Contains(rendered, "  bossd\n\n  Waiting for the daemon") {
		t.Fatalf("daemon wait view missing blank line before waiting copy:\n%s", rendered)
	}
}

func stripANSI(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}

func trimLineRightSpace(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}
