package chatpicker

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/boss/internal/agent"

	"github.com/recurser/boss/api/tui/ui"
)

var openURLFunc = func(string) error { return nil }

func hasNewTabSupport() bool { return os.Getenv("TMUX") != "" }
func newTestModel(c Client, ctx context.Context, id, highlight string) Model {
	return NewModel(c, ctx, id, highlight, ExternalActions{ChatTitle: func(path, id string) string { return agentChatTitle(path, id) }, SupportsNewTab: hasNewTabSupport(), OpenNewTab: func(string) error { return nil }, OpenURL: func(url string) error { return openURLFunc(url) }})
}
func withHostDestination(t *testing.T, host string) {
	t.Helper()
	old := ui.RemoteHostDestination()
	ui.SetHostDestination(host)
	t.Cleanup(func() { ui.SetHostDestination(old) })
}

var agentChatTitle = agent.ChatTitle
var agentTranscriptAbsentOrEmpty = func(string, string) bool { return false }
