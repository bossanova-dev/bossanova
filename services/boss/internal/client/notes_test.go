package client

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- fakeDaemonRPC: Notes (BOS-553) ---

func (f *fakeDaemonRPC) CreateNote(_ context.Context, req *connect.Request[pb.CreateNoteRequest]) (*connect.Response[pb.CreateNoteResponse], error) {
	return sessionResp(f, func() *pb.CreateNoteResponse {
		return &pb.CreateNoteResponse{Note: &pb.Note{Id: "note-1", RepoId: req.Msg.GetRepoId(), Body: req.Msg.GetBody()}}
	})
}

func (f *fakeDaemonRPC) GetNote(_ context.Context, req *connect.Request[pb.GetNoteRequest]) (*connect.Response[pb.GetNoteResponse], error) {
	f.lastNoteGetReq = req.Msg
	return sessionResp(f, func() *pb.GetNoteResponse {
		return &pb.GetNoteResponse{Note: &pb.Note{Id: req.Msg.GetId()}}
	})
}

func (f *fakeDaemonRPC) ListNotes(_ context.Context, req *connect.Request[pb.ListNotesRequest]) (*connect.Response[pb.ListNotesResponse], error) {
	return sessionResp(f, func() *pb.ListNotesResponse {
		return &pb.ListNotesResponse{Notes: []*pb.Note{{Id: "note-1"}, {Id: "note-2"}}}
	})
}

func (f *fakeDaemonRPC) UpdateNote(_ context.Context, req *connect.Request[pb.UpdateNoteRequest]) (*connect.Response[pb.UpdateNoteResponse], error) {
	f.lastNoteUpdateReq = req.Msg
	return sessionResp(f, func() *pb.UpdateNoteResponse {
		return &pb.UpdateNoteResponse{Note: &pb.Note{Id: req.Msg.GetId(), Body: req.Msg.GetBody()}}
	})
}

func (f *fakeDaemonRPC) DeleteNote(_ context.Context, req *connect.Request[pb.DeleteNoteRequest]) (*connect.Response[pb.DeleteNoteResponse], error) {
	f.lastNoteDeleteReq = req.Msg
	return sessionResp(f, func() *pb.DeleteNoteResponse {
		return &pb.DeleteNoteResponse{}
	})
}

// --- LocalClient: Notes ---

func TestLocalClientCreateNote(t *testing.T) {
	t.Parallel()

	c := &LocalClient{rpc: &fakeDaemonRPC{}}
	note, err := c.CreateNote(context.Background(), &pb.CreateNoteRequest{RepoId: "repo-1", Body: "hello"})
	if err != nil {
		t.Fatalf("CreateNote: unexpected error: %v", err)
	}
	if note.GetId() != "note-1" || note.GetRepoId() != "repo-1" || note.GetBody() != "hello" {
		t.Fatalf("CreateNote: unexpected note: %+v", note)
	}

	c = &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
	if _, err := c.CreateNote(context.Background(), &pb.CreateNoteRequest{}); !errors.Is(err, errRPC) {
		t.Fatalf("CreateNote: expected errRPC, got %v", err)
	}
}

func TestLocalClientGetNote(t *testing.T) {
	t.Parallel()

	c := &LocalClient{rpc: &fakeDaemonRPC{}}
	note, err := c.GetNote(context.Background(), "repo-1", "note-1")
	if err != nil {
		t.Fatalf("GetNote: unexpected error: %v", err)
	}
	if note.GetId() != "note-1" {
		t.Fatalf("GetNote: unexpected note: %+v", note)
	}

	c = &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
	if _, err := c.GetNote(context.Background(), "repo-1", "note-1"); !errors.Is(err, errRPC) {
		t.Fatalf("GetNote: expected errRPC, got %v", err)
	}
}

func TestLocalClientGetNoteIgnoresRepoID(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemonRPC{}
	c := &LocalClient{rpc: fake}
	if _, err := c.GetNote(context.Background(), "repo-a", "note-1"); err != nil {
		t.Fatalf("GetNote: unexpected error: %v", err)
	}
	firstReq := fake.lastNoteGetReq

	if _, err := c.GetNote(context.Background(), "repo-b-totally-different", "note-1"); err != nil {
		t.Fatalf("GetNote: unexpected error: %v", err)
	}
	secondReq := fake.lastNoteGetReq

	if firstReq.GetId() != secondReq.GetId() {
		t.Fatalf("GetNote: id changed across differing repoID: %q vs %q", firstReq.GetId(), secondReq.GetId())
	}
	// The daemon request type carries no repo routing field at all, so a
	// differing repoID cannot have reached the wire either way — this just
	// pins that LocalClient never tries to smuggle it in.
	if secondReq.GetId() != "note-1" {
		t.Fatalf("GetNote: unexpected id on wire: %q", secondReq.GetId())
	}
}

