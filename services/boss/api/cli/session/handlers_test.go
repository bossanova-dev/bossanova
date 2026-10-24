package session

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/rpcerror"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRefreshSessionPRForwardsSelectors(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		prNumber  int32
		wantID    string
		wantPR    int32
	}{
		{name: "session id only", sessionID: "sess-1", wantID: "sess-1"},
		{name: "PR only", prNumber: 42, wantPR: 42},
		{name: "both", sessionID: "sess-1", prNumber: 42, wantID: "sess-1", wantPR: 42},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionPR := int32(42)
			stub := &refreshPRStub{session: &pb.Session{
				Id:            "sess-1",
				PrNumber:      &sessionPR,
				DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
			}}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := refreshSessionPR(cmd, stub, tc.sessionID, tc.prNumber); err != nil {
				t.Fatalf("refreshSessionPR: %v", err)
			}
			if got := stub.req.GetId(); got != tc.wantID {
				t.Fatalf("id = %q, want %q", got, tc.wantID)
			}
			if got := stub.req.GetPrNumber(); got != tc.wantPR {
				t.Fatalf("pr_number = %d, want %d", got, tc.wantPR)
			}
			if !strings.Contains(out.String(), "refreshed PR") || !strings.Contains(out.String(), "passing") {
				t.Fatalf("output = %q, want refreshed PR passing line", out.String())
			}
		})
	}
}

// TestRefreshSessionPRExplainsAFinishedSession pins that refresh-pr says when
// it could not change a closed or merged session, and names the link-pr call
// that reopens it, instead of reporting a bare refresh.
func TestRefreshSessionPRExplainsAFinishedSession(t *testing.T) {
	cases := []struct {
		name    string
		state   pb.SessionState
		display pb.DisplayStatus
		want    string
	}{
		{"closed PR", pb.SessionState_SESSION_STATE_CLOSED, pb.DisplayStatus_DISPLAY_STATUS_CLOSED, "Session sess-1 stays closed: refresh-pr re-reads the PR but never reopens a session. If the work moved to an open PR, run `boss session link-pr sess-1 <pr-url>` to reopen it.\n"},
		{"reopened PR", pb.SessionState_SESSION_STATE_CLOSED, pb.DisplayStatus_DISPLAY_STATUS_CHECKING, "Session sess-1 stays closed although PR #42 is open again: refresh-pr never reopens a session. Run `boss session link-pr sess-1 42` to reopen it.\n"},
		{"merged PR", pb.SessionState_SESSION_STATE_MERGED, pb.DisplayStatus_DISPLAY_STATUS_MERGED, "Session sess-1 stays merged: refresh-pr re-reads the PR but never reopens a session. If the work moved to an open PR, run `boss session link-pr sess-1 <pr-url>` to reopen it.\n"},
		{"live session", pb.SessionState_SESSION_STATE_AWAITING_CHECKS, pb.DisplayStatus_DISPLAY_STATUS_CHECKING, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := int32(42)
			stub := &refreshPRStub{session: &pb.Session{Id: "sess-1", PrNumber: &pr, State: tc.state, DisplayStatus: tc.display}}
			cmd := &cobra.Command{}
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			if err := refreshSessionPR(cmd, stub, "sess-1", 0); err != nil {
				t.Fatalf("refreshSessionPR: %v", err)
			}
			if errOut.String() != tc.want {
				t.Fatalf("stderr = %q, want %q", errOut.String(), tc.want)
			}
			if !strings.Contains(out.String(), "refreshed PR #42") {
				t.Fatalf("stdout = %q, want the refresh line", out.String())
			}
		})
	}
	if got := finishedSessionNote(&pb.Session{Id: "sess-1", State: pb.SessionState_SESSION_STATE_CLOSED}, false); got != "Session sess-1 stays closed: only linking an open PR reopens it." {
		t.Fatalf("link-pr note = %q", got)
	}
}

type linkPRStub struct {
	resp *pb.LinkSessionPRResponse
	err  error
}

