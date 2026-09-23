package server

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/tmux"
)

type deleteChatStoreFake struct {
	mu         sync.Mutex
	chat       *models.AgentChat
	getErr     error
	deleteErr  error
	operations *[]string
	// clearOnDelete models the real store, which stops resolving a row once it
	// is deleted and reports the miss as a wrapped db.ErrAgentChatNotFound
	// rather than a nil chat (internal/db/agent_chat_store.go). Opt-in so the
	// other tests in this file keep their simpler always-resolves fake.
	clearOnDelete bool
}

func (f *deleteChatStoreFake) Create(context.Context, db.CreateAgentChatParams) (*models.AgentChat, error) {
	return nil, nil
}

func (f *deleteChatStoreFake) GetByAgentSessionID(context.Context, string) (*models.AgentChat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.chat, nil
}

func (f *deleteChatStoreFake) ListBySession(context.Context, string) ([]*models.AgentChat, error) {
	return nil, nil
}

func (f *deleteChatStoreFake) ListBySessions(context.Context, []string) (map[string][]*models.AgentChat, error) {
	return nil, nil
}

func (f *deleteChatStoreFake) UpdateTitle(context.Context, string, string) error {
	return nil
}

func (f *deleteChatStoreFake) UpdateTitleByAgentSessionID(context.Context, string, string) error {
	return nil
}

func (f *deleteChatStoreFake) UpdateTmuxSessionName(context.Context, string, *string) error {
	return nil
}

func (f *deleteChatStoreFake) UpdateProviderSessionID(context.Context, string, *string) error {
	return nil
}

func (f *deleteChatStoreFake) UpdateAccountIDByAgentSessionID(context.Context, string, *string) error {
	return nil
}

func (f *deleteChatStoreFake) MarkStartFailed(context.Context, string, string) error {
	return nil
}

func (f *deleteChatStoreFake) RebindResumedChat(_ context.Context, _ string, _ db.RebindResumedChatParams) error {
	return nil
}

func (f *deleteChatStoreFake) DeleteByAgentSessionID(_ context.Context, agentSessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.operations != nil {
		*f.operations = append(*f.operations, "delete:"+agentSessionID)
	}
	if f.deleteErr == nil && f.clearOnDelete {
		f.chat = nil
		f.getErr = db.ErrAgentChatNotFound
	}
	return f.deleteErr
}

func (f *deleteChatStoreFake) ListWithTmuxSession(context.Context) ([]*models.AgentChat, error) {
	return nil, nil
}

func (f *deleteChatStoreFake) ListRoutableChats(context.Context) ([]*models.AgentChat, error) {
	return nil, nil
}

func TestDeleteChat_KillsTmuxSessionBeforeDeletingRow(t *testing.T) {
	ctx := context.Background()
	tmuxName := "boss-repo1234-agent5678"
	agentSessionID := "agent-5678"
	operations := []string{}

	srv := &Server{
		agentChats: &deleteChatStoreFake{
			operations: &operations,
			chat: &models.AgentChat{
				SessionID:       "session-1",
				AgentSessionID:  agentSessionID,
				TmuxSessionName: &tmuxName,
			},
		},
		chatStatus: status.NewTracker(),
		tmux: tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			if len(args) > 0 && args[0] == "kill-session" {
				target := ""
				if len(args) >= 3 && args[1] == "-t" {
					target = args[2]
				}
				operations = append(operations, "kill:"+target)
			}
			return exec.CommandContext(ctx, "true")
		})),
	}

	_, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
	}))
	if err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	want := []string{"kill:" + tmuxName, "delete:" + agentSessionID}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}
}

func TestDeleteChat_RejectsChatFromADifferentSession(t *testing.T) {
	ctx := context.Background()
	agentSessionID := "agent-5678"
	operations := []string{}

	srv := &Server{
		agentChats: &deleteChatStoreFake{
			operations: &operations,
			chat: &models.AgentChat{
				SessionID:      "session-OWNER",
				AgentSessionID: agentSessionID,
			},
		},
		chatStatus: status.NewTracker(),
	}

	// Caller authorized "session-OTHER" but the chat belongs to
	// "session-OWNER": the daemon must refuse and must NOT delete anything.
	_, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
		SessionId:      "session-OTHER",
	}))
	if err == nil {
		t.Fatal("expected DeleteChat to reject cross-session delete, got nil error")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("error code = %v, want NotFound", connect.CodeOf(err))
	}
	if len(operations) != 0 {
		t.Fatalf("expected no delete/kill operations on rejection, got %#v", operations)
	}
}

