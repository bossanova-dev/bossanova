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