func (s *linkPRStub) LinkSessionPR(context.Context, string, string) (*pb.LinkSessionPRResponse, error) {
	return s.resp, s.err
}

// TestLinkSessionPRPrintsTheDaemonRefusalPlainly pins that link-pr shows the
// daemon's refusal of a PR another session owns as the daemon wrote it,
// without the transport code or a doubled prefix.
func TestLinkSessionPRPrintsTheDaemonRefusalPlainly(t *testing.T) {
	refusal := `PR #6606 is already linked to session s2 ("Consolidated work"); archive that session first`
	stub := &linkPRStub{err: rpcerror.WrapError(connect.NewError(connect.CodeAlreadyExists, errors.New(refusal)))}
	err := linkSessionPR(&cobra.Command{}, stub, "s1", "6606")
	if err == nil || err.Error() != "link PR: "+refusal {
		t.Fatalf("error = %v, want %q", err, "link PR: "+refusal)
	}
}

// TestLinkSessionPRNamesTheWorktreeBranchToCheckOut pins that link-pr says
// when the session worktree is not on the linked PR's branch, because a push
// from that checkout would miss the PR, and stays quiet when it is.
func TestLinkSessionPRNamesTheWorktreeBranchToCheckOut(t *testing.T) {
	url := "https://github.com/owner/repo/pull/6606"
	sess := &pb.Session{Id: "sess-1", PrUrl: &url, BranchName: "fix/source-led-planner-structured-output", State: pb.SessionState_SESSION_STATE_AWAITING_CHECKS}
	cases := []struct {
		name     string
		worktree string
		want     string
	}{
		{"worktree on another branch", "dave-madverts/mad-1594", "Session worktree is on dave-madverts/mad-1594; check out fix/source-led-planner-structured-output to push to this PR.\n"},
		{"worktree on the PR branch", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			stub := &linkPRStub{resp: &pb.LinkSessionPRResponse{Session: sess, WorktreeBranch: tc.worktree}}
			if err := linkSessionPR(cmd, stub, "sess-1", "6606"); err != nil {
				t.Fatalf("linkSessionPR: %v", err)
			}
			if out.String() != "Session sess-1 linked to PR "+url+".\n" {
				t.Fatalf("stdout = %q", out.String())
			}
			if errOut.String() != tc.want {
				t.Fatalf("stderr = %q, want %q", errOut.String(), tc.want)
			}
		})
	}
}

func TestRefreshSessionPRRequiresSelector(t *testing.T) {
	err := refreshSessionPR(&cobra.Command{}, &refreshPRStub{}, "", 0)
	if err == nil || !strings.Contains(err.Error(), "session id or --pr is required") {
		t.Fatalf("error = %v, want missing selector error", err)
	}
}

type refreshPRStub struct {
	req     *pb.RefreshSessionPRRequest
	session *pb.Session
}

func (s *refreshPRStub) RefreshSessionPR(_ context.Context, req *pb.RefreshSessionPRRequest) (*pb.Session, error) {
	s.req = req
	return s.session, nil
}

// TestListRenderersEmitFullIDs is the regression guard for the ID-truncation
// bug: CLI list/detail renderers must print the full, untruncated resource ID
// so the displayed value round-trips back into the exact-match commands
// (repo/cron/account removal accept only the full 16-char id). printChatsTable
// exercises the shared table path (MaxColWidth("ID", ids, 0) + table.New) used
// by the session/repo/trash/cron list tables; printSessionShowHeader covers the
// `boss show` printf path.
func TestListRenderersEmitFullIDs(t *testing.T) {
	const fullSessionID = "ae347e386b61682c"                   // 16-hex sqlutil.NewID form
	const fullAgentID = "550e8400-e29b-41d4-a716-446655440000" // full agent-session UUID

	t.Run("chats table shows full agent-session id", func(t *testing.T) {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printChatsTable(cmd, []*pb.AgentChat{{AgentSessionId: fullAgentID}}, nil, true)
		if !strings.Contains(buf.String(), fullAgentID) {
			t.Fatalf("chats table truncated the id; got:\n%s", buf.String())
		}
	})

	t.Run("show header shows full session id", func(t *testing.T) {
		out := captureStdout(t, func() {
			printSessionShowHeader(&pb.Session{Id: fullSessionID})
		})
		if !strings.Contains(out, fullSessionID) {
			t.Fatalf("show header truncated the id; got:\n%s", out)
		}
	})
}