func TestDeleteChat_AllowsMatchingSessionScope(t *testing.T) {
	ctx := context.Background()
	agentSessionID := "agent-5678"
	operations := []string{}

	srv := &Server{
		agentChats: &deleteChatStoreFake{
			operations: &operations,
			chat: &models.AgentChat{
				SessionID:      "session-1",
				AgentSessionID: agentSessionID,
			},
		},
		chatStatus: status.NewTracker(),
	}

	_, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
		SessionId:      "session-1",
	}))
	if err != nil {
		t.Fatalf("DeleteChat with matching scope: %v", err)
	}
	want := []string{"delete:" + agentSessionID}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}
}

func TestDeleteChat_MissingChatIsIdempotentAndDoesNotKillTmux(t *testing.T) {
	ctx := context.Background()
	agentSessionID := "agent-missing"
	operations := []string{}

	srv := &Server{
		agentChats: &deleteChatStoreFake{
			operations: &operations,
			getErr:     db.ErrAgentChatNotFound,
		},
		chatStatus: status.NewTracker(),
		tmux: tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			if len(args) > 0 && args[0] == "kill-session" {
				operations = append(operations, "kill")
			}
			return exec.CommandContext(ctx, "true")
		})),
	}

	_, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
	}))
	if err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	want := []string{"delete:" + agentSessionID}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}
}

// AC3, and the ordering the whole criterion rests on. DeleteChat clears the
// cached status at server.go:4607 -- while the chat row is still present --
// and only deletes the row afterwards. The eviction hook therefore fires with
// the row still resolvable, so the recompute finds its session and the label
// settles; had Remove been called after the delete, the lookup would miss and
// the session's label would stay frozen with nothing left to re-emit it.
//
// The fake stops resolving the row once it is deleted, exactly as the real
// store does, so a regression that moves the eviction after the delete records
// recompute-missed instead of recompute and fails on both count and order.
func TestDeleteChat_RecomputesWhileTheChatRowIsStillPresent(t *testing.T) {
	ctx := context.Background()
	agentSessionID := "agent-5678"
	operations := []string{}

	store := &deleteChatStoreFake{
		operations:    &operations,
		clearOnDelete: true,
		chat: &models.AgentChat{
			SessionID:      "session-1",
			AgentSessionID: agentSessionID,
		},
	}

	tracker := status.NewTracker()
	// A live entry, so DeleteChat's Remove has something to evict and the
	// eviction hook fires at all.
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	// Stands in for cmd's wireEvictionRecompute, which lives in package main and
	// is out of reach here. Same shape: resolve each evicted id to its session,
	// then recompute that session.
	tracker.SetOnEntriesEvicted(func(agentSessionIDs []string) {
		for _, id := range agentSessionIDs {
			chat, err := store.GetByAgentSessionID(ctx, id)
			if err != nil || chat == nil {
				operations = append(operations, "recompute-missed:"+id)
				continue
			}
			operations = append(operations, "recompute:"+chat.SessionID)
		}
	})

	srv := &Server{agentChats: store, chatStatus: tracker}

	if _, err := srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
	})); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	want := []string{"recompute:session-1", "delete:" + agentSessionID}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}
}

// --- Typed deletion reason: audit line + fail-closed recorded-agent gate ---

// deleteChatFixture builds the Server/fake/log triple every reason test below
// needs. It keeps the established idioms of this file -- bare *Server struct
// literal, hand-written deleteChatStoreFake, tmux faked through a command
// factory, an ordered operations slice -- and only adds the log capture
// (zerolog.New(&logs)) that the pre-existing tests deliberately omit.
type deleteChatFixture struct {
	srv        *Server
	store      *deleteChatStoreFake
	operations *[]string
	logs       *bytes.Buffer
}

