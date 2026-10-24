package attach

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/attachprocess"
	"github.com/recurser/boss/internal/pty"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

type stubClient struct{}

func (*stubClient) GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error) {
	return &pb.Session{Id: "session-1"}, nil
}
func (*stubClient) ListChats(context.Context, string) ([]*pb.AgentChat, error) {
	panic("unexpected ListChats")
}
func (*stubClient) RecordChat(context.Context, string, string, string, string, pb.ChatRole, bool) (*pb.AgentChat, error) {
	panic("unexpected RecordChat")
}
func (*stubClient) UpdateChatTitle(context.Context, string, string) error {
	panic("unexpected UpdateChatTitle")
}
func (*stubClient) DeleteChat(context.Context, string, pb.DeleteChatRequest_DeletionReason) error {
	panic("unexpected DeleteChat")
}
func (*stubClient) ReportChatStatus(context.Context, []*pb.ChatStatusReport) error {
	panic("unexpected ReportChatStatus")
}
func (*stubClient) DescribeChatLaunch(context.Context, string) (*pb.DescribeChatLaunchResponse, error) {
	panic("unexpected DescribeChatLaunch")
}
func NewAttachModel(c Client, ctx context.Context, manager *pty.Manager, sessionID, resumeID string) Model {
	return NewModel(c, ctx, manager, sessionID, resumeID, attachprocess.OS{})
}

func withHostDestination(t *testing.T, destination string) {
	t.Helper()
	previous := ui.RemoteHostDestination()
	t.Cleanup(func() { ui.SetHostDestination(previous) })
	ui.SetHostDestination(destination)
}

func withHostTunnelReason(t *testing.T, reason string) {
	t.Helper()
	previous := ui.SetHostTunnelReason(nil)
	t.Cleanup(func() { ui.SetHostTunnelReason(previous) })
	if reason == "" {
		ui.SetHostTunnelReason(nil)
		return
	}
	ui.SetHostTunnelReason(func() string { return reason })
}

// errForwardedSocketDial is the failure a --host RPC actually returns once the
// ssh tunnel is down: every request dials the forwarded unix socket, so the
// error names a local temp path that does not exist. This exact string reached a
// user verbatim, which is what BOS-724 was filed for.
var errForwardedSocketDial = errors.New(
	"unavailable: dial unix /var/folders/x/T/boss-host-4081179615/bossd.sock: connect: no such file or directory")

func normalizedRender(rendered string) string {
	return strings.Join(strings.Fields(stripANSI(rendered)), " ")
}

// assertNoForwardedSocketDial is the shared absence guard for the three views:
// none of them may show the raw dial error, the forwarded socket path, or the
// temp directory it lives in.
func assertNoForwardedSocketDial(t *testing.T, what, rendered string) {
	t.Helper()
	for _, forbidden := range []string{"dial unix", "bossd.sock", "boss-host-"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("%s %q must never show %q", what, rendered, forbidden)
		}
	}
}

func stripANSI(s string) string { return ansi.Strip(s) }

type fakeTelemetry struct {
	events      []telemetry.Event
	distinctIDs []string
	props       []map[string]any
}

func (f *fakeTelemetry) Capture(_ context.Context, event telemetry.Event, distinctID string, props map[string]any) {
	f.events = append(f.events, event)
	f.distinctIDs = append(f.distinctIDs, distinctID)
	f.props = append(f.props, props)
}

func (f *fakeTelemetry) Identify(context.Context, string, map[string]any) {}
func (f *fakeTelemetry) Alias(context.Context, string, string)            {}
func (f *fakeTelemetry) Close()                                           {}

func assertNoSensitiveTelemetryProps(t *testing.T, props map[string]any) {
	t.Helper()
	for _, key := range []string{"args", "prompt", "transcript", "repo_path", "branch", "path", "file_path", "comment", "email"} {
		if _, ok := props[key]; ok {
			t.Fatalf("sensitive prop %q present in %v", key, props)
		}
	}
}

func enableViewTelemetryForTest(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatal(err)
	}
	ui.ResetTelemetryCache()
	t.Cleanup(ui.ResetTelemetryCache)
}

func withHostControlPath(t *testing.T, controlPath string) {
	t.Helper()
	previous := ui.RemoteHostControlPath()
	t.Cleanup(func() { ui.SetHostControlPath(previous) })
	ui.SetHostControlPath(controlPath)
}
