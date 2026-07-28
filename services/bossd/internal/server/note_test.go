package server

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// newNoteServer builds a Server backed by a real migrated in-memory note store
// so the RPC handlers exercise the actual SQL path rather than a fake.
func newNoteServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		notes:  db.NewNoteStore(setupServerTestDB(t)),
		logger: zerolog.Nop(),
	}
}

// mustCreateNoteRPC creates a note through the RPC surface.
func mustCreateNoteRPC(t *testing.T, srv *Server, req *pb.CreateNoteRequest) *pb.Note {
	t.Helper()
	resp, err := srv.CreateNote(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	return resp.Msg.Note
}

func strp(s string) *string { return &s }

// assertConnectCode fails unless err is a connect error with the wanted code.
func assertConnectCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error with code %v, got nil", want)
	}
	if got := connect.CodeOf(err); got != want {
		t.Fatalf("code = %v, want %v (err: %v)", got, want, err)
	}
}

func TestCreateNote_HappyPathNormalizesTags(t *testing.T) {
	srv := newNoteServer(t)

	note := mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{
		RepoId:    "repo-1",
		SessionId: strp("session-1"),
		ChatId:    strp("chat-1"),
		Body:      "what this run learned",
		Tags:      []string{"Tech-Debt", "tech-debt", "  Cron  "},
	})

	if note.Id == "" {
		t.Error("expected a non-empty id")
	}
	if note.RepoId != "repo-1" {
		t.Errorf("repo id = %q, want repo-1", note.RepoId)
	}
	if note.SessionId != "session-1" || note.ChatId != "chat-1" {
		t.Errorf("provenance = %q/%q, want session-1/chat-1", note.SessionId, note.ChatId)
	}
	if note.Body != "what this run learned" {
		t.Errorf("body = %q, want it carried through", note.Body)
	}
	if !reflect.DeepEqual(note.Tags, []string{"cron", "tech-debt"}) {
		t.Errorf("tags = %v, want [cron tech-debt] (trimmed, lowercased, de-duplicated)", note.Tags)
	}
	if note.CreatedAt == nil || note.UpdatedAt == nil {
		t.Errorf("timestamps unset: created=%v updated=%v", note.CreatedAt, note.UpdatedAt)
	}
}

func TestCreateNote_OmittedProvenanceIsEmpty(t *testing.T) {
	srv := newNoteServer(t)
	note := mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{RepoId: "repo-1", Body: "body"})
	if note.SessionId != "" || note.ChatId != "" {
		t.Errorf("provenance = %q/%q, want empty when unset", note.SessionId, note.ChatId)
	}
	if len(note.Tags) != 0 {
		t.Errorf("tags = %v, want empty", note.Tags)
	}
}

func TestCreateNote_ValidationIsInvalidArgument(t *testing.T) {
	srv := newNoteServer(t)
	ctx := context.Background()

	cases := []struct {
		name string
		req  *pb.CreateNoteRequest
	}{
		{name: "missing repo id", req: &pb.CreateNoteRequest{Body: "body"}},
		{name: "empty body", req: &pb.CreateNoteRequest{RepoId: "repo-1", Body: "  "}},
		{
			name: "oversize body",
			req: &pb.CreateNoteRequest{
				RepoId: "repo-1", Body: strings.Repeat("x", models.NoteMaxBodyBytes+1),
			},
		},
		{
			name: "oversize tag",
			req: &pb.CreateNoteRequest{
				RepoId: "repo-1", Body: "body",
				Tags: []string{strings.Repeat("t", models.NoteMaxTagLength+1)},
			},
		},
		{
			name: "empty tag",
			req:  &pb.CreateNoteRequest{RepoId: "repo-1", Body: "body", Tags: []string{" "}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.CreateNote(ctx, connect.NewRequest(tc.req))
			assertConnectCode(t, err, connect.CodeInvalidArgument)
		})
	}
}

func TestGetNote(t *testing.T) {
	srv := newNoteServer(t)
	ctx := context.Background()

	created := mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{
		RepoId: "repo-1", Body: "body", Tags: []string{"alpha"},
	})

	resp, err := srv.GetNote(ctx, connect.NewRequest(&pb.GetNoteRequest{Id: created.Id}))
	if err != nil {
		t.Fatalf("GetNote: %v", err)
	}
	if resp.Msg.Note.Id != created.Id {
		t.Errorf("id = %q, want %q", resp.Msg.Note.Id, created.Id)
	}
	if !reflect.DeepEqual(resp.Msg.Note.Tags, []string{"alpha"}) {
		t.Errorf("tags = %v, want [alpha]", resp.Msg.Note.Tags)
	}

	t.Run("missing id is not found", func(t *testing.T) {
		_, err := srv.GetNote(ctx, connect.NewRequest(&pb.GetNoteRequest{Id: "nope"}))
		assertConnectCode(t, err, connect.CodeNotFound)
	})

	t.Run("blank id is invalid argument", func(t *testing.T) {
		_, err := srv.GetNote(ctx, connect.NewRequest(&pb.GetNoteRequest{Id: "  "}))
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})
}