func newDeleteChatFixture(chat *models.AgentChat, getErr error) *deleteChatFixture {
	operations := []string{}
	logs := &bytes.Buffer{}
	store := &deleteChatStoreFake{operations: &operations, chat: chat, getErr: getErr}
	srv := &Server{
		agentChats: store,
		chatStatus: status.NewTracker(),
		logger:     zerolog.New(logs),
		tmux: tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			if len(args) > 0 && args[0] == "kill-session" {
				target := ""
				if len(args) >= 3 && args[1] == "-t" {
					target = args[2]
				}
				operations = append(operations, "kill:"+target)
			}
			return exec.CommandContext(ctx, "true")
		})),
	}
	return &deleteChatFixture{srv: srv, store: store, operations: &operations, logs: logs}
}

// deleteChatCodexRow is the incident's shape: a Codex sibling chat living in a
// Claude-owned session, with a live tmux pane.
func deleteChatCodexRow(tmuxName *string) *models.AgentChat {
	return &models.AgentChat{
		SessionID:       "session-1",
		AgentSessionID:  "agent-5678",
		AgentName:       "codex",
		Title:           "Improve video ad generation",
		TmuxSessionName: tmuxName,
	}
}

// TestDeleteChat_CleanupReasonPreservesCodexChat is the incident, inverted
// (AC2). A Claude-transcript-absence reap reached a Codex chat and destroyed
// it. The daemon must now refuse: the row survives, the pane is untouched, the
// caller gets FailedPrecondition, and one warn line says so.
func TestDeleteChat_CleanupReasonPreservesCodexChat(t *testing.T) {
	ctx := context.Background()
	tmuxName := "boss-30337857-769495d7"
	f := newDeleteChatFixture(deleteChatCodexRow(&tmuxName), nil)

	_, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: "agent-5678",
		SessionId:      "session-1",
		Reason:         pb.DeleteChatRequest_DELETION_REASON_CLEANUP_LOCAL_CLAUDE_TRANSCRIPT_ABSENT,
	}))

	// Preservation, asserted on its own: nothing was killed and nothing was
	// deleted. An empty operations slice is the whole point of the gate.
	if !reflect.DeepEqual(*f.operations, []string{}) {
		t.Fatalf("operations = %#v, want no kill and no delete", *f.operations)
	}
	if err == nil {
		t.Fatal("expected DeleteChat to refuse a cleanup reap of a codex chat, got nil error")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("error code = %v, want FailedPrecondition", connect.CodeOf(err))
	}

	// The warning, asserted separately: a quiet refusal is indistinguishable
	// from a no-op, so the level and the named fields are part of the contract.
	logs := f.logs.Bytes()
	for _, want := range [][]byte{
		[]byte(`"level":"warn"`),
		[]byte(`"agent_name":"codex"`),
		[]byte(`"reason":"DELETION_REASON_CLEANUP_LOCAL_CLAUDE_TRANSCRIPT_ABSENT"`),
		[]byte(`"agent_session_id":"agent-5678"`),
		[]byte(`"session_id":"session-1"`),
	} {
		if !bytes.Contains(logs, want) {
			t.Fatalf("refusal log %q missing %q", logs, want)
		}
	}
}

// TestDeleteChat_CleanupReasonDeletesClaudeChat pins that the intended cleanup
// still works -- the gate must not regress the reap it exists to permit (AC3).
func TestDeleteChat_CleanupReasonDeletesClaudeChat(t *testing.T) {
	ctx := context.Background()
	f := newDeleteChatFixture(&models.AgentChat{
		SessionID:      "session-1",
		AgentSessionID: "agent-5678",
		AgentName:      "claude",
	}, nil)

	if _, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: "agent-5678",
		Reason:         pb.DeleteChatRequest_DELETION_REASON_CLEANUP_LOCAL_CLAUDE_TRANSCRIPT_ABSENT,
	})); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if want := []string{"delete:agent-5678"}; !reflect.DeepEqual(*f.operations, want) {
		t.Fatalf("operations = %#v, want %#v", *f.operations, want)
	}
}

