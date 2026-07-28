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