func TestLocalClientListNotes(t *testing.T) {
	t.Parallel()

	c := &LocalClient{rpc: &fakeDaemonRPC{}}
	notes, err := c.ListNotes(context.Background(), &pb.ListNotesRequest{})
	if err != nil {
		t.Fatalf("ListNotes: unexpected error: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("ListNotes: expected 2 notes, got %d", len(notes))
	}

	c = &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
	if _, err := c.ListNotes(context.Background(), &pb.ListNotesRequest{}); !errors.Is(err, errRPC) {
		t.Fatalf("ListNotes: expected errRPC, got %v", err)
	}
}

func TestLocalClientUpdateNote(t *testing.T) {
	t.Parallel()

	body := "updated"
	c := &LocalClient{rpc: &fakeDaemonRPC{}}
	note, err := c.UpdateNote(context.Background(), "repo-1", &pb.UpdateNoteRequest{Id: "note-1", Body: &body})
	if err != nil {
		t.Fatalf("UpdateNote: unexpected error: %v", err)
	}
	if note.GetId() != "note-1" || note.GetBody() != "updated" {
		t.Fatalf("UpdateNote: unexpected note: %+v", note)
	}

	c = &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
	if _, err := c.UpdateNote(context.Background(), "repo-1", &pb.UpdateNoteRequest{Id: "note-1"}); !errors.Is(err, errRPC) {
		t.Fatalf("UpdateNote: expected errRPC, got %v", err)
	}
}

func TestLocalClientUpdateNoteIgnoresRepoID(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemonRPC{}
	c := &LocalClient{rpc: fake}
	if _, err := c.UpdateNote(context.Background(), "repo-a", &pb.UpdateNoteRequest{Id: "note-1"}); err != nil {
		t.Fatalf("UpdateNote: unexpected error: %v", err)
	}
	firstReq := fake.lastNoteUpdateReq

	if _, err := c.UpdateNote(context.Background(), "repo-b-totally-different", &pb.UpdateNoteRequest{Id: "note-1"}); err != nil {
		t.Fatalf("UpdateNote: unexpected error: %v", err)
	}
	secondReq := fake.lastNoteUpdateReq

	if firstReq.GetId() != secondReq.GetId() {
		t.Fatalf("UpdateNote: id changed across differing repoID: %q vs %q", firstReq.GetId(), secondReq.GetId())
	}
}

func TestLocalClientUpdateNotePreservesTagsPointer(t *testing.T) {
	t.Parallel()

	// Unset Tags must stay nil through the wrapper — it must not be
	// normalized to an empty NoteTagSet, which would silently mean "clear
	// all tags" instead of "leave tags alone".
	fake := &fakeDaemonRPC{}
	c := &LocalClient{rpc: fake}
	if _, err := c.UpdateNote(context.Background(), "repo-1", &pb.UpdateNoteRequest{Id: "note-1"}); err != nil {
		t.Fatalf("UpdateNote: unexpected error: %v", err)
	}
	if fake.lastNoteUpdateReq.GetTags() != nil {
		t.Fatalf("UpdateNote: expected nil Tags to stay nil, got %+v", fake.lastNoteUpdateReq.GetTags())
	}

	// A set (possibly empty) Tags pointer must pass through unchanged.
	tags := &pb.NoteTagSet{Tags: []string{"a", "b"}}
	if _, err := c.UpdateNote(context.Background(), "repo-1", &pb.UpdateNoteRequest{Id: "note-1", Tags: tags}); err != nil {
		t.Fatalf("UpdateNote: unexpected error: %v", err)
	}
	if fake.lastNoteUpdateReq.GetTags() != tags {
		t.Fatalf("UpdateNote: expected the same Tags pointer to pass through, got a different one")
	}
}

func TestLocalClientDeleteNote(t *testing.T) {
	t.Parallel()

	c := &LocalClient{rpc: &fakeDaemonRPC{}}
	if err := c.DeleteNote(context.Background(), "repo-1", "note-1"); err != nil {
		t.Fatalf("DeleteNote: unexpected error: %v", err)
	}

	c = &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
	if err := c.DeleteNote(context.Background(), "repo-1", "note-1"); !errors.Is(err, errRPC) {
		t.Fatalf("DeleteNote: expected errRPC, got %v", err)
	}
}

func TestLocalClientDeleteNoteIgnoresRepoID(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemonRPC{}
	c := &LocalClient{rpc: fake}
	if err := c.DeleteNote(context.Background(), "repo-a", "note-1"); err != nil {
		t.Fatalf("DeleteNote: unexpected error: %v", err)
	}
	firstReq := fake.lastNoteDeleteReq

	if err := c.DeleteNote(context.Background(), "repo-b-totally-different", "note-1"); err != nil {
		t.Fatalf("DeleteNote: unexpected error: %v", err)
	}
	secondReq := fake.lastNoteDeleteReq

	if firstReq.GetId() != secondReq.GetId() {
		t.Fatalf("DeleteNote: id changed across differing repoID: %q vs %q", firstReq.GetId(), secondReq.GetId())
	}
}

// --- fakeChatOrchestrator: Notes (BOS-553) ---

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