// TestDeleteChat_CleanupReasonDeletesLegacyEmptyAgentChat is the regression
// guard for the naive-comparison bug (AC3). A row predating the agent_name
// column carries "", which resolves to claude by the same legacy default the
// rest of bossd uses -- so it must still be reaped. A gate written as
// `chat.AgentName != "claude"` passes every other test in this file and
// strands every legacy row forever.
func TestDeleteChat_CleanupReasonDeletesLegacyEmptyAgentChat(t *testing.T) {
	ctx := context.Background()
	f := newDeleteChatFixture(&models.AgentChat{
		SessionID:      "session-1",
		AgentSessionID: "agent-5678",
		AgentName:      "",
	}, nil)

	if _, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: "agent-5678",
		Reason:         pb.DeleteChatRequest_DELETION_REASON_CLEANUP_LOCAL_CLAUDE_TRANSCRIPT_ABSENT,
	})); err != nil {
		t.Fatalf("DeleteChat of a legacy empty-agent row: %v", err)
	}
	if want := []string{"delete:agent-5678"}; !reflect.DeepEqual(*f.operations, want) {
		t.Fatalf("operations = %#v, want %#v", *f.operations, want)
	}
}

// TestDeleteChat_UngatedReasonsDeleteACodexChat pins AC4: only the cleanup
// reason is gated. Explicit user deletion, agent-initiated deletion, and the
// zero value (every pre-existing caller) all delete a non-Claude chat exactly
// as they did before this field existed. The UNSPECIFIED row is also the test
// form of R4's no-bump argument: a pinned client cannot set the reason, so it
// can never reach the refusal.
func TestDeleteChat_UngatedReasonsDeleteACodexChat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason pb.DeleteChatRequest_DeletionReason
	}{
		{"user requested", pb.DeleteChatRequest_DELETION_REASON_USER_REQUESTED},
		{"agent requested", pb.DeleteChatRequest_DELETION_REASON_AGENT_REQUESTED},
		{"unspecified", pb.DeleteChatRequest_DELETION_REASON_UNSPECIFIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tmuxName := "boss-30337857-769495d7"
			f := newDeleteChatFixture(deleteChatCodexRow(&tmuxName), nil)

			if _, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
				AgentSessionId: "agent-5678",
				Reason:         tc.reason,
			})); err != nil {
				t.Fatalf("DeleteChat: %v", err)
			}
			want := []string{"kill:" + tmuxName, "delete:agent-5678"}
			if !reflect.DeepEqual(*f.operations, want) {
				t.Fatalf("operations = %#v, want %#v", *f.operations, want)
			}
			if bytes.Contains(f.logs.Bytes(), []byte(`"level":"warn"`)) {
				t.Fatalf("ungated delete logged a refusal: %s", f.logs.Bytes())
			}
		})
	}
}

// TestDeleteChat_CleanupReasonWithNoChatRowIsIdempotent pins the nil-chat row
// of the decision table: there is nothing to protect, so the gate must not
// convert an idempotent no-op into a refusal.
func TestDeleteChat_CleanupReasonWithNoChatRowIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newDeleteChatFixture(nil, db.ErrAgentChatNotFound)

	if _, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: "agent-missing",
		Reason:         pb.DeleteChatRequest_DELETION_REASON_CLEANUP_LOCAL_CLAUDE_TRANSCRIPT_ABSENT,
	})); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if want := []string{"delete:agent-missing"}; !reflect.DeepEqual(*f.operations, want) {
		t.Fatalf("operations = %#v, want %#v", *f.operations, want)
	}
	if logs := f.logs.Bytes(); !bytes.Contains(logs, []byte(`"chat_absent":true`)) {
		t.Fatalf("absent-chat log %q missing chat_absent", logs)
	}
}