func TestListNotes(t *testing.T) {
	srv := newNoteServer(t)
	ctx := context.Background()

	a := mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{
		RepoId: "repo-1", SessionId: strp("session-1"),
		Body: "Refactored the cron scheduler", Tags: []string{"tech-debt", "cron"},
	})
	b := mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{
		RepoId: "repo-1", Body: "Flaky webhook test", Tags: []string{"testing"},
	})
	mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{RepoId: "repo-2", Body: "elsewhere"})

	idSet := func(notes []*pb.Note) map[string]bool {
		out := map[string]bool{}
		for _, n := range notes {
			out[n.Id] = true
		}
		return out
	}

	cases := []struct {
		name string
		req  *pb.ListNotesRequest
		want map[string]bool
	}{
		{name: "no filter", req: &pb.ListNotesRequest{}, want: nil}, // checked by count below
		{
			name: "by repo",
			req:  &pb.ListNotesRequest{RepoId: strp("repo-1")},
			want: map[string]bool{a.Id: true, b.Id: true},
		},
		{
			name: "by session",
			req:  &pb.ListNotesRequest{SessionId: strp("session-1")},
			want: map[string]bool{a.Id: true},
		},
		{
			name: "tags are any-of",
			req:  &pb.ListNotesRequest{Tags: []string{"cron", "testing"}},
			want: map[string]bool{a.Id: true, b.Id: true},
		},
		{
			name: "search",
			req:  &pb.ListNotesRequest{Search: strp("FLAKY")},
			want: map[string]bool{b.Id: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.ListNotes(ctx, connect.NewRequest(tc.req))
			if err != nil {
				t.Fatalf("ListNotes: %v", err)
			}
			if tc.want == nil {
				if len(resp.Msg.Notes) != 3 {
					t.Fatalf("unfiltered notes = %d, want 3", len(resp.Msg.Notes))
				}
				return
			}
			if got := idSet(resp.Msg.Notes); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ids = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("limit caps the result", func(t *testing.T) {
		resp, err := srv.ListNotes(ctx, connect.NewRequest(&pb.ListNotesRequest{Limit: 1}))
		if err != nil {
			t.Fatalf("ListNotes: %v", err)
		}
		if len(resp.Msg.Notes) != 1 {
			t.Errorf("notes = %d, want 1", len(resp.Msg.Notes))
		}
	})

	t.Run("zero limit is unlimited", func(t *testing.T) {
		resp, err := srv.ListNotes(ctx, connect.NewRequest(&pb.ListNotesRequest{Limit: 0}))
		if err != nil {
			t.Fatalf("ListNotes: %v", err)
		}
		if len(resp.Msg.Notes) != 3 {
			t.Errorf("notes = %d, want all 3", len(resp.Msg.Notes))
		}
	})

	// An all-blank tag list reaches the handler verbatim from the CLI/MCP
	// surfaces (`--tag ""`, `{"tags": [""]}`). It must narrow to nothing, not
	// widen to the whole table — the "zero limit is unlimited" case above
	// establishes that 3 notes are there to be wrongly returned.
	t.Run("blank tag filter matches nothing rather than everything", func(t *testing.T) {
		resp, err := srv.ListNotes(ctx, connect.NewRequest(&pb.ListNotesRequest{Tags: []string{""}}))
		if err != nil {
			t.Fatalf("ListNotes: %v", err)
		}
		if len(resp.Msg.Notes) != 0 {
			t.Errorf("notes = %d, want 0 — a blank tag filter must not fall through to an unfiltered dump", len(resp.Msg.Notes))
		}
	})
}

func TestUpdateNote(t *testing.T) {
	srv := newNoteServer(t)
	ctx := context.Background()

	newNote := func(t *testing.T) *pb.Note {
		t.Helper()
		return mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{
			RepoId: "repo-1", Body: "old body", Tags: []string{"keep-a", "keep-b"},
		})
	}

	t.Run("body only leaves tags alone", func(t *testing.T) {
		note := newNote(t)
		resp, err := srv.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{
			Id: note.Id, Body: strp("new body"),
		}))
		if err != nil {
			t.Fatalf("UpdateNote: %v", err)
		}
		if resp.Msg.Note.Body != "new body" {
			t.Errorf("body = %q, want new body", resp.Msg.Note.Body)
		}
		if !reflect.DeepEqual(resp.Msg.Note.Tags, []string{"keep-a", "keep-b"}) {
			t.Errorf("tags = %v, want untouched — an unset tag field must not clear them", resp.Msg.Note.Tags)
		}
	})

	t.Run("set tags replace the whole set", func(t *testing.T) {
		note := newNote(t)
		resp, err := srv.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{
			Id:   note.Id,
			Tags: &pb.NoteTagSet{Tags: []string{"Replacement"}},
		}))
		if err != nil {
			t.Fatalf("UpdateNote: %v", err)
		}
		if resp.Msg.Note.Body != "old body" {
			t.Errorf("body = %q, want unchanged", resp.Msg.Note.Body)
		}
		if !reflect.DeepEqual(resp.Msg.Note.Tags, []string{"replacement"}) {
			t.Errorf("tags = %v, want [replacement] — replace, not merge", resp.Msg.Note.Tags)
		}
	})

	t.Run("empty set tags clear every tag", func(t *testing.T) {
		note := newNote(t)
		resp, err := srv.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{
			Id: note.Id, Tags: &pb.NoteTagSet{},
		}))
		if err != nil {
			t.Fatalf("UpdateNote: %v", err)
		}
		if len(resp.Msg.Note.Tags) != 0 {
			t.Errorf("tags = %v, want cleared", resp.Msg.Note.Tags)
		}
	})

	t.Run("both fields", func(t *testing.T) {
		note := newNote(t)
		resp, err := srv.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{
			Id: note.Id, Body: strp("both"), Tags: &pb.NoteTagSet{Tags: []string{"x"}},
		}))
		if err != nil {
			t.Fatalf("UpdateNote: %v", err)
		}
		if resp.Msg.Note.Body != "both" || !reflect.DeepEqual(resp.Msg.Note.Tags, []string{"x"}) {
			t.Errorf("note = %+v, want body=both tags=[x]", resp.Msg.Note)
		}
	})

	t.Run("missing note is not found", func(t *testing.T) {
		_, err := srv.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{
			Id: "nope", Body: strp("x"),
		}))
		assertConnectCode(t, err, connect.CodeNotFound)
	})

	t.Run("validation is invalid argument", func(t *testing.T) {
		note := newNote(t)
		cases := []struct {
			name string
			req  *pb.UpdateNoteRequest
		}{
			{name: "blank id", req: &pb.UpdateNoteRequest{Id: " ", Body: strp("x")}},
			{name: "empty body", req: &pb.UpdateNoteRequest{Id: note.Id, Body: strp("  ")}},
			{
				name: "oversize body",
				req: &pb.UpdateNoteRequest{
					Id: note.Id, Body: strp(strings.Repeat("x", models.NoteMaxBodyBytes+1)),
				},
			},
			{
				name: "oversize tag",
				req: &pb.UpdateNoteRequest{
					Id:   note.Id,
					Tags: &pb.NoteTagSet{Tags: []string{strings.Repeat("t", models.NoteMaxTagLength+1)}},
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := srv.UpdateNote(ctx, connect.NewRequest(tc.req))
				assertConnectCode(t, err, connect.CodeInvalidArgument)
			})
		}
	})
}

