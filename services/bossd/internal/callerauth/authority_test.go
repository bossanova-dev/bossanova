package callerauth

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestAuthority(t *testing.T) {
	for _, tc := range []struct {
		name               string
		parent             func(int) (int, error)
		panesErr, chatsErr bool
		want               domain.CallerKind
	}{
		{name: "operator", want: domain.CallerOperator},
		{name: "vanished peer", parent: func(int) (int, error) { return 0, os.ErrNotExist }, want: domain.CallerUnattested},
		{name: "vanished ancestor", parent: func(pid int) (int, error) {
			if pid == 10 {
				return 20, nil
			}
			return 0, os.ErrNotExist
		}, want: domain.CallerUnattested},
		{name: "daemon child", parent: func(pid int) (int, error) {
			if pid == 10 {
				return 99, nil
			}
			return 1, nil
		}, want: domain.CallerDaemonChild},
		{name: "parent fails", parent: func(int) (int, error) { return 0, errors.New("denied") }, want: domain.CallerUnattested},
		{name: "panes fail", panesErr: true, want: domain.CallerUnattested},
		{name: "chats fail", chatsErr: true, want: domain.CallerUnattested},
		{name: "cycle", parent: func(pid int) (int, error) { return pid, nil }, want: domain.CallerUnattested},
		{name: "too long", parent: func(pid int) (int, error) { return pid + 1, nil }, want: domain.CallerUnattested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := tc.parent
			if parent == nil {
				parent = func(int) (int, error) { return 1, nil }
			}
			a := New(Config{PeerPID: func(net.Conn) (int, error) { return 10, nil }, ParentPID: parent,
				PaneSessions: func(context.Context) (map[int][]string, error) {
					if tc.panesErr {
						return nil, errors.New("panes")
					}
					return nil, nil
				},
				Chats: func(context.Context) ([]*domain.AgentChat, error) {
					if tc.chatsErr {
						return nil, errors.New("chats")
					}
					return nil, nil
				}, DaemonPID: 99})
			if got := a.Principal(a.ConnContext(context.Background(), nil)); got.Kind != tc.want {
				t.Fatalf("got %+v want %s", got, tc.want)
			}
		})
	}
}

func TestAuthorityChatAndUnknownPeer(t *testing.T) {
	name := "chat-tmux"
	cfg := Config{PeerPID: func(net.Conn) (int, error) { return 10, nil }, ParentPID: func(pid int) (int, error) {
		if pid == 10 {
			return 20, nil
		}
		return 1, nil
	},
		PaneSessions: func(context.Context) (map[int][]string, error) { return map[int][]string{20: {name}, 30: {name}}, nil },
		Chats: func(context.Context) ([]*domain.AgentChat, error) {
			return []*domain.AgentChat{{AgentSessionID: "chat", SessionID: "session", Role: domain.AgentChatRoleOperator, TmuxSessionName: &name}}, nil
		}, DaemonPID: 99}
	a := New(cfg)
	if got := a.Principal(a.ConnContext(context.Background(), nil)); got.Kind != domain.CallerAgentChat || got.AgentSessionID != "chat" || got.Role != domain.AgentChatRoleOperator {
		t.Fatalf("chat: %+v", got)
	}
	cfg.PeerPID = func(net.Conn) (int, error) { return 0, errors.New("peer") }
	a = New(cfg)
	if got := a.Principal(a.ConnContext(context.Background(), nil)); got.Kind != domain.CallerUnattested {
		t.Fatalf("peer: %+v", got)
	}
	if got := a.Principal(context.Background()); got.Kind != domain.CallerUnattested {
		t.Fatalf("missing peer: %+v", got)
	}
}

func TestAuthorityLinkedPaneSessions(t *testing.T) {
	managed, other := "managed-chat", "other-chat"
	for _, tc := range []struct {
		name     string
		sessions []string
		chats    []*domain.AgentChat
		want     domain.CallerKind
	}{
		{name: "managed then personal", sessions: []string{managed, "personal"}, want: domain.CallerAgentChat},
		{name: "personal then managed", sessions: []string{"personal", managed}, want: domain.CallerAgentChat},
		{name: "renamed managed session", sessions: []string{"personal"}, want: domain.CallerUnattested},
		{name: "no panes for known chat", want: domain.CallerUnattested},
		{name: "two managed identities", sessions: []string{managed, other}, chats: []*domain.AgentChat{{AgentSessionID: "other", TmuxSessionName: &other}}, want: domain.CallerUnattested},
		{name: "duplicate managed identity", sessions: []string{managed, managed}, want: domain.CallerAgentChat},
		{name: "conflicting session owners", sessions: []string{managed}, chats: []*domain.AgentChat{{AgentSessionID: "other", TmuxSessionName: &managed}}, want: domain.CallerUnattested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{
				PeerPID: func(net.Conn) (int, error) { return 10, nil },
				ParentPID: func(pid int) (int, error) {
					if pid == 10 {
						return 20, nil
					}
					return 1, nil
				},
				PaneSessions: func(context.Context) (map[int][]string, error) { return map[int][]string{20: tc.sessions}, nil },
				Chats: func(context.Context) ([]*domain.AgentChat, error) {
					return append([]*domain.AgentChat{{AgentSessionID: "chat", Role: domain.AgentChatRoleOperator, TmuxSessionName: &managed}}, tc.chats...), nil
				},
				DaemonPID: 99,
			})
			got := a.Principal(a.ConnContext(context.Background(), nil))
			if got.Kind != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
			if tc.want == domain.CallerAgentChat && got.AgentSessionID != "chat" {
				t.Fatalf("wrong chat: %+v", got)
			}
			if tc.want != domain.CallerOperator && got.HasCapability(domain.CapabilityVerifyApprove, nil) {
				t.Fatalf("unexpected approval capability: %+v", got)
			}
		})
	}
}