// TestDeleteChat_EmitsStructuredAuditLine pins AC5. The incident had to be
// reconstructed from tmux reaper logs because the deletion itself left nothing
// behind; one line carrying the identifiers, the recorded agent and the reason
// is what replaces that reconstruction. The negative assertion is half the
// criterion: an audit line that leaks the chat title (or anything else
// transcript-shaped) trades one defect for a worse one.
func TestDeleteChat_EmitsStructuredAuditLine(t *testing.T) {
	ctx := context.Background()
	tmuxName := "boss-30337857-769495d7"
	f := newDeleteChatFixture(deleteChatCodexRow(&tmuxName), nil)

	if _, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: "agent-5678",
		SessionId:      "session-1",
		Reason:         pb.DeleteChatRequest_DELETION_REASON_USER_REQUESTED,
	})); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	logs := f.logs.Bytes()
	for _, want := range [][]byte{
		[]byte(`"level":"info"`),
		[]byte(`"session_id":"session-1"`),
		[]byte(`"agent_session_id":"agent-5678"`),
		[]byte(`"agent_name":"codex"`),
		[]byte(`"reason":"DELETION_REASON_USER_REQUESTED"`),
		[]byte(`"tmux_session":"` + tmuxName + `"`),
	} {
		if !bytes.Contains(logs, want) {
			t.Fatalf("audit log %q missing %q", logs, want)
		}
	}
	// Exactly one line per deletion attempt.
	if got := bytes.Count(bytes.TrimSpace(logs), []byte("\n")) + 1; got != 1 {
		t.Fatalf("audit emitted %d lines, want 1: %s", got, logs)
	}
	// Redaction contract: identifiers only, never chat content.
	if bytes.Contains(logs, []byte("Improve video ad generation")) {
		t.Fatalf("audit log leaked the chat title: %s", logs)
	}
	// The reason is logged as its proto name, never as the raw integer.
	if bytes.Contains(logs, []byte(`"reason":1`)) {
		t.Fatalf("audit log recorded the reason as an integer: %s", logs)
	}
}

// TestDeleteChat_AuditsCrossSessionDenial is the security-relevant half of AC5:
// "every deletion ATTEMPT" includes the ones that are refused before the gate
// is even reached. The session-scope mismatch is an authz denial -- a caller
// asking the daemon to destroy a chat it was not authorized for -- and until
// this line existed it returned NotFound and left nothing behind at all, which
// is precisely the forensic void BOS-1299 exists to close. The denial must also
// say WHICH session was asked for; the owning session alone does not describe
// the attempt.
func TestDeleteChat_AuditsCrossSessionDenial(t *testing.T) {
	ctx := context.Background()
	tmuxName := "boss-30337857-769495d7"
	f := newDeleteChatFixture(deleteChatCodexRow(&tmuxName), nil)

	_, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: "agent-5678",
		SessionId:      "session-2",
		Reason:         pb.DeleteChatRequest_DELETION_REASON_USER_REQUESTED,
	}))
	if err == nil {
		t.Fatal("expected DeleteChat to deny a cross-session delete, got nil error")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("error code = %v, want NotFound", connect.CodeOf(err))
	}
	// Nothing was killed and nothing was deleted: the denial is a denial.
	if !reflect.DeepEqual(*f.operations, []string{}) {
		t.Fatalf("operations = %#v, want no kill and no delete", *f.operations)
	}

	logs := f.logs.Bytes()
	for _, want := range [][]byte{
		// Warn, not Info: a quiet denial is indistinguishable from a no-op.
		[]byte(`"level":"warn"`),
		[]byte(`"outcome":"cross-session"`),
		[]byte(`"session_id":"session-1"`),
		[]byte(`"requested_session_id":"session-2"`),
		[]byte(`"agent_session_id":"agent-5678"`),
		[]byte(`"agent_name":"codex"`),
		[]byte(`"reason":"DELETION_REASON_USER_REQUESTED"`),
		[]byte(`"tmux_session":"` + tmuxName + `"`),
	} {
		if !bytes.Contains(logs, want) {
			t.Fatalf("cross-session audit log %q missing %q", logs, want)
		}
	}
	if got := bytes.Count(bytes.TrimSpace(logs), []byte("\n")) + 1; got != 1 {
		t.Fatalf("audit emitted %d lines, want 1: %s", got, logs)
	}
	// The redaction contract holds on the denial path too.
	if bytes.Contains(logs, []byte("Improve video ad generation")) {
		t.Fatalf("cross-session audit log leaked the chat title: %s", logs)
	}
}

