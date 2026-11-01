// Package callerauth attributes local socket callers from process ancestry.
package callerauth

import (
	"context"
	"net"
	"regexp"

	"github.com/recurser/bossd/internal/domain"
)

// Config supplies kernel facts and current chat/pane snapshots.
type Config struct {
	PeerPID      func(net.Conn) (int, error)
	ParentPID    func(int) (int, error)
	PaneSessions func(context.Context) (map[int][]string, error)
	Chats        func(context.Context) ([]*domain.AgentChat, error)
	DaemonPID    int
}

// Authority captures credentials at accept and resolves only operator actions.
type Authority struct{ config Config }

// New creates an authority. Missing dependencies fail closed.
func New(config Config) *Authority { return &Authority{config: config} }

type peerKey struct{}

// Match the canonical session names minted by domain.ChatSessionName, not
// personal names such as boss-notes. A live pane can outlast its chat pointer.
var callerChatSessionName = regexp.MustCompile(`^boss-[0-9a-f]{8}-[0-9a-f]{8}$`)

// ConnContext records the kernel peer PID before any request is handled.
func (a *Authority) ConnContext(ctx context.Context, conn net.Conn) context.Context {
	pid := 0
	if a.config.PeerPID != nil {
		if got, err := a.config.PeerPID(conn); err == nil && got > 0 {
			pid = got
		}
	}
	return context.WithValue(ctx, peerKey{}, pid)
}

// Principal maps a complete bounded ancestry and snapshots to a principal.
func (a *Authority) Principal(ctx context.Context) domain.CallerPrincipal {
	unknown := domain.CallerPrincipal{Kind: domain.CallerUnattested}
	pid, _ := ctx.Value(peerKey{}).(int)
	if pid <= 0 || a.config.ParentPID == nil || a.config.PaneSessions == nil || a.config.Chats == nil || a.config.DaemonPID <= 0 {
		return unknown
	}
	chain, ok := a.ancestry(pid)
	if !ok {
		return unknown
	}
	panes, err := a.config.PaneSessions(ctx)
	if err != nil {
		return unknown
	}
	chats, err := a.config.Chats(ctx)
	if err != nil {
		return unknown
	}
	bySession := make(map[string]domain.CallerPrincipal, len(chats))
	for _, chat := range chats {
		if chat == nil || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		principal := domain.CallerPrincipal{Kind: domain.CallerAgentChat, AgentSessionID: chat.AgentSessionID, SessionID: chat.SessionID, Role: chat.Role}
		if previous, ok := bySession[*chat.TmuxSessionName]; ok && previous != principal {
			principal = unknown
		}
		bySession[*chat.TmuxSessionName] = principal
	}
	byPID, ok := panePrincipals(panes, bySession)
	if !ok {
		return unknown
	}
	for _, ancestor := range chain {
		if pane, found := byPID[ancestor]; found && pane.Kind == domain.CallerUnattested {
			return unknown
		}
	}
	return domain.ClassifyCaller(chain, byPID, a.config.DaemonPID)
}

func panePrincipals(panes map[int][]string, chats map[string]domain.CallerPrincipal) (map[int]domain.CallerPrincipal, bool) {
	byPID := make(map[int]domain.CallerPrincipal)
	seenSessions := make(map[string]bool, len(chats))
	for panePID, names := range panes {
		for _, name := range names {
			seenSessions[name] = true
			principal, known := chats[name]
			if !known {
				if !callerChatSessionName.MatchString(name) {
					continue
				}
				principal.Kind = domain.CallerUnattested
			}
			if previous, exists := byPID[panePID]; exists && previous != principal {
				principal = domain.CallerPrincipal{Kind: domain.CallerUnattested}
			}
			byPID[panePID] = principal
		}
	}
	// An absent recorded session could have been renamed; unrelated personal
	// panes cannot establish that the managed inventory is complete.
	for name := range chats {
		if !seenSessions[name] {
			return nil, false
		}
	}
	return byPID, true
}

func (a *Authority) ancestry(pid int) ([]int, bool) {
	chain := make([]int, 0, 16)
	seen := make(map[int]bool)
	for len(chain) < 64 {
		if pid <= 1 {
			return chain, true
		}
		if seen[pid] {
			return nil, false
		}
		seen[pid] = true
		chain = append(chain, pid)
		parent, err := a.config.ParentPID(pid)
		if err != nil || parent < 0 {
			return nil, false
		}
		pid = parent
	}
	return nil, false
}
