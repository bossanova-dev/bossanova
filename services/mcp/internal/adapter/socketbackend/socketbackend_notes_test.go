package socketbackend

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// fakeDaemonNotes records each notes RPC into its OWN field and answers it from
// its OWN canned response. The per-RPC split is deliberate: a shared hook would
// let a method wired to a sibling's RPC satisfy every assertion, which is
// exactly the miswiring these tests exist to catch.
type fakeDaemonNotes struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler

	createReq *pb.CreateNoteRequest
	getReq    *pb.GetNoteRequest
	listReq   *pb.ListNotesRequest
	updateReq *pb.UpdateNoteRequest
	deleteReq *pb.DeleteNoteRequest

	createNote *pb.Note
	getNote    *pb.Note
	listNotes  []*pb.Note
	updateNote *pb.Note
}

func (f *fakeDaemonNotes) CreateNote(_ context.Context, req *connect.Request[pb.CreateNoteRequest]) (*connect.Response[pb.CreateNoteResponse], error) {
	f.createReq = req.Msg
	return connect.NewResponse(&pb.CreateNoteResponse{Note: f.createNote}), nil
}

func (f *fakeDaemonNotes) GetNote(_ context.Context, req *connect.Request[pb.GetNoteRequest]) (*connect.Response[pb.GetNoteResponse], error) {
	f.getReq = req.Msg
	return connect.NewResponse(&pb.GetNoteResponse{Note: f.getNote}), nil
}

func (f *fakeDaemonNotes) ListNotes(_ context.Context, req *connect.Request[pb.ListNotesRequest]) (*connect.Response[pb.ListNotesResponse], error) {
	f.listReq = req.Msg
	return connect.NewResponse(&pb.ListNotesResponse{Notes: f.listNotes}), nil
}

func (f *fakeDaemonNotes) UpdateNote(_ context.Context, req *connect.Request[pb.UpdateNoteRequest]) (*connect.Response[pb.UpdateNoteResponse], error) {
	f.updateReq = req.Msg
	return connect.NewResponse(&pb.UpdateNoteResponse{Note: f.updateNote}), nil
}

func (f *fakeDaemonNotes) DeleteNote(_ context.Context, req *connect.Request[pb.DeleteNoteRequest]) (*connect.Response[pb.DeleteNoteResponse], error) {
	f.deleteReq = req.Msg
	return connect.NewResponse(&pb.DeleteNoteResponse{}), nil
}

// notesBackend serves the given fake on a short Unix socket path and returns a
// Backend pointed at it. shortSocketPath keeps the path under the platform's
// ~104-byte sun_path limit, which a t.TempDir() carrying these test names would
// otherwise blow.
func notesBackend(t *testing.T, fake *fakeDaemonNotes) *Backend {
	t.Helper()

	socketPath := shortSocketPath(t)
	serveFakeDaemonAt(t, fake, socketPath)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return backend
}

// TestCreateNoteCallsRPC proves CreateNote reaches DaemonService.CreateNote with
// every request field intact — repo, both provenance fields, body and tags — and
// returns the daemon's Note.
func TestCreateNoteCallsRPC(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemonNotes{createNote: &pb.Note{Id: "note-created", RepoId: "repo-1"}}
	backend := notesBackend(t, fake)

	sessionID, chatID := "sess-1", "chat-1"
	got, err := backend.CreateNote(context.Background(), &pb.CreateNoteRequest{
		RepoId:    "repo-1",
		SessionId: &sessionID,
		ChatId:    &chatID,
		Body:      "the daemon rebased the worktree mid-run",
		Tags:      []string{"gotcha", "bossd"},
	})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}

	if fake.createReq == nil {
		t.Fatal("CreateNote did not reach DaemonService.CreateNote")
	}
	if fake.createReq.GetRepoId() != "repo-1" {
		t.Errorf("repo_id = %q, want repo-1", fake.createReq.GetRepoId())
	}
	if fake.createReq.GetSessionId() != "sess-1" || fake.createReq.GetChatId() != "chat-1" {
		t.Errorf("provenance not forwarded: session=%q chat=%q", fake.createReq.GetSessionId(), fake.createReq.GetChatId())
	}
	if fake.createReq.GetBody() != "the daemon rebased the worktree mid-run" {
		t.Errorf("body = %q, want the request body verbatim", fake.createReq.GetBody())
	}
	if len(fake.createReq.GetTags()) != 2 || fake.createReq.GetTags()[0] != "gotcha" || fake.createReq.GetTags()[1] != "bossd" {
		t.Errorf("tags = %v, want [gotcha bossd]", fake.createReq.GetTags())
	}
	if got.GetId() != "note-created" {
		t.Errorf("returned note id = %q, want note-created (the daemon's Note)", got.GetId())
	}
}