// TestDeleteChat_AuditsMissingAgentSessionID pins the first of the three early
// returns that used to escape the audit entirely (AC5). There is no chat to
// name, so the line carries chat_absent rather than inventing an agent.
func TestDeleteChat_AuditsMissingAgentSessionID(t *testing.T) {
	ctx := context.Background()
	f := newDeleteChatFixture(nil, nil)

	_, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		SessionId: "session-1",
		Reason:    pb.DeleteChatRequest_DELETION_REASON_USER_REQUESTED,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("error code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	logs := f.logs.Bytes()
	for _, want := range [][]byte{
		[]byte(`"level":"warn"`),
		[]byte(`"outcome":"invalid-argument"`),
		[]byte(`"session_id":"session-1"`),
		[]byte(`"chat_absent":true`),
		[]byte(`"reason":"DELETION_REASON_USER_REQUESTED"`),
	} {
		if !bytes.Contains(logs, want) {
			t.Fatalf("invalid-argument audit log %q missing %q", logs, want)
		}
	}
	if got := bytes.Count(bytes.TrimSpace(logs), []byte("\n")) + 1; got != 1 {
		t.Fatalf("audit emitted %d lines, want 1: %s", got, logs)
	}
}

// TestDeleteChat_AuditsLookupFailure pins the second escaped return (AC5). A
// store that cannot answer is a deletion attempt that happened and did nothing,
// and it must not be silent. The store error itself stays OUT of the line: the
// redaction contract is a closed field set, and the error still reaches the
// caller as the RPC error.
func TestDeleteChat_AuditsLookupFailure(t *testing.T) {
	ctx := context.Background()
	f := newDeleteChatFixture(nil, errors.New("sqlite: disk I/O error on chats.db"))

	_, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: "agent-5678",
		SessionId:      "session-1",
		Reason:         pb.DeleteChatRequest_DELETION_REASON_CLEANUP_LOCAL_CLAUDE_TRANSCRIPT_ABSENT,
	}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("error code = %v, want Internal", connect.CodeOf(err))
	}
	if !reflect.DeepEqual(*f.operations, []string{}) {
		t.Fatalf("operations = %#v, want no kill and no delete", *f.operations)
	}

	logs := f.logs.Bytes()
	for _, want := range [][]byte{
		[]byte(`"level":"warn"`),
		[]byte(`"outcome":"lookup-failed"`),
		[]byte(`"agent_session_id":"agent-5678"`),
		[]byte(`"chat_absent":true`),
		[]byte(`"reason":"DELETION_REASON_CLEANUP_LOCAL_CLAUDE_TRANSCRIPT_ABSENT"`),
	} {
		if !bytes.Contains(logs, want) {
			t.Fatalf("lookup-failure audit log %q missing %q", logs, want)
		}
	}
	if bytes.Contains(logs, []byte("disk I/O error")) {
		t.Fatalf("lookup-failure audit log leaked the store error: %s", logs)
	}
	if got := bytes.Count(bytes.TrimSpace(logs), []byte("\n")) + 1; got != 1 {
		t.Fatalf("audit emitted %d lines, want 1: %s", got, logs)
	}
}

// TestDeleteChat_AllowedDeleteNamesItsOutcome pins the positive half of the
// outcome discriminator: the allowed path stays Info and says so, so a reader
// filtering on outcome sees every attempt, not only the failures.
func TestDeleteChat_AllowedDeleteNamesItsOutcome(t *testing.T) {
	ctx := context.Background()
	tmuxName := "boss-30337857-769495d7"
	f := newDeleteChatFixture(deleteChatCodexRow(&tmuxName), nil)

	if _, err := f.srv.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: "agent-5678",
		SessionId:      "session-1",
		Reason:         pb.DeleteChatRequest_DELETION_REASON_USER_REQUESTED,
	})); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	logs := f.logs.Bytes()
	if !bytes.Contains(logs, []byte(`"outcome":"allowed"`)) {
		t.Fatalf("allowed audit log %q missing outcome", logs)
	}
	if !bytes.Contains(logs, []byte(`"level":"info"`)) {
		t.Fatalf("allowed audit log %q is not at info level", logs)
	}
	// The owning session and the requested one agree, so no second id.
	if bytes.Contains(logs, []byte("requested_session_id")) {
		t.Fatalf("allowed audit log emitted requested_session_id needlessly: %s", logs)
	}
}
