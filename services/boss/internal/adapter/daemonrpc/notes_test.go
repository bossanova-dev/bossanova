package daemonrpc

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

func (f *fakeDaemonRPC) SyncNotesNow(_ context.Context, _ *connect.Request[pb.SyncNotesNowRequest]) (*connect.Response[pb.SyncNotesNowResponse], error) {
	return sessionResp(f, func() *pb.SyncNotesNowResponse {
		return &pb.SyncNotesNowResponse{
			IsWorkerConfigured: true,
			StateCounts:        []*pb.NoteSyncStateCount{{State: "pending", NoteCount: 3}},
		}
	})
}

// --- LocalClient: Notes ---

func TestLocalClientSyncNotesNow(t *testing.T) {
	t.Parallel()

	c := &LocalClient{rpc: &fakeDaemonRPC{}}
	resp, err := c.SyncNotesNow(context.Background())
	if err != nil {
		t.Fatalf("SyncNotesNow: unexpected error: %v", err)
	}
	if !resp.GetIsWorkerConfigured() || len(resp.GetStateCounts()) != 1 || resp.GetStateCounts()[0].GetNoteCount() != 3 {
		t.Fatalf("SyncNotesNow: unexpected response: %+v", resp)
	}

	c = &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
	if _, err := c.SyncNotesNow(context.Background()); !errors.Is(err, errRPC) {
		t.Fatalf("SyncNotesNow: expected errRPC, got %v", err)
	}
}

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
