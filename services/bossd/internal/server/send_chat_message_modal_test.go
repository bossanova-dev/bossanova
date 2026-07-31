package server

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/tmux"
)

// modalProbeClient is an AgentRunnerClient that answers only HasQuestionPrompt.
// The embedded nil interface supplies the rest of the surface and panics if the
// send path ever calls anything else, which is the assertion: the modal check
// must cost exactly one RPC per capture and must not reach for the plugin's
// other capabilities.
type modalProbeClient struct {
	agent.AgentRunnerClient

	mu       sync.Mutex
	calls    int
	lastPane []byte
	// hasPrompt is the NOTIFY verdict and blocksInput the MODAL one. They are
	// separate fields here because the gate must read only the second: a real
	// plugin sets hasPrompt=true, blocksInput=false for a conversational
	// question asked with a live composer, and a gate keyed on hasPrompt would
	// refuse to deliver the answer.
	hasPrompt   bool
	blocksInput bool
	err         error
}

func (c *modalProbeClient) HasQuestionPrompt(_ context.Context, req *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.lastPane = req.GetPaneContent()
	if c.err != nil {
		return nil, c.err
	}
	return &pb.HasQuestionPromptResponse{HasPrompt: c.hasPrompt, BlocksInput: c.blocksInput}, nil
}

func (c *modalProbeClient) snapshot() (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, string(c.lastPane)
}

// recordingPaneFactory serves pane as the answer to every capture-pane and
// records each tmux subcommand, so a test can prove which tmux verbs ran.
type recordingPaneFactory struct {
	mu   sync.Mutex
	pane string
	subs []string
}

func (f *recordingPaneFactory) factory(ctx context.Context, _ string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	f.subs = append(f.subs, sub)
	if sub == "capture-pane" {
		return exec.CommandContext(ctx, "printf", "%s", f.pane)
	}
	return exec.CommandContext(ctx, "true")
}

func (f *recordingPaneFactory) ran(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.subs, sub)
}

// newModalTestServer mirrors newVerifierTestServer but also wires the chat
// agent's plugin client, which is what makes modalDetectorFor resolve.
func newModalTestServer(t *testing.T, client agent.AgentRunnerClient, factory tmux.CommandFactory) *Server {
	t.Helper()
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		tmux:       tmux.NewClient(tmux.WithCommandFactory(factory)),
	}
	if client != nil {
		s.agentClients = map[string]agent.AgentRunnerClient{"claude": client}
	}
	return s
}

// The pane the daemon "captures" in these tests. Its ❯ leads a MENU row, which
// is exactly the shape that defeats the readiness gate on its own: row anchoring
// accepts it, and only the agent's own grammar can tell it from a composer.
const modalTestPane = "What do you want to do?\n\n❯ 1. Stop and wait for limit to reset\n  2. Switch to usage credits\n"

// TestSendChatMessageRefusesModalPane is BOS-600 end to end through the RPC: a
// chat whose agent reports a question prompt is refused before anything is
// typed, and the refusal comes back as FailedPrecondition naming the state.
//
// FailedPrecondition rather than Internal because the daemon is healthy and the
// request was well-formed — the PANE is in the wrong state, and resending
// changes nothing until a human (or the agent) leaves it. That is precisely what
// separates this from OutcomeNotSubmitted, whose retry is safe.
func TestSendChatMessageRefusesModalPane(t *testing.T) {
	probe := &modalProbeClient{hasPrompt: true, blocksInput: true}
	fake := &recordingPaneFactory{pane: modalTestPane}
	s := newModalTestServer(t, probe, fake.factory)

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
		Submit:         true,
	}))
	if err == nil {
		t.Fatal("expected a refusal for a pane showing a modal, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want CodeFailedPrecondition: %v", connect.CodeOf(err), err)
	}
	// The state has to be named in the message: delivery_state is dropped by the
	// hand-rolled converters on the proxied surface, so a caller reached that way
	// learns "modal, do not resend" from the error text or nowhere.
	if !strings.Contains(err.Error(), tmux.OutcomeBlockedByModal.String()) {
		t.Errorf("error %q does not name the delivery state %q", err, tmux.OutcomeBlockedByModal)
	}
	if fake.ran("send-keys") || fake.ran("load-buffer") || fake.ran("paste-buffer") {
		t.Fatalf("pane was refused but tmux still ran a delivery verb: %v", fake.subs)
	}
}

