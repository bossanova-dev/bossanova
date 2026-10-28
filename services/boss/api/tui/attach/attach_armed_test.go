package attach

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAttach_LaunchingViewShowsArmedNote(t *testing.T) {
	note := "verify · " + "awaiting checks_passed_ready" + " · expires in 6d"
	newLaunching := func() Model {
		m := NewAttachModel(&attachTelemetryStub{}, context.Background(), pty.NewManager(), "session-1", "agent-armed")
		m.session = &pb.Session{Id: "session-1", Title: "ship it", AgentName: "claude"}
		m.launching = true
		return m
	}

	plain := newLaunching()
	base := plain.View().Content
	if strings.Contains(base, "awaiting") || strings.Count(base, "\n") != 0 {
		t.Fatalf("launching view without a note changed: %q", base)
	}

	armed := newLaunching()
	armed.SetArmedNote(note)
	view := stripANSI(armed.View().Content)
	lines := strings.Split(view, "\n")
	if len(lines) != 2 {
		t.Fatalf("launching view lines = %d, want 2: %q", len(lines), view)
	}
	if lines[0] != stripANSI(base) {
		t.Fatalf("first line changed: %q, want %q", lines[0], stripANSI(base))
	}
	if strings.TrimSpace(lines[1]) != note {
		t.Fatalf("armed line = %q, want %q", strings.TrimSpace(lines[1]), note)
	}
	if strings.Contains(view, "unpublished callback body") {
		t.Fatalf("message body rendered: %q", view)
	}

	// Width-truncated to one row.
	armed.width = 30
	for _, line := range strings.Split(stripANSI(armed.View().Content), "\n")[1:] {
		if w := len([]rune(line)); w > 30 {
			t.Fatalf("armed line %d columns wide in a 30-column terminal: %q", w, line)
		}
	}
}

type attachTrustClient struct {
	attachTelemetryStub
	repos   []*pb.Repo
	repoErr error
}

func (*attachTrustClient) GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error) {
	return &pb.Session{Id: "session-1", Title: "ship it", RepoId: "repo-1", AgentName: "codex"}, nil
}
func (c *attachTrustClient) ListRepos(context.Context) ([]*pb.Repo, error) {
	return c.repos, c.repoErr
}
func (*attachTrustClient) ListChats(context.Context, string) ([]*pb.AgentChat, error) {
	return nil, nil
}

func TestAttach_RepoTrustNote_Chat_AC4(t *testing.T) {
	const note = `work is untrusted, so Codex runs without its project configuration and may stop on a folder-trust prompt. To trust it: boss repo update repo-1 --trusted, or "isTrusted": true in .boss-project.json.`
	for _, tt := range []struct {
		name     string
		repos    []*pb.Repo
		err      error
		wantNote bool
	}{
		{name: "untrusted", repos: []*pb.Repo{{Id: "other", DisplayName: "other"}, {Id: "repo-1", DisplayName: "work"}}, wantNote: true},
		{name: "trusted", repos: []*pb.Repo{{Id: "repo-1", DisplayName: "work", IsTrusted: true}}},
		{name: "lookup failed", err: errors.New("local only"), repos: []*pb.Repo{{Id: "repo-1", DisplayName: "work"}}},
		{name: "repo missing", repos: []*pb.Repo{{Id: "other", DisplayName: "other"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &attachTrustClient{repos: tt.repos, repoErr: tt.err}
			m := NewAttachModel(client, context.Background(), pty.NewManager(), "session-1", "")
			msg := m.Init()()
			if _, ok := msg.(attachReadyMsg); !ok {
				t.Fatalf("fetch = %T, want attachReadyMsg", msg)
			}
			updated, _ := m.Update(msg)
			m = updated.(Model)
			for _, state := range []string{"launching", "ended", "error"} {
				m.launching = state == "launching"
				m.returned = !m.launching
				if state == "error" {
					m.agentErr = errors.New("exit status 1")
				}
				view := stripANSI(m.View().Content)
				if got := strings.Contains(view, note); got != tt.wantNote {
					t.Fatalf("%s note present = %v, want %v: %q", state, got, tt.wantNote, view)
				}
				if !tt.wantNote && strings.Contains(view, "is untrusted") {
					t.Fatalf("%s claimed unknown trust: %q", state, view)
				}
				if tt.wantNote && state == "launching" {
					lines := strings.Split(view, "\n")
					if len(lines) != 2 || !strings.Contains(lines[0], "Launching Codex") || strings.TrimSpace(lines[1]) != note {
						t.Fatalf("launch note layout: %q", view)
					}
				}
				if tt.wantNote {
					m.width = 45
					for _, line := range strings.Split(stripANSI(m.View().Content), "\n") {
						if strings.Contains(line, "is untrusted") && ansi.StringWidth(line) > m.width {
							t.Fatalf("trust note exceeds width: %q", line)
						}
					}
					m.width = 0
				}
			}
		})
	}
}

func TestAttach_RepoTrustNoteUsesDisplayedAgent(t *testing.T) {
	client := &attachTrustClient{repos: []*pb.Repo{{Id: "repo-1", DisplayName: "work"}}}
	m := NewAttachModel(client, context.Background(), pty.NewManager(), "session-1", "existing-chat")
	m.SetOverrideAgent("opencode")
	updated, _ := m.Update(m.Init()())
	m = updated.(Model)
	if view := stripANSI(m.View().Content); !strings.Contains(view, "Launching OpenCode") || !strings.Contains(view, "so OpenCode runs") {
		t.Fatalf("override agent disagrees with trust note: %q", view)
	}
	// RecordChat supplies the authoritative agent for an existing mixed-agent chat.
	m.recordedAgent = "claude"
	for _, returned := range []bool{false, true} {
		m.launching = !returned
		m.returned = returned
		view := stripANSI(m.View().Content)
		if !strings.Contains(view, "so Claude Code runs") || strings.Contains(view, "so OpenCode runs") {
			t.Fatalf("recorded agent disagrees with trust note: %q", view)
		}
	}
}
