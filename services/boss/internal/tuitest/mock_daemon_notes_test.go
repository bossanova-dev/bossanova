package tuitest_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestMockDaemonUpdateNoteWithNothingSetIsANoOp pins the mock to the daemon's
// short-circuit: an UpdateNote with neither body nor tags set changes nothing,
// so it must NOT bump updated_at (services/bossd/internal/db/note_store.go —
// "return the current note rather than bumping updated_at for a write that
// alters no field"). A mock that ticked its clock anyway would let a caller
// that silently sent an empty update look like it had done work.
//
// This is driven against the handler directly rather than through the CLI: the
// `boss notes edit` command refuses an edit with neither --body nor --tag, so
// the CLI cannot reach this path — only the MCP/API surfaces can.
func TestMockDaemonUpdateNoteWithNothingSetIsANoOp(t *testing.T) {
	d := tuitest.NewMockDaemon(t)
	ctx := context.Background()

	created, err := d.CreateNote(ctx, connect.NewRequest(&pb.CreateNoteRequest{
		RepoId: "repo-1",
		Body:   "the retry loop swallows the deadline",
		Tags:   []string{"flaky"},
	}))
	if err != nil {
		t.Fatalf("create note: %v", err)
	}
	note := created.Msg.GetNote()
	before := note.GetUpdatedAt().AsTime()

	updated, err := d.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{Id: note.GetId()}))
	if err != nil {
		t.Fatalf("update note: %v", err)
	}
	got := updated.Msg.GetNote()
	if after := got.GetUpdatedAt().AsTime(); !after.Equal(before) {
		t.Errorf("updated_at = %s, want it unchanged at %s for an empty update", after, before)
	}
	if got.GetBody() != note.GetBody() {
		t.Errorf("body = %q, want it unchanged", got.GetBody())
	}
	if len(got.GetTags()) != 1 || got.GetTags()[0] != "flaky" {
		t.Errorf("tags = %v, want them unchanged", got.GetTags())
	}

	// The stored note, not just the response, must be untouched.
	stored := d.Notes()
	if len(stored) != 1 {
		t.Fatalf("expected 1 stored note, got %d", len(stored))
	}
	if after := stored[0].GetUpdatedAt().AsTime(); !after.Equal(before) {
		t.Errorf("stored updated_at = %s, want it unchanged at %s", after, before)
	}
}

// TestMockDaemonUpdateNoteUnknownIDIsNotFound covers the other half of the
// short-circuit: the daemon confirms the row exists before deciding there is
// nothing to change, so an empty update against a missing id is still NotFound
// rather than a silent success.
func TestMockDaemonUpdateNoteUnknownIDIsNotFound(t *testing.T) {
	d := tuitest.NewMockDaemon(t)

	_, err := d.UpdateNote(context.Background(), connect.NewRequest(&pb.UpdateNoteRequest{Id: "note-missing"}))
	if err == nil {
		t.Fatalf("expected an error for an unknown note id")
	}
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", code)
	}
}

// TestMockDaemonUpdateChatTitlePersists pins the fixture behaviour the chat
// picker's inline [r]ename depends on (BOS-836): the mock must actually store
// the new title, so the next ListChats poll reports it. While UpdateChatTitle
// was inert, a scenario that renamed a chat captured the rename visibly
// reverting a moment later — a fixture artefact indistinguishable, on screen,
// from the feature being broken.
func TestMockDaemonUpdateChatTitlePersists(t *testing.T) {
	d := tuitest.NewMockDaemon(t)
	ctx := context.Background()

	d.AddChat(&pb.ClaudeChat{
		SessionId:      "sess-1",
		AgentSessionId: "agent-1",
		Title:          "Initial implementation",
	})

	if _, err := d.UpdateChatTitle(ctx, connect.NewRequest(&pb.UpdateChatTitleRequest{
		AgentSessionId: "agent-1",
		Title:          "Renamed from the chat list",
	})); err != nil {
		t.Fatalf("update chat title: %v", err)
	}

	listed, err := d.ListChats(ctx, connect.NewRequest(&pb.ListChatsRequest{SessionId: "sess-1"}))
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	chats := listed.Msg.GetChats()
	if len(chats) != 1 {
		t.Fatalf("listed %d chats, want 1", len(chats))
	}
	if got := chats[0].GetTitle(); got != "Renamed from the chat list" {
		t.Fatalf("title after rename = %q, want the new title to have persisted", got)
	}
}

