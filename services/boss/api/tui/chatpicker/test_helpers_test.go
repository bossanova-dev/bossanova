package chatpicker

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/recurser/bossalib/config"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, child := range batch {
			out = append(out, drainCmd(child)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}
func keyPress(ch rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: ch, Text: string(ch)}
}
func pasteText(text string) tea.PasteMsg {
	return tea.PasteMsg{Content: text}
}
func specialKeyPress(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}
func endpointSession(id string, ports ...uint32) *pb.Session {
	sess := &pb.Session{Id: id, Title: "Session " + id}
	for _, p := range ports {
		sess.HttpEndpoints = append(sess.HttpEndpoints, &pb.HTTPEndpoint{
			Port: p,
			Url:  fmt.Sprintf("http://localhost:%d", p),
		})
	}
	return sess
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
func normalizedRender(rendered string) string {
	return strings.Join(strings.Fields(stripANSI(rendered)), " ")
}
func assertNoForwardedSocketDial(t *testing.T, what, rendered string) {
	t.Helper()
	for _, forbidden := range []string{"dial unix", "bossd.sock", "boss-host-"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("%s %q must never show %q", what, rendered, forbidden)
		}
	}
}

func stripANSI(s string) string         { return ansi.Strip(s) }
func visibleRowText(cell string) string { return ansi.Strip(cell) }

func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

var errForwardedSocketDial = rpcerr.New(rpcerr.Unavailable, errors.New("dial unix /var/folders/tmp/bossd.sock: connect: connection refused"))

func listedChats(n int) ChatsListedMsg {
	msg := ChatsListedMsg{DaemonStatuses: map[string]string{}}
	for i := range n {
		id := "agent-" + strings.Repeat("x", i)
		msg.Chats = append(msg.Chats, &pb.AgentChat{SessionId: "session-1", AgentSessionId: id, Title: id, CreatedAt: timestamppb.Now()})
		msg.DaemonStatuses[id] = ui.StatusStopped
	}
	return msg
}
func assertOneBlankLineAbove(t *testing.T, screen, want, rowMarker string) {
	t.Helper()
	lines := strings.Split(ansi.Strip(screen), "\n")
	at := -1
	for i, line := range lines {
		if strings.Contains(line, want) {
			at = i
			break
		}
	}
	if at < 2 {
		t.Fatalf("%q at line %d of the screen, want a table row and a blank line above it:\n%s", want, at, strings.Join(lines, "\n"))
	}
	if strings.TrimSpace(lines[at-1]) != "" {
		t.Fatalf("line above %q is %q, want a blank line:\n%s", want, lines[at-1], strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[at-2], rowMarker) {
		t.Fatalf("two lines above %q is %q, want the last table row (%q), i.e. exactly one blank line:\n%s", want, lines[at-2], rowMarker, strings.Join(lines, "\n"))
	}
}
func runAttachCmdGraph(t *testing.T, cmd tea.Cmd, handle func(tea.Msg)) {
	t.Helper()
	if cmd == nil {
		return
	}
	runAttachMsgGraph(t, cmd(), handle)
}
func runAttachMsgGraph(t *testing.T, msg tea.Msg, handle func(tea.Msg)) {
	t.Helper()
	switch msg := msg.(type) {
	case nil:
		return
	case tea.BatchMsg:
		for _, cmd := range msg {
			runAttachCmdGraph(t, cmd, handle)
		}
	default:
		v := reflect.ValueOf(msg)
		if v.IsValid() && v.Kind() == reflect.Slice && strings.Contains(v.Type().String(), "sequenceMsg") {
			for i := 0; i < v.Len(); i++ {
				cmd, ok := v.Index(i).Interface().(tea.Cmd)
				if !ok {
					t.Fatalf("sequence element %d is %T, want tea.Cmd", i, v.Index(i).Interface())
				}
				runAttachCmdGraph(t, cmd, handle)
			}
			return
		}
		handle(msg)
	}
}
func withTranscriptSpy(t *testing.T) *transcriptSpy {
	t.Helper()
	spy := &transcriptSpy{}
	previousTitle, previousAbsence := agentChatTitle, agentTranscriptAbsentOrEmpty
	t.Cleanup(func() {
		agentChatTitle, agentTranscriptAbsentOrEmpty = previousTitle, previousAbsence
	})
	agentChatTitle = func(string, string) string {
		spy.titleCalls++
		return "a parsed title"
	}
	agentTranscriptAbsentOrEmpty = func(string, string) bool {
		spy.absenceCalls++
		return false
	}
	return spy
}
func reflowStatusBlock(block string) string {
	return strings.Join(strings.Fields(stripANSI(block)), " ")
}
func trimLineRightSpace(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}
func renderedColumnOf(t *testing.T, rendered, needle string) int {
	t.Helper()
	for _, line := range strings.Split(stripANSI(rendered), "\n") {
		if idx := strings.Index(line, needle); idx >= 0 {
			return lipgloss.Width(line[:idx])
		}
	}
	t.Fatalf("needle %q not found in rendered block:\n%s", needle, rendered)
	return -1
}
func firstNonSpaceColumn(t *testing.T, rendered string) int {
	t.Helper()
	for _, line := range strings.Split(stripANSI(rendered), "\n") {
		if idx := strings.IndexFunc(line, func(r rune) bool { return r != ' ' }); idx >= 0 {
			return lipgloss.Width(line[:idx])
		}
	}
	t.Fatalf("rendered block has no non-space content:\n%s", rendered)
	return -1
}
func rotationSession(evs ...*pb.RotationEvent) *pb.Session {
	return &pb.Session{RotationEvents: evs}
}
func withTempConfigHome(t *testing.T) {
	t.Helper()
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	if runtime.GOOS != "darwin" {
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempHome, ".config"))
	}
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(tempHome, "settings.json"))
	// viewDistinctID resolves the signed-in email through the keychain, and on
	// darwin the system Keychain is per-user rather than per-HOME, so the lines
	// above do not redirect it. Pin the file backend the way
	// enableCommandTelemetryForTest does, or these tests read the developer's
	// real login keychain and can raise an authorization prompt that hangs a
	// non-interactive run.
	t.Setenv("BOSS_KEYRING_BACKEND", "file")
}
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
	withTempConfigHome(t)
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatal(err)
	}
	ui.ResetTelemetryCache()
	t.Cleanup(ui.ResetTelemetryCache)
}

type transcriptSpy struct{ titleCalls, absenceCalls int }

const testWaitingReason = "awaiting ready_for_review on acme/widget#123"
const renderedHead = "0123456789abcdef0123456789abcdef01234567"