// TestPrintSessionShowHeaderPrintsTheFullBlockedReason is the 2026-09-03 fix.
//
// The reason was reachable from no CLI surface at all — not `boss show`, not
// `--json`, not `boss ls` — while the TUI truncated it to 48 runes and the
// daemon, started detached, was writing its logs to /dev/null. Diagnosis ended
// in a direct sqlite3 query against bossd.db. This line is the CLI's answer, and
// it must never acquire a truncation: that is the whole point of it.
func TestPrintSessionShowHeaderPrintsTheFullBlockedReason(t *testing.T) {
	// Deliberately longer than the TUI's 48-rune hint cap, and multi-line, which
	// is the shape a real `gh pr create` failure has.
	const reason = "draft PR creation failed: create PR: HTTP 401: Requires authentication (https://api.github.com/graphql)\nTry authenticating with:  gh auth login"

	out := captureStdout(t, func() {
		printSessionShowHeader(&pb.Session{Id: "sess-1234abcd", BlockedReason: ptrTo(reason)})
	})

	if !strings.Contains(out, "Blocked:") {
		t.Fatalf("output has no Blocked line:\n%s", out)
	}
	// Every load-bearing fragment survives, including the part a 48-rune cap
	// would have cut and the part after the newline.
	for _, fragment := range []string{
		"HTTP 401: Requires authentication",
		"https://api.github.com/graphql",
		"gh auth login",
	} {
		if !strings.Contains(out, fragment) {
			t.Fatalf("output dropped %q — it must print the reason in full:\n%s", fragment, out)
		}
	}
	if strings.Contains(out, "…") {
		t.Fatalf("output was truncated; this surface exists precisely because the TUI truncates:\n%s", out)
	}
}

// TestPrintSessionShowHeaderOmitsAnEmptyBlockedReason keeps the common case
// clean: an unblocked session must not grow a dangling label.
func TestPrintSessionShowHeaderOmitsAnEmptyBlockedReason(t *testing.T) {
	out := captureStdout(t, func() {
		printSessionShowHeader(&pb.Session{Id: "sess-1234abcd"})
	})
	if strings.Contains(out, "Blocked:") {
		t.Fatalf("unblocked session printed a Blocked line:\n%s", out)
	}
}

// TestNewMergeJSONFallsBackWhenReReadFails covers the one path the end-to-end
// merge assertions cannot reach: the post-merge re-read failed, so state falls
// back to the merge response's possibly-lagging value. Without state_settled
// that fallback would silently reintroduce the pre-merge state the re-read
// exists to remove.
func TestNewMergeJSONFallsBackWhenReReadFails(t *testing.T) {
	response := &pb.Session{
		Id:    "sess-1",
		Title: "add dark mode",
		State: pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
	}

	env := newMergeJSON(response, nil, "")
	if env.Session.State != "SESSION_STATE_READY_FOR_REVIEW" {
		t.Errorf("state = %q, want the merge response's value as the fallback", env.Session.State)
	}
	if env.Session.StateSettled {
		t.Error("state_settled = true after a failed re-read")
	}

	settled := &pb.Session{
		Id:    "sess-1",
		Title: "add dark mode",
		State: pb.SessionState_SESSION_STATE_MERGED,
	}
	env = newMergeJSON(response, settled, "")
	if env.Session.State != "SESSION_STATE_MERGED" {
		t.Errorf("state = %q, want the settled value", env.Session.State)
	}
	if !env.Session.StateSettled {
		t.Error("state_settled = false after a successful re-read")
	}
	// Identity still comes from the merge response.
	if env.Session.ID != "sess-1" || env.Session.Title != "add dark mode" {
		t.Errorf("identity drifted: %+v", env.Session)
	}
}

