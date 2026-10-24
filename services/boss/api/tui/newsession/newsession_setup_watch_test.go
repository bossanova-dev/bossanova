package newsession

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/domain/rpc"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type watchClient struct {
	stubClient
	stream rpc.WatchSessionSetupStream
	err    error
}

func (c *watchClient) WatchSessionSetup(context.Context, string) (rpc.WatchSessionSetupStream, error) {
	return c.stream, c.err
}

type watchStream struct {
	frames []*pb.WatchSessionSetupResponse
	index  int
	closed bool
}

func (s *watchStream) Receive() bool                      { s.index++; return s.index <= len(s.frames) }
func (s *watchStream) Msg() *pb.WatchSessionSetupResponse { return s.frames[s.index-1] }
func (s *watchStream) Err() error                         { return nil }
func (s *watchStream) Close() error                       { s.closed = true; return nil }

func TestSetupWatchReplayAndSettle(t *testing.T) {
	for _, failure := range []string{"", "setup failed"} {
		t.Run(failure, func(t *testing.T) {
			stream := &watchStream{frames: []*pb.WatchSessionSetupResponse{
				{Event: &pb.WatchSessionSetupResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: "earlier output"}}},
				{Event: &pb.WatchSessionSetupResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: "live output"}}},
				{Event: &pb.WatchSessionSetupResponse_SetupSettled{SetupSettled: &pb.SessionSetupSettled{SetupError: failure}}},
			}}
			if failure != "" {
				terminal := stream.frames[len(stream.frames)-1]
				stream.frames = stream.frames[:len(stream.frames)-1]
				for i := range 12 {
					stream.frames = append(stream.frames, &pb.WatchSessionSetupResponse{Event: &pb.WatchSessionSetupResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: fmt.Sprintf("additional line %02d", i)}}})
				}
				stream.frames = append(stream.frames, terminal)
			}
			m := NewSetupModel(&watchClient{stream: stream}, context.Background(), &pb.Session{Id: "session-1", Title: "Building something"})
			batch := m.Init()().(tea.BatchMsg)
			updated, cmd := m.Update(batch[0]())
			m = updated.(Model)
			for range 2 {
				updated, next := m.Update(cmd())
				m, cmd = updated.(Model), next
			}
			for _, text := range []string{"initializing", "Running setup script…", "Session session-1", "Building something", "earlier output", "live output"} {
				if !strings.Contains(m.View().Content, text) {
					t.Fatalf("missing %q: %s", text, m.View().Content)
				}
			}
			if failure != "" {
				for range 12 {
					updated, cmd = m.Update(cmd())
					m = updated.(Model)
				}
				if strings.Contains(m.View().Content, "earlier output") {
					t.Fatal("running pane did not retain only tail")
				}
			}
			updated, cmd = m.Update(cmd())
			m = updated.(Model)
			if !stream.closed {
				t.Fatal("settled stream remained open")
			}
			if failure == "" {
				if cmd == nil {
					t.Fatal("no navigation on successful settle")
				}
				if got := cmd(); !reflect.DeepEqual(got, ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "session-1"}) {
					t.Fatalf("navigation=%+v", got)
				}
				if m.Done() {
					t.Fatal("watch emits session-created telemetry/navigation")
				}
			} else {
				if cmd != nil {
					t.Fatal("failed setup navigated away")
				}
				for _, text := range []string{failure, "earlier output", "live output", "additional line 00", "additional line 11", "[esc] back"} {
					if !strings.Contains(m.View().Content, text) {
						t.Fatalf("missing %q: %s", text, m.View().Content)
					}
				}
			}
		})
	}
}

func TestSetupWatchUnavailableFallsBack(t *testing.T) {
	for _, kind := range []rpcerr.Kind{rpcerr.NotFound, rpcerr.Unimplemented} {
		t.Run(string(kind), func(t *testing.T) {
			m := NewSetupModel(&watchClient{err: rpcerr.New(kind, errors.New("unavailable"))}, context.Background(), &pb.Session{Id: "session-1"})
			batch := m.Init()().(tea.BatchMsg)
			_, cmd := m.Update(batch[0]())
			if cmd == nil {
				t.Fatal("missing fallback")
			}
			if got := cmd(); !reflect.DeepEqual(got, ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "session-1"}) {
				t.Fatalf("fallback=%+v", got)
			}
		})
	}
}

func TestStaleSetupStreamMessagesIgnored(t *testing.T) {
	stale := &streamIdentity{}
	for _, m := range []Model{NewModel(&stubClient{}, context.Background()), NewSetupModel(&stubClient{}, context.Background(), &pb.Session{Id: "session-1"})} {
		for _, msg := range []tea.Msg{
			setupScriptLineMsg{streamID: stale, text: "stale"},
			streamSessionAcceptedMsg{streamID: stale, session: &pb.Session{Id: "wrong"}},
			streamSessionCreatedMsg{streamID: stale, session: &pb.Session{Id: "wrong"}},
			streamErrorMsg{streamID: stale, err: errors.New("stale")},
			createSessionStreamMsg{streamID: stale},
			setupWatchStreamMsg{streamID: stale},
			setupSettledMsg{streamID: stale},
		} {
			updated, cmd := m.Update(msg)
			got := updated.(Model)
			if cmd != nil || got.err != nil || got.done || len(got.setupLines) != 0 || got.acceptedSess != m.acceptedSess {
				t.Fatalf("stale %T mutated model or scheduled read", msg)
			}
		}
	}
}