// TestSendChatMessageDeliversWhenAgentSeesNoModal is the converse of the test
// above, and it is the one that makes that test mean something: run the SAME
// pane past a detector that answers false and the message is delivered. So the
// refusal is caused by the agent's verdict, not by the pane shape, the fixture,
// or a gate that now refuses everything.
//
// It is also the pre-BOS-600 behaviour reproduced on purpose: this ❯-leading
// menu row satisfies the readiness gate on its own, which is exactly why the
// readiness marker alone could never have caught it.
func TestSendChatMessageDeliversWhenAgentSeesNoModal(t *testing.T) {
	// Two shapes must both deliver, and the second is the one that matters.
	//
	//   no question at all          -> has_prompt=false, blocks_input=false
	//   conversational question     -> has_prompt=TRUE,  blocks_input=false
	//
	// The second is what a real Claude pane reports when Claude ends a turn with
	// "…?" and leaves the composer live — the single commonest reason anyone
	// sends a chat message, and the reason the gate must read blocks_input. A
	// gate keyed on has_prompt returns FailedPrecondition here, which also takes
	// down broadcast fan-out and GitHub callbacks, since both deliver through
	// this same RPC.
	for _, tt := range []struct {
		name      string
		hasPrompt bool
	}{
		{name: "agent reports no prompt at all", hasPrompt: false},
		{name: "agent reports a question that does not take the composer", hasPrompt: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe := &modalProbeClient{hasPrompt: tt.hasPrompt, blocksInput: false}
			fake := &recordingPaneFactory{pane: modalTestPane}
			s := newModalTestServer(t, probe, fake.factory)

			if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
				AgentSessionId: "agent-1",
				Message:        "do the thing",
				Submit:         true,
			})); err != nil {
				t.Fatalf("send failed for a pane the agent does not call a modal: %v", err)
			}
			if !fake.ran("send-keys") && !fake.ran("load-buffer") && !fake.ran("paste-buffer") {
				t.Errorf("no text was delivered; tmux ran %v", fake.subs)
			}
			if calls, _ := probe.snapshot(); calls == 0 {
				t.Error("the agent was never consulted; the modal check did not run at all")
			}
		})
	}
}

