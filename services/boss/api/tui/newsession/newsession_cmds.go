package newsession

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/boss/internal/domain/rpc"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// issueSearchDebounce is the wait between the last filter keystroke and the
// debounced server-side search. ~250ms is the sweet spot — slow enough that
// fast typists only fire one request per word, fast enough that pausing feels
// instant.
const issueSearchDebounce = 250 * time.Millisecond

func fetchRepos(c Client, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		repos, err := c.ListRepos(ctx)
		return ReposMsg{Repos: repos, Err: err}
	}
}

// fetchAgents loads the daemon's installed agent runners. Errors fall
// through silently — a failed fetch leaves agents empty, which collapses
// the wizard to its single-agent shape (skip the agent-select phase).
func fetchAgents(c Client, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		agents, err := c.ListAgents(ctx)
		return agentsMsg{agents: agents, err: err}
	}
}

func agentIndex(agents []rpc.AgentInfo, name string) int {
	if name == "" {
		return -1
	}
	for i, agent := range agents {
		if agent.Name == name {
			return i
		}
	}
	return -1
}

func fetchPRs(c Client, ctx context.Context, repoID string) tea.Cmd {
	return func() tea.Msg {
		prs, err := c.ListRepoPRs(ctx, repoID)
		return PrsMsg{PRs: prs, Err: err}
	}
}

func fetchIssues(c Client, ctx context.Context, repoID, query, source string, seq uint64) tea.Cmd {
	return func() tea.Msg {
		issues, err := c.ListTrackerIssues(ctx, repoID, query, source)
		return IssuesMsg{Issues: issues, Err: err, Seq: seq, Query: query}
	}
}

// scheduleIssueSearch schedules a debounced server-side search. The returned
// command emits a searchIssuesTickMsg after issueSearchDebounce; the handler
// for that message ignores it if a newer keystroke has incremented seq.
func scheduleIssueSearch(seq uint64, query string) tea.Cmd {
	return tea.Tick(issueSearchDebounce, func(time.Time) tea.Msg {
		return searchIssuesTickMsg{seq: seq, query: query}
	})
}

func openCreateStream(c Client, ctx context.Context, req *pb.CreateSessionRequest, id *streamIdentity) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.CreateSession(ctx, req)
		return createSessionStreamMsg{streamID: id, stream: stream, err: err}
	}
}

// readNextStreamMsg reads one meaningful frame off the create stream.
//
// accepted is the session the accepted frame carried, or nil before it has
// arrived. Since BOS-720 the daemon emits SessionCreated TWICE on the
// bootstrapping path — once when the session row is inserted (accepted; the
// bootstrap is still running) and once when the bootstrap settles — and only
// the second is terminal. The command is re-issued per frame, so the state is
// threaded through the call rather than held in view state.
//
// Carrying the session itself rather than a "have we seen one" bit is what
// keeps the deliberately single-frame paths working: the attach short-circuit
// and quick chat run no bootstrap, so their one SessionCreated is followed
// straight by EOF. Reaching EOF holding an accepted session is a completed
// create — the same drain-to-EOF, last-value-wins rule `boss new` uses — not a
// truncated stream.
func readNextStreamMsg(stream rpc.CreateSessionStream, accepted *pb.Session, id *streamIdentity) tea.Cmd {
	return func() tea.Msg {
		// Close the stream on any terminal path (error, EOF, the settled
		// SessionCreated). SetupOutput, the accepted SessionCreated, and an
		// unrecognised event are all non-terminal: the caller schedules another
		// readNextStreamMsg and the stream must stay open.
		for {
			if !stream.Receive() {
				_ = stream.Close()
				if err := stream.Err(); err != nil {
					return streamErrorMsg{streamID: id, err: err}
				}
				if accepted != nil {
					return streamSessionCreatedMsg{streamID: id, session: accepted}
				}
				return streamErrorMsg{streamID: id, err: fmt.Errorf("stream ended unexpectedly")}
			}
			msg := stream.Msg()
			switch e := msg.Event.(type) {
			case *pb.CreateSessionResponse_SetupOutput:
				return setupScriptLineMsg{streamID: id, text: e.SetupOutput.GetText()}
			case *pb.CreateSessionResponse_SessionCreated:
				if accepted == nil {
					// Closing here would abandon the setup output the user is
					// watching, and lose the settled session's
					// agent_session_id.
					return streamSessionAcceptedMsg{streamID: id, session: e.SessionCreated.GetSession()}
				}
				_ = stream.Close()
				return streamSessionCreatedMsg{streamID: id, session: e.SessionCreated.GetSession()}
			default:
				// Skip an unrecognised event rather than failing the stream, so
				// a newer daemon can add a frame type without breaking this
				// binary. The old hard error here is exactly what forced
				// BOS-720 to reuse SessionCreated instead of adding a dedicated
				// accepted event. Looping rather than recursing keeps a chatty
				// unknown-event daemon off this goroutine's stack.
				continue
			}
		}
	}
}