func TestAuthorityIncompletePaneSnapshots(t *testing.T) {
	managed := domain.ChatSessionName("1234567890abcdef", "abcdef12-3456-7890")
	other := domain.ChatSessionName("fedcba0987654321", "12345678-3456-7890")
	for _, tc := range []struct {
		name  string
		panes map[int][]string
		chats []*domain.AgentChat
		want  domain.CallerKind
	}{
		{name: "missing inventory with known chat", chats: []*domain.AgentChat{{AgentSessionID: "chat", TmuxSessionName: &managed}}, want: domain.CallerUnattested},
		{name: "known chat renamed to personal", panes: map[int][]string{20: {"personal"}}, chats: []*domain.AgentChat{{AgentSessionID: "chat", TmuxSessionName: &managed}}, want: domain.CallerUnattested},
		{name: "all known sessions present with personal caller", panes: map[int][]string{20: {"personal"}, 30: {managed}, 40: {other}}, chats: []*domain.AgentChat{{AgentSessionID: "chat", TmuxSessionName: &managed}, {AgentSessionID: "other", TmuxSessionName: &other}}, want: domain.CallerOperator},
		{name: "one of several known sessions missing", panes: map[int][]string{20: {"personal"}, 30: {managed}}, chats: []*domain.AgentChat{{AgentSessionID: "chat", TmuxSessionName: &managed}, {AgentSessionID: "other", TmuxSessionName: &other}}, want: domain.CallerUnattested},
		{name: "own pane has conflicting identities", panes: map[int][]string{20: {managed, other}}, chats: []*domain.AgentChat{{AgentSessionID: "chat", TmuxSessionName: &managed}, {AgentSessionID: "other", TmuxSessionName: &other}}, want: domain.CallerUnattested},
		{name: "unrelated pane has conflicting identities", panes: map[int][]string{20: {"personal"}, 30: {managed, other}}, chats: []*domain.AgentChat{{AgentSessionID: "chat", TmuxSessionName: &managed}, {AgentSessionID: "other", TmuxSessionName: &other}}, want: domain.CallerOperator},
		{name: "unrelated session has conflicting owners", panes: map[int][]string{20: {"personal"}, 30: {managed}}, chats: []*domain.AgentChat{{AgentSessionID: "chat", TmuxSessionName: &managed}, {AgentSessionID: "other", TmuxSessionName: &managed}}, want: domain.CallerOperator},
		{name: "no server and no chats", want: domain.CallerOperator},
		{name: "live managed pane with cleared pointer", panes: map[int][]string{20: {managed}}, chats: []*domain.AgentChat{{AgentSessionID: "chat"}}, want: domain.CallerUnattested},
		{name: "live managed pane missing chat row", panes: map[int][]string{20: {managed}}, want: domain.CallerUnattested},
		{name: "linked unknown managed pane", panes: map[int][]string{20: {"personal", managed}}, want: domain.CallerUnattested},
		{name: "personal tmux", panes: map[int][]string{20: {"personal"}}, want: domain.CallerOperator},
		{name: "personal boss notes", panes: map[int][]string{20: {"boss-notes"}}, want: domain.CallerOperator},
		{name: "unrelated untracked managed pane", panes: map[int][]string{30: {managed}, 20: {"personal"}}, want: domain.CallerOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{
				PeerPID: func(net.Conn) (int, error) { return 10, nil },
				ParentPID: func(pid int) (int, error) {
					if pid == 10 {
						return 20, nil
					}
					return 1, nil
				},
				PaneSessions: func(context.Context) (map[int][]string, error) { return tc.panes, nil },
				Chats:        func(context.Context) ([]*domain.AgentChat, error) { return tc.chats, nil },
				DaemonPID:    99,
			})
			got := a.Principal(a.ConnContext(context.Background(), nil))
			if got.Kind != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
}