// TestModalDetectorForRoutesToAgentPlugin is the other half of the BOS-600
// criterion-1 proof composition. The codex approval-menu grammar lives in the
// codex plugin — a separate Go module neither this package nor internal/tmux may
// import — so what has to be proven HERE is that the captured pane bytes reach
// that agent's HasQuestionPrompt and that its verdict is what the gate acts on.
// The plugin's own TestQuestionPromptRealPaneFixture proves the verdict is true
// for the real codex approval capture.
func TestModalDetectorForRoutesToAgentPlugin(t *testing.T) {
	t.Run("routes the pane and returns the plugin verdict", func(t *testing.T) {
		for _, verdict := range []bool{true, false} {
			// has_prompt is pinned TRUE in both iterations so the detector's
			// answer can only be tracking blocks_input. Wire it to has_prompt
			// and the false iteration fails.
			probe := &modalProbeClient{hasPrompt: true, blocksInput: verdict}
			s := newModalTestServer(t, probe, (&recordingPaneFactory{}).factory)

			detector := s.modalDetectorFor("claude")
			if detector == nil {
				t.Fatal("modalDetectorFor returned nil for a loaded agent")
			}
			got, err := detector(context.Background(), []byte(modalTestPane))
			if err != nil {
				t.Fatalf("detector returned an error: %v", err)
			}
			if got != verdict {
				t.Errorf("detector = %v, want the plugin's verdict %v", got, verdict)
			}
			calls, pane := probe.snapshot()
			if calls != 1 {
				t.Errorf("HasQuestionPrompt called %d times, want exactly 1 per capture", calls)
			}
			if pane != modalTestPane {
				t.Errorf("plugin received pane %q, want the captured pane %q", pane, modalTestPane)
			}
		}
	})

	t.Run("surfaces the plugin error rather than guessing", func(t *testing.T) {
		wantErr := errors.New("plugin unreachable")
		probe := &modalProbeClient{err: wantErr}
		s := newModalTestServer(t, probe, (&recordingPaneFactory{}).factory)

		_, err := s.modalDetectorFor("claude")(context.Background(), []byte(modalTestPane))
		if !errors.Is(err, wantErr) {
			t.Fatalf("detector error = %v, want %v", err, wantErr)
		}
	})

	t.Run("reports the degradation once per send", func(t *testing.T) {
		// Failing open on a detector error is correct — an unreachable plugin must
		// not stop delivery — but silent fail-open would restore the pre-BOS-600
		// behaviour with nothing anywhere to say so. Exactly once per send, not
		// once per poll tick: the readiness window polls every 100ms for 5s, and 50
		// identical warnings per send would be its own bug.
		var logs bytes.Buffer
		probe := &modalProbeClient{err: errors.New("plugin unreachable")}
		s := newModalTestServer(t, probe, (&recordingPaneFactory{}).factory)
		s.logger = zerolog.New(&logs)

		detector := s.modalDetectorFor("claude")
		for range 5 {
			if _, err := detector(context.Background(), []byte(modalTestPane)); err == nil {
				t.Fatal("detector swallowed the plugin error instead of returning it")
			}
		}
		if got := strings.Count(logs.String(), "modal check unavailable"); got != 1 {
			t.Errorf("logged the degradation %d times across 5 probes, want exactly 1:\n%s", got, logs.String())
		}
	})

	t.Run("no client means no check", func(t *testing.T) {
		// Fail open, mirroring the status poller's rule that a missing plugin
		// can never lock a chat in QUESTION: an unloaded or crashed plugin must
		// not become a new reason chat delivery stops.
		s := newModalTestServer(t, nil, (&recordingPaneFactory{}).factory)
		if d := s.modalDetectorFor("claude"); d != nil {
			t.Fatal("modalDetectorFor returned a detector with no agent client loaded")
		}
		if d := s.modalDetectorFor("codex"); d != nil {
			t.Fatal("modalDetectorFor returned a detector for an agent that is not loaded")
		}
	})
}

// TestSendChatMessagePassesDetectorToSpawner pins the wiring the RPC test cannot
// see: the send path must resolve the detector from the CHAT's agent and hand it
// to the spawner per call. A nil here would leave the readiness gate running
// with the modal check silently disabled.
func TestSendChatMessagePassesDetectorToSpawner(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	spawner := &fakeTmuxClient{available: true, hasSession: true}
	s := &Server{
		agentChats:   &chatStoreFake{chat: chat},
		sessions:     &sessionStoreFake{sess: sess},
		agentClients: map[string]agent.AgentRunnerClient{"claude": &modalProbeClient{}},
		wakeHook: wakeHook{
			spawner:     spawner,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv:        claudeArgvBuilder(),
		},
	}

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
		Submit:         true,
	})); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if len(spawner.sentMessages) != 1 {
		t.Fatalf("recorded %d sends, want 1", len(spawner.sentMessages))
	}
	if spawner.sentMessages[0].modal == nil {
		t.Fatal("send ran with no modal detector; the readiness gate's modal check was disabled")
	}
}
