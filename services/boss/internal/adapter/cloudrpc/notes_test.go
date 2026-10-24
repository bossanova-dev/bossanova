package cloudrpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- fakeDaemonRPC: Notes (BOS-553) ---

func TestRemoteClientSyncNotesNowIsLocalOnly(t *testing.T) {
	t.Parallel()

	_, err := (&RemoteClient{}).SyncNotesNow(context.Background())
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented (err=%v)", connect.CodeOf(err), err)
	}
}

func (f *fakeChatOrchestrator) ProxyCreateNote(_ context.Context, req *connect.Request[pb.ProxyCreateNoteRequest]) (*connect.Response[pb.ProxyCreateNoteResponse], error) {
	f.createNoteReq = req.Msg
	return connect.NewResponse(&pb.ProxyCreateNoteResponse{
		Note: &pb.Note{Id: "note-1", RepoId: req.Msg.GetRepoId(), Body: req.Msg.GetBody()},
	}), nil
}

func (f *fakeChatOrchestrator) ProxyGetNote(_ context.Context, req *connect.Request[pb.ProxyGetNoteRequest]) (*connect.Response[pb.ProxyGetNoteResponse], error) {
	f.getNoteReq = req.Msg
	return connect.NewResponse(&pb.ProxyGetNoteResponse{
		Note: &pb.Note{Id: req.Msg.GetId(), RepoId: req.Msg.GetRepoId()},
	}), nil
}

func (f *fakeChatOrchestrator) ProxyListNotes(_ context.Context, req *connect.Request[pb.ProxyListNotesRequest]) (*connect.Response[pb.ProxyListNotesResponse], error) {
	f.listNoteReq = req.Msg
	return connect.NewResponse(&pb.ProxyListNotesResponse{
		Notes: []*pb.Note{{Id: "note-1"}, {Id: "note-2"}},
	}), nil
}

func (f *fakeChatOrchestrator) ProxyUpdateNote(_ context.Context, req *connect.Request[pb.ProxyUpdateNoteRequest]) (*connect.Response[pb.ProxyUpdateNoteResponse], error) {
	f.updateNoteReq = req.Msg
	return connect.NewResponse(&pb.ProxyUpdateNoteResponse{
		Note: &pb.Note{Id: req.Msg.GetId(), RepoId: req.Msg.GetRepoId(), Body: req.Msg.GetBody()},
	}), nil
}

func (f *fakeChatOrchestrator) ProxyDeleteNote(_ context.Context, req *connect.Request[pb.ProxyDeleteNoteRequest]) (*connect.Response[pb.ProxyDeleteNoteResponse], error) {
	f.deleteNoteReq = req.Msg
	return connect.NewResponse(&pb.ProxyDeleteNoteResponse{}), nil
}

// --- RemoteClient: Notes ---

func TestRemoteClientCreateNote(t *testing.T) {
	t.Parallel()

	c, fake := newTestRemote(t)
	sessionID := "sess-1"
	chatID := "chat-1"
	idempotencyKey := "release-marker"
	note, err := c.CreateNote(context.Background(), &pb.CreateNoteRequest{
		RepoId:         "repo-1",
		SessionId:      &sessionID,
		ChatId:         &chatID,
		Body:           "hello",
		Tags:           []string{"a", "b"},
		IdempotencyKey: &idempotencyKey,
	})
	if err != nil {
		t.Fatalf("CreateNote: unexpected error: %v", err)
	}
	if note.GetId() != "note-1" {
		t.Fatalf("CreateNote: unexpected note: %+v", note)
	}

	if fake.createNoteReq.GetRepoId() != "repo-1" {
		t.Fatalf("CreateNote: expected repo_id %q on the wire, got %q", "repo-1", fake.createNoteReq.GetRepoId())
	}
	if fake.createNoteReq.GetSessionId() != sessionID {
		t.Fatalf("CreateNote: session_id mismatch: got %q", fake.createNoteReq.GetSessionId())
	}
	if fake.createNoteReq.GetChatId() != chatID {
		t.Fatalf("CreateNote: chat_id mismatch: got %q", fake.createNoteReq.GetChatId())
	}
	if fake.createNoteReq.GetIdempotencyKey() != idempotencyKey {
		t.Fatalf("CreateNote: idempotency_key mismatch: got %q", fake.createNoteReq.GetIdempotencyKey())
	}
	if fake.createNoteReq.GetBody() != "hello" {
		t.Fatalf("CreateNote: body mismatch: got %q", fake.createNoteReq.GetBody())
	}
	if got := fake.createNoteReq.GetTags(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("CreateNote: tags mismatch: got %+v", got)
	}
}

func TestRemoteClientGetNote(t *testing.T) {
	t.Parallel()

	c, fake := newTestRemote(t)
	note, err := c.GetNote(context.Background(), "repo-1", "note-1")
	if err != nil {
		t.Fatalf("GetNote: unexpected error: %v", err)
	}
	if note.GetId() != "note-1" {
		t.Fatalf("GetNote: unexpected note: %+v", note)
	}
	if fake.getNoteReq.GetRepoId() != "repo-1" {
		t.Fatalf("GetNote: expected repo_id %q on the wire, got %q", "repo-1", fake.getNoteReq.GetRepoId())
	}
	if fake.getNoteReq.GetId() != "note-1" {
		t.Fatalf("GetNote: id mismatch: got %q", fake.getNoteReq.GetId())
	}
}

