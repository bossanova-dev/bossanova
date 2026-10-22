package session

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestLaunchDiagnosisArgv(t *testing.T) {
	for _, tt := range []struct{ name, ps, payload, want string }{
		{"descendant", "12 1 shell\n14 13 agent implement the plan\n13 12 shim\n99 1 peer", "\n implement the plan\nnext line", "yes"},
		{"peer only", "12 1 shell\n14 12 agent\n99 1 agent implement the plan", "implement the plan", "no"},
		{"no payload", "12 1 shell", " ", "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := launchArgvCarriesInput(tt.ps, 12, tt.payload); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestLaunchDiagnosisBeforeCleanup(t *testing.T) {
	for _, consumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unread", true: "consumed"}[consumed], func(t *testing.T) {
			h := newDeliveryHarness(t, 80*time.Millisecond)
			h.tmuxFake.capturePaneOutput = "Codex\n› Ask Codex to do anything\n\n⚠ 1 warning · f2 to view\n"
			h.tmuxFake.listPanesOutput = "12\n"
			h.lc.launchProcessSnapshot = func(context.Context) ([]byte, error) { return nil, errors.New("ps unavailable") }
			h.sessions.sessions["sess-1"].AccountID = ptr("account-7")
			h.agentFake.ResolveInteractiveSessionIDFunc = func(*bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
				if consumed {
					_ = os.Remove(h.agentFake.LastBuildInteractiveCommand.GetInitialInputPath())
				}
				return &bossanovav1.ResolveInteractiveSessionIDResponse{Reason: "codex process tree visible but no rollout fd open yet"}, nil
			}
			_, err := h.lc.StartTmuxChat(context.Background(), "sess-1", submitInput("implement the plan"), "T", HookOpts{})
			var failure *DeliveryUnconfirmedError
			if !errors.As(err, &failure) {
				t.Fatalf("got %v", err)
			}
			state := map[bool]string{false: "unread", true: "consumed"}[consumed]
			for _, part := range []string{"input-file=" + state, "argv-carries-input=unknown", "composer=empty", "working=no", "account=account-7", `resolve="codex process tree visible but no rollout fd open yet"`, "pane tail:"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("missing %q in %s", part, err)
				}
			}
			if _, err := os.Stat(h.agentFake.LastBuildInteractiveCommand.GetInitialInputPath()); !os.IsNotExist(err) {
				t.Fatalf("input not cleaned: %v", err)
			}
		})
	}
}

func TestStartTmuxChatRelaunch(t *testing.T) {
	for _, shape := range []string{"success", "second-failure", "hook", "working", "other", "exited", "optional", "unresolved", "unknown-channel", "empty-with-log", "kill-failed", "working-unknown", "budget"} {
		t.Run(shape, func(t *testing.T) {
			h := newDeliveryHarness(t, 80*time.Millisecond)
			h.tmuxFake.capturePaneOutput = "Codex\n›\n"
			h.agentFake.CanConfirmDeliveryViaTranscript = shape != "hook" && shape != "unknown-channel"
			if shape == "empty-with-log" {
				h.tmuxFake.capturePaneOutput = ""
				if err := os.WriteFile(h.lc.agentLogPathFor("agent-1"), []byte("stale Codex\n›\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "other" {
				h.tmuxFake.capturePaneOutput = "Codex\n› another draft\n"
			}
			launches := 0
			h.lc.newTmuxChatAgentSessionID = func() string {
				launches++
				if launches == 1 {
					return "agent-1"
				}
				return "agent-2"
			}
			h.tmuxFake.hasSessionFunc = func(calls []recordedTmuxCall) bool {
				if shape == "exited" {
					return false
				}
				if shape == "kill-failed" {
					return true
				}
				for i := len(calls) - 1; i >= 0; i-- {
					if calls[i].subcommand == "new-session" {
						return true
					}
					if calls[i].subcommand == "kill-session" {
						return false
					}
				}
				return true
			}
			h.agentFake.ResolveInteractiveSessionIDFunc = func(req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
				if shape == "unknown-channel" || (shape == "unresolved" && launches == 1) {
					return &bossanovav1.ResolveInteractiveSessionIDResponse{Reason: "not yet"}, nil
				}
				id := "provider-" + req.GetRequestedSessionId()
				if shape == "hook" {
					id = req.GetRequestedSessionId()
				}
				return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: true, SessionId: id}, nil
			}
			h.agentFake.ReadTranscriptFunc = func(*bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
				resp := &bossanovav1.ReadTranscriptResponse{Exists: true}
				if (shape == "success" || shape == "unresolved") && launches == 2 {
					resp.Messages = []*bossanovav1.ChatMessage{{Role: "user", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}}
				}
				return resp, nil
			}
			h.agentFake.HasWorkingIndicatorFunc = func(*bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
				if shape == "working-unknown" {
					return nil, errors.New("unavailable")
				}
				return &bossanovav1.HasWorkingIndicatorResponse{IsWorking: shape == "working"}, nil
			}
			session := h.sessions.sessions["sess-1"]
			ctx := context.Background()
			if shape == "budget" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			var id string
			var err error
			if shape == "optional" {
				input := submitInput(session.Plan)
				input.DeliveryOptional = true
				id, err = h.lc.StartTmuxChat(ctx, "sess-1", input, "T", HookOpts{})
			} else {
				id, err = h.lc.startTmuxChat(ctx, "sess-1", StartSessionOpts{IsTmuxUnattended: true}, session, nil)
			}
			want := 1
			if shape == "success" || shape == "second-failure" || shape == "unresolved" {
				want = 2
			}
			if launches != want {
				t.Fatalf("launches=%d, want %d; err=%v", launches, want, err)
			}
			if shape == "success" || shape == "unresolved" {
				if err != nil || id != "agent-2" {
					t.Fatalf("id=%s err=%v", id, err)
				}
				if fails := h.startFailures(); len(fails) != 1 || !strings.Contains(fails[0].reason, "diagnosis:") {
					t.Fatalf("failed historical chat=%+v", fails)
				}
			} else if shape != "optional" && err == nil {
				t.Fatal("expected failed start")
			}
			h.tmuxFake.mu.Lock()
			defer h.tmuxFake.mu.Unlock()
			kills := 0
			news := 0
			for _, c := range h.tmuxFake.calls {
				if c.subcommand == "kill-session" {
					kills++
				}
				if c.subcommand == "new-session" {
					news++
				}
			}
			if news != want {
				t.Fatalf("new-session=%d want %d", news, want)
			}
			if shape == "success" && kills != 1 {
				t.Fatalf("kills=%d want 1", kills)
			}
		})
	}
}