// TestMockDaemonUpdateChatTitleForUnknownChatSucceeds pins the deliberate
// asymmetry with DeleteChat: renaming a chat this daemon never saw is a no-op,
// not a NotFound. Tests seed only the chats they care about, and an error here
// would surface far from its cause.
func TestMockDaemonUpdateChatTitleForUnknownChatSucceeds(t *testing.T) {
	d := tuitest.NewMockDaemon(t)

	if _, err := d.UpdateChatTitle(context.Background(), connect.NewRequest(&pb.UpdateChatTitleRequest{
		AgentSessionId: "never-seeded",
		Title:          "whatever",
	})); err != nil {
		t.Fatalf("update chat title for an unseeded chat = %v, want no error", err)
	}
}

// TestMockDaemonUpdateChatTitleCallLogIsObservable pins BOS-811's half of the
// contract: a rename is observable in BOTH directions. The stored chat carries
// the new title (so the next ListChats poll reports it), and the call log
// carries what was asked for. UpdateChatTitleResponse is an empty proto message
// — there is no field to hand the updated chat back in — so without the log a
// caller that renamed the wrong chat and one that renamed nothing at all would
// look identical from the response alone.
func TestMockDaemonUpdateChatTitleCallLogIsObservable(t *testing.T) {
	d := tuitest.NewMockDaemon(t)
	ctx := context.Background()

	d.AddChat(&pb.ClaudeChat{
		SessionId:      "sess-1",
		AgentSessionId: "agent-1",
		Title:          "Initial implementation",
	})

	if calls := d.UpdateChatTitleCalls(); len(calls) != 0 {
		t.Fatalf("call log = %d entries before any rename, want 0", len(calls))
	}

	if _, err := d.UpdateChatTitle(ctx, connect.NewRequest(&pb.UpdateChatTitleRequest{
		AgentSessionId: "agent-1",
		Title:          "Backfilled from the transcript",
	})); err != nil {
		t.Fatalf("update chat title: %v", err)
	}

	calls := d.UpdateChatTitleCalls()
	if len(calls) != 1 {
		t.Fatalf("call log = %d entries, want 1", len(calls))
	}
	if got := calls[0].GetAgentSessionId(); got != "agent-1" {
		t.Errorf("recorded agent_session_id = %q, want agent-1", got)
	}
	if got := calls[0].GetTitle(); got != "Backfilled from the transcript" {
		t.Errorf("recorded title = %q, want the requested title", got)
	}

	// The stored chat, not just the call log, must carry the new title.
	listed, err := d.ListChats(ctx, connect.NewRequest(&pb.ListChatsRequest{SessionId: "sess-1"}))
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	chats := listed.Msg.GetChats()
	if len(chats) != 1 {
		t.Fatalf("listed %d chats, want 1", len(chats))
	}
	if got := chats[0].GetTitle(); got != "Backfilled from the transcript" {
		t.Errorf("stored title = %q, want the rename to have persisted", got)
	}

	// The returned log is a copy: mutating it must not reach mock state.
	calls[0].Title = "mutated by the caller"
	if got := d.UpdateChatTitleCalls()[0].GetTitle(); got != "Backfilled from the transcript" {
		t.Errorf("call log leaked mutable state: title = %q", got)
	}
}