func TestRemoteClientListNotes(t *testing.T) {
	t.Parallel()

	c, fake := newTestRemote(t)
	repoID := "repo-1"
	sessionID := "sess-1"
	chatID := "chat-1"
	search := "needle"
	notes, err := c.ListNotes(context.Background(), &pb.ListNotesRequest{
		RepoId:    &repoID,
		SessionId: &sessionID,
		ChatId:    &chatID,
		Tags:      []string{"x"},
		Search:    &search,
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("ListNotes: unexpected error: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("ListNotes: expected 2 notes, got %d", len(notes))
	}

	req := fake.listNoteReq
	if req.GetRepoId() != repoID {
		t.Fatalf("ListNotes: expected repo_id %q on the wire, got %q", repoID, req.GetRepoId())
	}
	if req.GetSessionId() != sessionID {
		t.Fatalf("ListNotes: session_id mismatch: got %q", req.GetSessionId())
	}
	if req.GetChatId() != chatID {
		t.Fatalf("ListNotes: chat_id mismatch: got %q", req.GetChatId())
	}
	if req.GetSearch() != search {
		t.Fatalf("ListNotes: search mismatch: got %q", req.GetSearch())
	}
	if req.GetLimit() != 5 {
		t.Fatalf("ListNotes: limit mismatch: got %d", req.GetLimit())
	}
	if got := req.GetTags(); len(got) != 1 || got[0] != "x" {
		t.Fatalf("ListNotes: tags mismatch: got %+v", got)
	}
}

func TestRemoteClientListNotesUnsetFiltersStayUnconstrained(t *testing.T) {
	t.Parallel()

	c, fake := newTestRemote(t)
	if _, err := c.ListNotes(context.Background(), &pb.ListNotesRequest{}); err != nil {
		t.Fatalf("ListNotes: unexpected error: %v", err)
	}
	req := fake.listNoteReq
	if req.RepoId != nil {
		t.Fatalf("ListNotes: expected nil RepoId to stay unconstrained, got %v", req.RepoId)
	}
	if req.SessionId != nil || req.ChatId != nil || req.Search != nil {
		t.Fatalf("ListNotes: expected optional filters to stay nil, got %+v", req)
	}
}

func TestRemoteClientUpdateNote(t *testing.T) {
	t.Parallel()

	c, fake := newTestRemote(t)
	body := "updated"
	note, err := c.UpdateNote(context.Background(), "repo-1", &pb.UpdateNoteRequest{Id: "note-1", Body: &body})
	if err != nil {
		t.Fatalf("UpdateNote: unexpected error: %v", err)
	}
	if note.GetId() != "note-1" || note.GetBody() != "updated" {
		t.Fatalf("UpdateNote: unexpected note: %+v", note)
	}
	if fake.updateNoteReq.GetRepoId() != "repo-1" {
		t.Fatalf("UpdateNote: expected repo_id %q on the wire, got %q", "repo-1", fake.updateNoteReq.GetRepoId())
	}
}

func TestRemoteClientUpdateNotePreservesTagsSemantics(t *testing.T) {
	t.Parallel()

	// This call goes over a real HTTP round trip (httptest), so the fake
	// handler receives a freshly deserialized message, never the same Go
	// pointer the caller built — assert on content and nil-ness, not
	// pointer identity (unlike the LocalClient in-process equivalent).
	c, fake := newTestRemote(t)

	// Unset Tags must stay nil through the proxy call — must not be
	// normalized to an empty NoteTagSet, which would silently mean "clear
	// all tags" instead of "leave tags alone".
	if _, err := c.UpdateNote(context.Background(), "repo-1", &pb.UpdateNoteRequest{Id: "note-1"}); err != nil {
		t.Fatalf("UpdateNote: unexpected error: %v", err)
	}
	if fake.updateNoteReq.GetTags() != nil {
		t.Fatalf("UpdateNote: expected nil Tags to stay nil, got %+v", fake.updateNoteReq.GetTags())
	}

	// A set-but-empty Tags pointer must survive as non-nil (this is what
	// "replace with nothing" looks like) — distinct from the unset/nil case.
	if _, err := c.UpdateNote(context.Background(), "repo-1", &pb.UpdateNoteRequest{Id: "note-1", Tags: &pb.NoteTagSet{}}); err != nil {
		t.Fatalf("UpdateNote: unexpected error: %v", err)
	}
	if got := fake.updateNoteReq.GetTags(); got == nil || len(got.GetTags()) != 0 {
		t.Fatalf("UpdateNote: expected a non-nil empty Tags set, got %+v", got)
	}

	// A set Tags pointer with content must round-trip unchanged.
	if _, err := c.UpdateNote(context.Background(), "repo-1", &pb.UpdateNoteRequest{Id: "note-1", Tags: &pb.NoteTagSet{Tags: []string{"a", "b"}}}); err != nil {
		t.Fatalf("UpdateNote: unexpected error: %v", err)
	}
	if got := fake.updateNoteReq.GetTags().GetTags(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("UpdateNote: tags mismatch: got %+v", got)
	}
}

func TestRemoteClientDeleteNote(t *testing.T) {
	t.Parallel()

	c, fake := newTestRemote(t)
	if err := c.DeleteNote(context.Background(), "repo-1", "note-1"); err != nil {
		t.Fatalf("DeleteNote: unexpected error: %v", err)
	}
	if fake.deleteNoteReq.GetRepoId() != "repo-1" {
		t.Fatalf("DeleteNote: expected repo_id %q on the wire, got %q", "repo-1", fake.deleteNoteReq.GetRepoId())
	}
	if fake.deleteNoteReq.GetId() != "note-1" {
		t.Fatalf("DeleteNote: id mismatch: got %q", fake.deleteNoteReq.GetId())
	}
}