// TestPrintChatsTable_PinnedFromBothCommands is BOS-1281's fence for the shared
// chat-table renderer. `boss show` (handlers.go runShow) and `boss chats`
// (handlers.go runChats) are the renderer's only two non-test call sites, and
// until now neither pinned the other's output — so editing the table for one
// command silently changed the other's stdout, its stderr behaviour and its
// generated CLI docs, with nothing to notice.
//
// Both subtests call printChatsTable directly, once per command's own
// *cobra.Command from the real showCmd()/chatsCmd() constructors. Note the
// limit this leaves: neither runShow nor runChats is executed here, so the
// test does NOT catch one command being forked onto a private renderer. It
// pins the single shared renderer's output, twice.
//
// The assertion is shared on purpose: an edit to printChatsTable itself fails
// both arms. Forking one command onto a renderer of its own is the case this
// does not reach — closing that needs the commands driven for real.
func TestPrintChatsTable_PinnedFromBothCommands(t *testing.T) {
	const agentID = "550e8400-e29b-41d4-a716-446655440000"
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	lastOutput := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)

	chats := []*pb.AgentChat{{
		AgentSessionId: agentID,
		Title:          "Fix the renderer",
		CreatedAt:      timestamppb.New(created),
	}}
	statuses := map[string]*pb.ChatStatusEntry{
		agentID: {
			AgentSessionId:          agentID,
			Status:                  pb.ChatStatus_CHAT_STATUS_WAITING,
			WaitingReason:           "review",
			LastOutputAt:            timestamppb.New(lastOutput),
			SpinnerPresent:          true,
			LastSubstantiveOutputAt: timestamppb.New(lastOutput),
		},
	}

	// assertSharedTable is the pin. Every expectation below is a property of
	// printChatsTable, not of either command, which is precisely why both
	// commands assert it.
	assertSharedTable := func(t *testing.T, out string) {
		t.Helper()
		// Columns, in order. Appended-not-inserted is a stated contract of the
		// renderer (a reader slicing by field index must be widened, never
		// shifted), so the ORDER is asserted rather than mere presence.
		wantHeaders := []string{"ID", "TITLE", "CREATED", "STATUS", "LAST OUTPUT", "LIVENESS"}
		at := -1
		for _, h := range wantHeaders {
			i := strings.Index(out, h)
			if i < 0 {
				t.Fatalf("header %q missing from the rendered table:\n%s", h, out)
			}
			if i <= at {
				t.Fatalf("header %q is out of order in the rendered table:\n%s", h, out)
			}
			at = i
		}
		// Cells. The full id must survive untruncated so it round-trips back
		// into the exact-match commands; the waiting reason must be inline, or
		// a reader has to run a second command to learn what the chat is
		// blocked on; the liveness cell must carry its spinner marker.
		for _, want := range []string{agentID, "Fix the renderer", "WAITING (review)", "spinner"} {
			if !strings.Contains(out, want) {
				t.Fatalf("rendered table is missing %q:\n%s", want, out)
			}
		}
	}

	t.Run("boss show", func(t *testing.T) {
		cmd := showCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printChatsTable(cmd, chats, statuses, true)
		assertSharedTable(t, buf.String())
	})

	t.Run("boss chats", func(t *testing.T) {
		cmd := chatsCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printChatsTable(cmd, chats, statuses, true)
		assertSharedTable(t, buf.String())
	})

	// The status-unavailable shape is shared too, and it is the one a reader
	// most needs to be unambiguous: "?" rather than a cell that could be
	// mistaken for a settled chat.
	t.Run("status unavailable renders ? from both commands", func(t *testing.T) {
		for name, cmd := range map[string]*cobra.Command{"show": showCmd(), "chats": chatsCmd()} {
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			printChatsTable(cmd, chats, nil, false)
			out := buf.String()
			if !strings.Contains(out, "?") {
				t.Fatalf("%s: unavailable status must render \"?\":\n%s", name, out)
			}
			if strings.Contains(out, "WAITING") {
				t.Fatalf("%s: unavailable status must not render a settled cell:\n%s", name, out)
			}
		}
	})
}
