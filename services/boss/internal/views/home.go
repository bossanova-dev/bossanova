package views

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const pollInterval = 2 * time.Second

// sessionListMsg carries the result of a ListSessions RPC call.
type sessionListMsg struct {
	sessions []*pb.Session
	err      error
}

// HomeModel is the main dashboard view showing active sessions.
type HomeModel struct {
	client   *client.Client
	ctx      context.Context
	sessions []*pb.Session
	cursor   int
	err      error
	loading  bool
	width    int
	height   int
}

// NewHomeModel creates a HomeModel wired to the daemon client.
func NewHomeModel(c *client.Client, ctx context.Context) HomeModel {
	return HomeModel{
		client:  c,
		ctx:     ctx,
		loading: true,
	}
}

func (h HomeModel) Init() tea.Cmd {
	return tea.Batch(fetchSessions(h.client, h.ctx), tickCmd())
}

func (h HomeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case sessionListMsg:
		h.loading = false
		h.sessions = msg.sessions
		h.err = msg.err
		if h.cursor >= len(h.sessions) && len(h.sessions) > 0 {
			h.cursor = len(h.sessions) - 1
		}
		return h, nil

	case tickMsg:
		return h, tea.Batch(fetchSessions(h.client, h.ctx), tickCmd())

	case tea.KeyMsg:
		switch msg.String() {
		case "q":
			return h, tea.Quit
		case "up", "k":
			if h.cursor > 0 {
				h.cursor--
			}
			return h, nil
		case "down", "j":
			if h.cursor < len(h.sessions)-1 {
				h.cursor++
			}
			return h, nil
		}
	}

	return h, nil
}

func (h HomeModel) View() tea.View {
	if h.err != nil {
		return tea.NewView(fmt.Sprintf(
			"\n  Cannot connect to daemon: %v\n\n  Start the daemon with: bossd\n\n  Press q to quit.\n",
			h.err,
		))
	}

	if h.loading {
		return tea.NewView("\n  Loading sessions...\n")
	}

	if len(h.sessions) == 0 {
		return tea.NewView("\n  No active sessions.\n\n  Press n to create a new session, r for repos, q to quit.\n")
	}

	// Session table rendered in next task.
	s := "\n  Sessions\n\n"
	for i, sess := range h.sessions {
		cursor := "  "
		if i == h.cursor {
			cursor = "> "
		}
		state := pb.SessionState_name[int32(sess.State)]
		s += fmt.Sprintf("  %s%-8s  %-30s  %s\n", cursor, sess.Id[:8], sess.Title, state)
	}
	s += "\n  [n]ew  [r]epo  [enter] attach  [q]uit\n"

	return tea.NewView(s)
}

// tickMsg signals a polling refresh.
type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func fetchSessions(c *client.Client, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{})
		return sessionListMsg{sessions: sessions, err: err}
	}
}