// TestGetNoteCallsRPC proves GetNote reaches DaemonService.GetNote carrying the
// id, and that the repoID parameter is genuinely unused by this adapter: the
// same note comes back whether repoID is blank or a value the daemon has never
// heard of. repoID exists for the hosted gateway, which routes with it.
func TestGetNoteCallsRPC(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemonNotes{getNote: &pb.Note{Id: "note-7", RepoId: "repo-real"}}
	backend := notesBackend(t, fake)

	got, err := backend.GetNote(context.Background(), "", "note-7")
	if err != nil {
		t.Fatalf("GetNote (blank repoID): %v", err)
	}
	if fake.getReq == nil {
		t.Fatal("GetNote did not reach DaemonService.GetNote")
	}
	if fake.getReq.GetId() != "note-7" {
		t.Errorf("id = %q, want note-7", fake.getReq.GetId())
	}
	if got.GetId() != "note-7" {
		t.Errorf("returned note id = %q, want note-7 (the daemon's Note)", got.GetId())
	}

	// A repoID the daemon has never seen must change nothing: it is ignored.
	fake.getReq = nil
	got, err = backend.GetNote(context.Background(), "repo-not-a-real-repo", "note-7")
	if err != nil {
		t.Fatalf("GetNote (bogus repoID): %v", err)
	}
	if fake.getReq.GetId() != "note-7" {
		t.Errorf("id = %q, want note-7 (repoID must not leak into the request)", fake.getReq.GetId())
	}
	if got.GetId() != "note-7" {
		t.Errorf("returned note id = %q, want note-7 regardless of repoID", got.GetId())
	}
}