func TestDeleteNote(t *testing.T) {
	srv := newNoteServer(t)
	ctx := context.Background()

	note := mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{
		RepoId: "repo-1", Body: "body", Tags: []string{"a"},
	})

	if _, err := srv.DeleteNote(ctx, connect.NewRequest(&pb.DeleteNoteRequest{Id: note.Id})); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if _, err := srv.GetNote(ctx, connect.NewRequest(&pb.GetNoteRequest{Id: note.Id})); err == nil {
		t.Fatal("expected the note to be gone")
	} else {
		assertConnectCode(t, err, connect.CodeNotFound)
	}

	t.Run("delete is idempotent", func(t *testing.T) {
		if _, err := srv.DeleteNote(ctx, connect.NewRequest(&pb.DeleteNoteRequest{Id: note.Id})); err != nil {
			t.Fatalf("second DeleteNote: %v", err)
		}
		if _, err := srv.DeleteNote(ctx, connect.NewRequest(&pb.DeleteNoteRequest{Id: "never"})); err != nil {
			t.Fatalf("DeleteNote absent: %v", err)
		}
	})

	t.Run("blank id is invalid argument", func(t *testing.T) {
		_, err := srv.DeleteNote(ctx, connect.NewRequest(&pb.DeleteNoteRequest{Id: " "}))
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})
}

// TestNoteRPCs_UnconfiguredStoreIsUnavailable pins that a daemon wired without
// a note store reports CodeUnavailable from every note RPC rather than panicking
// on a nil interface.
func TestNoteRPCs_UnconfiguredStoreIsUnavailable(t *testing.T) {
	srv := &Server{logger: zerolog.Nop()}
	ctx := context.Background()

	calls := map[string]func() error{
		"create": func() error {
			_, err := srv.CreateNote(ctx, connect.NewRequest(&pb.CreateNoteRequest{RepoId: "r", Body: "b"}))
			return err
		},
		"get": func() error {
			_, err := srv.GetNote(ctx, connect.NewRequest(&pb.GetNoteRequest{Id: "x"}))
			return err
		},
		"list": func() error {
			_, err := srv.ListNotes(ctx, connect.NewRequest(&pb.ListNotesRequest{}))
			return err
		},
		"update": func() error {
			_, err := srv.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{Id: "x", Body: strp("b")}))
			return err
		},
		"delete": func() error {
			_, err := srv.DeleteNote(ctx, connect.NewRequest(&pb.DeleteNoteRequest{Id: "x"}))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			assertConnectCode(t, call(), connect.CodeUnavailable)
		})
	}
}