type blockingCreateClient struct {
	stubClient
	opened chan context.Context
}

func (c *blockingCreateClient) CreateSession(ctx context.Context, _ *pb.CreateSessionRequest) (rpc.CreateSessionStream, error) {
	c.opened <- ctx
	return &blockingCreateStream{ctx: ctx}, nil
}

type blockingCreateStream struct{ ctx context.Context }

func (s *blockingCreateStream) Receive() bool                  { <-s.ctx.Done(); return false }
func (s *blockingCreateStream) Msg() *pb.CreateSessionResponse { return nil }
func (s *blockingCreateStream) Err() error                     { return s.ctx.Err() }
func (s *blockingCreateStream) Close() error                   { return nil }

func TestCreatingEscapeCancelsBlockedReceive(t *testing.T) {
	c := &blockingCreateClient{opened: make(chan context.Context, 1)}
	m := NewModel(c, context.Background())
	m.repos = oneRepo()
	m.selectedRepoID = m.repos[0].Id
	open := m.startCreating()
	updated, read := m.Update(open())
	m = updated.(Model)
	ctx := <-c.opened
	done := make(chan tea.Msg, 1)
	go func() { done <- read() }()
	updated, _ = m.Update(specialKeyPress(tea.KeyEscape))
	m = updated.(Model)
	if !m.Cancelled() {
		t.Fatal("Escape did not cancel")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context not cancelled")
	}
	select {
	case msg := <-done:
		if _, cmd := m.Update(msg); cmd != nil {
			t.Fatal("abandoned stream scheduled another read")
		}
	case <-time.After(time.Second):
		t.Fatal("Receive blocked after Escape")
	}
}

func TestSetupWatchEscapeCancels(t *testing.T) {
	m := NewSetupModel(&stubClient{}, context.Background(), &pb.Session{Id: "session-1"})
	updated, _ := m.Update(specialKeyPress(tea.KeyEscape))
	m = updated.(Model)
	if !m.Cancelled() || m.ctx.Err() != context.Canceled {
		t.Fatal("Escape did not cancel watch")
	}
}

func TestSetupWatchIgnoresWizardResults(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{"repos", ReposMsg{Repos: []*pb.Repo{{Id: "old-repo"}}}},
		{"repos-error", ReposMsg{Err: errors.New("old repository error")}},
		{"agents", agentsMsg{agents: []rpc.AgentInfo{{Name: "old-agent"}}}},
		{"prs", PrsMsg{PRs: []*pb.PRSummary{{}}}},
		{"prs-error", PrsMsg{Err: errors.New("old PR error")}},
		{"issues", IssuesMsg{Issues: []*pb.TrackerIssue{{}}}},
		{"issues-error", IssuesMsg{Err: errors.New("old issue error")}},
		{"issue-search", searchIssuesTickMsg{query: "old query"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &watchStream{frames: []*pb.WatchSessionSetupResponse{
				{Event: &pb.WatchSessionSetupResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: "live output"}}},
				{Event: &pb.WatchSessionSetupResponse_SetupSettled{SetupSettled: &pb.SessionSetupSettled{}}},
			}}
			session := &pb.Session{Id: "session-1"}
			m := NewSetupModel(&watchClient{stream: stream}, context.Background(), session)
			defer m.streamCancel()
			batch := m.Init()().(tea.BatchMsg)
			updated, read := m.Update(batch[0]())
			m = updated.(Model)
			updated, cmd := m.Update(tc.msg)
			m = updated.(Model)
			if cmd != nil || m.phase != newSessionPhaseCreating || m.err != nil ||
				m.acceptedSess != session || m.watchStream != stream || !m.watchingSetup ||
				len(m.repos) != 0 || len(m.agents) != 0 || len(m.prs) != 0 || len(m.trackerIssues) != 0 ||
				m.selectedRepoID != "" || m.issueSearchQuery != "" {
				t.Fatalf("wizard result %T changed setup watcher (phase=%v, err=%v)", tc.msg, m.phase, m.err)
			}
			updated, read = m.Update(read())
			m = updated.(Model)
			if len(m.setupLines) != 1 || m.setupLines[0] != "live output" || read == nil {
				t.Fatal("watcher did not continue receiving live output")
			}
			_, navigate := m.Update(read())
			if navigate == nil || !reflect.DeepEqual(navigate(), ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: session.Id}) {
				t.Fatal("watcher did not navigate after setup settled")
			}
		})
	}
}