// TestMockDaemonReportChatStatusCallLogIsObservable pins the other half:
// ReportChatStatus records what the heartbeat pushed. Its proto response is an
// empty message and every caller discards it (heartbeat.go and attach.go both
// `_ =` the result), so before BOS-811 a heartbeat that reported nothing was
// indistinguishable from one that reported every chat correctly — the mock
// answered success either way.
func TestMockDaemonReportChatStatusCallLogIsObservable(t *testing.T) {
	d := tuitest.NewMockDaemon(t)

	if calls := d.ReportChatStatusCalls(); len(calls) != 0 {
		t.Fatalf("call log = %d entries before any report, want 0", len(calls))
	}

	reports := []*pb.ChatStatusReport{
		{AgentSessionId: "agent-1", Status: pb.ChatStatus_CHAT_STATUS_WORKING},
		{AgentSessionId: "agent-2", Status: pb.ChatStatus_CHAT_STATUS_QUESTION},
	}
	if _, err := d.ReportChatStatus(context.Background(), connect.NewRequest(&pb.ReportChatStatusRequest{
		Reports: reports,
	})); err != nil {
		t.Fatalf("report chat status: %v", err)
	}

	calls := d.ReportChatStatusCalls()
	if len(calls) != 1 {
		t.Fatalf("call log = %d entries, want 1", len(calls))
	}
	got := calls[0].GetReports()
	if len(got) != 2 {
		t.Fatalf("recorded %d reports, want 2", len(got))
	}
	if got[0].GetAgentSessionId() != "agent-1" || got[0].GetStatus() != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("report[0] = (%q, %v), want (agent-1, WORKING)", got[0].GetAgentSessionId(), got[0].GetStatus())
	}
	if got[1].GetAgentSessionId() != "agent-2" || got[1].GetStatus() != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("report[1] = (%q, %v), want (agent-2, QUESTION)", got[1].GetAgentSessionId(), got[1].GetStatus())
	}

	// The recorded request is a clone: a caller reusing its reports slice for
	// the next heartbeat tick must not rewrite what the mock already recorded.
	reports[0].Status = pb.ChatStatus_CHAT_STATUS_IDLE
	if s := d.ReportChatStatusCalls()[0].GetReports()[0].GetStatus(); s != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("call log aliased the caller's slice: status = %v, want WORKING", s)
	}
}

// TestMockDaemonUnfixturedRPCsStayUnimplemented is what turns the two-category
// invariant at the head of mock_daemon.go's unimplemented block from prose into
// a constraint. That comment says every RPC here is either fixture-backed or
// answers Unimplemented, and that the third shape — a zero-valued success with
// no fixture behind it — must never come back. BOS-811 flipped these five off
// that third shape; nothing but the comment stopped the next author flipping one
// back, because an empty success is exactly what an unfixtured view wants to
// hear and nothing in the suite would have gone red.
//
// This is the regression it catches: someone needs an answer from, say,
// ListPlugins, softens it to `return connect.NewResponse(&pb.ListPluginsResponse{}), nil`,
// and their new view test passes against a plugin list that is empty because
// nothing populates it — not because the daemon said so. That row fails here
// first, and the fix is to give the handler fixture state and an exported
// seeder (category 1), not to delete the row.
//
// The assertion is on the connect code, never the message: "not implemented" is
// incidental phrasing, CodeUnimplemented is the contract.
func TestMockDaemonUnfixturedRPCsStayUnimplemented(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		call func(*tuitest.MockDaemon) error
	}{
		{"RepairDoctor", func(d *tuitest.MockDaemon) error {
			_, err := d.RepairDoctor(ctx, connect.NewRequest(&pb.RepairDoctorRequest{}))
			return err
		}},
		{"StartRepairWorkflow", func(d *tuitest.MockDaemon) error {
			_, err := d.StartRepairWorkflow(ctx, connect.NewRequest(&pb.StartRepairWorkflowRequest{}))
			return err
		}},
		{"ListPlugins", func(d *tuitest.MockDaemon) error {
			_, err := d.ListPlugins(ctx, connect.NewRequest(&pb.ListPluginsRequest{}))
			return err
		}},
		{"GetSettings", func(d *tuitest.MockDaemon) error {
			_, err := d.GetSettings(ctx, connect.NewRequest(&pb.GetSettingsRequest{}))
			return err
		}},
		{"UpdateSettings", func(d *tuitest.MockDaemon) error {
			_, err := d.UpdateSettings(ctx, connect.NewRequest(&pb.UpdateSettingsRequest{}))
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(tuitest.NewMockDaemon(t))
			if err == nil {
				t.Fatalf("%s answered successfully; an unfixtured RPC must stay Unimplemented "+
					"rather than hand a caller a plausible empty answer (see the block comment "+
					"in mock_daemon.go)", tt.name)
			}
			if code := connect.CodeOf(err); code != connect.CodeUnimplemented {
				t.Errorf("%s code = %v, want Unimplemented", tt.name, code)
			}
		})
	}
}