// TestListNotesCallsRPC proves ListNotes reaches DaemonService.ListNotes with
// the filters intact across the wire — including that an optional filter left
// NIL arrives nil (unconstrained) while a SET one arrives set. Those two are
// different queries on the daemon, so collapsing them would silently widen or
// narrow every listing.
func TestListNotesCallsRPC(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemonNotes{listNotes: []*pb.Note{
		{Id: "note-a"},
		{Id: "note-b"},
	}}
	backend := notesBackend(t, fake)

	repoID, search := "repo-1", "rebase"
	got, err := backend.ListNotes(context.Background(), &pb.ListNotesRequest{
		RepoId: &repoID, // SET optional filter
		Search: &search, // SET optional filter
		// SessionId and ChatId deliberately left nil: unconstrained.
		Tags:  []string{"gotcha"},
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}

	if fake.listReq == nil {
		t.Fatal("ListNotes did not reach DaemonService.ListNotes")
	}
	if fake.listReq.RepoId == nil || fake.listReq.GetRepoId() != "repo-1" {
		t.Errorf("repo_id filter = %v, want set to repo-1", fake.listReq.RepoId)
	}
	if fake.listReq.Search == nil || fake.listReq.GetSearch() != "rebase" {
		t.Errorf("search filter = %v, want set to rebase", fake.listReq.Search)
	}
	if fake.listReq.SessionId != nil {
		t.Errorf("session_id filter = %v, want nil (unconstrained)", fake.listReq.SessionId)
	}
	if fake.listReq.ChatId != nil {
		t.Errorf("chat_id filter = %v, want nil (unconstrained)", fake.listReq.ChatId)
	}
	if len(fake.listReq.GetTags()) != 1 || fake.listReq.GetTags()[0] != "gotcha" {
		t.Errorf("tags filter = %v, want [gotcha]", fake.listReq.GetTags())
	}
	if fake.listReq.GetLimit() != 5 {
		t.Errorf("limit = %d, want 5", fake.listReq.GetLimit())
	}

	if len(got) != 2 || got[0].GetId() != "note-a" || got[1].GetId() != "note-b" {
		t.Errorf("returned notes = %+v, want the daemon's [note-a note-b]", got)
	}
}

// TestUpdateNoteCallsRPC proves UpdateNote reaches DaemonService.UpdateNote with
// the body and the NoteTagSet intact, and that an EXPLICITLY EMPTY NoteTagSet
// survives the wire as a set-but-empty list rather than collapsing to nil. That
// distinction is the replace-not-merge contract: set-empty means "clear every
// tag", nil means "leave the tags alone".
func TestUpdateNoteCallsRPC(t *testing.T) {
	t.Parallel()

	t.Run("body and tags", func(t *testing.T) {
		t.Parallel()

		fake := &fakeDaemonNotes{updateNote: &pb.Note{Id: "note-9", Body: "updated"}}
		backend := notesBackend(t, fake)

		body := "revised body"
		got, err := backend.UpdateNote(context.Background(), "repo-ignored", &pb.UpdateNoteRequest{
			Id:   "note-9",
			Body: &body,
			Tags: &pb.NoteTagSet{Tags: []string{"alpha", "beta"}},
		})
		if err != nil {
			t.Fatalf("UpdateNote: %v", err)
		}

		if fake.updateReq == nil {
			t.Fatal("UpdateNote did not reach DaemonService.UpdateNote")
		}
		if fake.updateReq.GetId() != "note-9" {
			t.Errorf("id = %q, want note-9", fake.updateReq.GetId())
		}
		if fake.updateReq.Body == nil || fake.updateReq.GetBody() != "revised body" {
			t.Errorf("body = %v, want set to %q", fake.updateReq.Body, "revised body")
		}
		if fake.updateReq.Tags == nil {
			t.Fatal("tags = nil, want the NoteTagSet forwarded")
		}
		if len(fake.updateReq.GetTags().GetTags()) != 2 || fake.updateReq.GetTags().GetTags()[0] != "alpha" {
			t.Errorf("tags = %v, want [alpha beta]", fake.updateReq.GetTags().GetTags())
		}
		if got.GetId() != "note-9" {
			t.Errorf("returned note id = %q, want note-9 (the daemon's Note)", got.GetId())
		}
	})

	t.Run("tags cleared", func(t *testing.T) {
		t.Parallel()

		fake := &fakeDaemonNotes{updateNote: &pb.Note{Id: "note-10"}}
		backend := notesBackend(t, fake)

		if _, err := backend.UpdateNote(context.Background(), "repo-ignored", &pb.UpdateNoteRequest{
			Id:   "note-10",
			Tags: &pb.NoteTagSet{}, // SET but empty: clear every tag.
		}); err != nil {
			t.Fatalf("UpdateNote: %v", err)
		}

		if fake.updateReq == nil {
			t.Fatal("UpdateNote did not reach DaemonService.UpdateNote")
		}
		if fake.updateReq.Tags == nil {
			t.Fatal("an explicitly empty NoteTagSet collapsed to nil: 'clear every tag' became 'leave tags alone'")
		}
		if len(fake.updateReq.GetTags().GetTags()) != 0 {
			t.Errorf("tags = %v, want a set-but-empty list", fake.updateReq.GetTags().GetTags())
		}
		if fake.updateReq.Body != nil {
			t.Errorf("body = %v, want nil (unset means leave the body alone)", fake.updateReq.Body)
		}
	})
}

// TestDeleteNoteCallsRPC proves DeleteNote reaches DaemonService.DeleteNote with
// the id, and that its repoID parameter is ignored by this adapter.
func TestDeleteNoteCallsRPC(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemonNotes{}
	backend := notesBackend(t, fake)

	if err := backend.DeleteNote(context.Background(), "repo-ignored", "note-doomed"); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if fake.deleteReq == nil {
		t.Fatal("DeleteNote did not reach DaemonService.DeleteNote")
	}
	if fake.deleteReq.GetId() != "note-doomed" {
		t.Errorf("id = %q, want note-doomed", fake.deleteReq.GetId())
	}
}

// TestNoteBodyNotRedacted proves the body is returned UNREDACTED on both read
// surfaces. Unlike a callback or broadcast message, a note body is the payload
// the caller asked for, not a secret — a redactor here would silently destroy
// the only thing notes exist to carry.
func TestNoteBodyNotRedacted(t *testing.T) {
	t.Parallel()

	const body = "PLAINTEXT-SENTINEL: token=abc123 password=hunter2 — a note body is not a secret"

	fake := &fakeDaemonNotes{
		getNote:   &pb.Note{Id: "note-1", Body: body},
		listNotes: []*pb.Note{{Id: "note-1", Body: body}},
	}
	backend := notesBackend(t, fake)

	got, err := backend.GetNote(context.Background(), "repo-1", "note-1")
	if err != nil {
		t.Fatalf("GetNote: %v", err)
	}
	if got.GetBody() != body {
		t.Errorf("GetNote body = %q, want it verbatim: %q", got.GetBody(), body)
	}

	listed, err := backend.ListNotes(context.Background(), &pb.ListNotesRequest{})
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("got %d notes, want 1", len(listed))
	}
	if listed[0].GetBody() != body {
		t.Errorf("ListNotes body = %q, want it verbatim: %q", listed[0].GetBody(), body)
	}
}
