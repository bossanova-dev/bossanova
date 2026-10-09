package notesync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// fakeBosso is an in-memory SyncDaemonNotes server with Bosso's per-item
// semantics that matter to the worker: daemon auth, a highest-version-wins
// cloud row per source note, STALE for replays, and tombstones on delete.
type fakeBosso struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	token string
	mu    sync.Mutex
	rows  map[string]*cloudRow
	calls int
}

type cloudRow struct {
	version int64
	body    string
	origin  string
	deleted bool
}

func (f *fakeBosso) SyncDaemonNotes(_ context.Context, req *connect.Request[pb.SyncDaemonNotesRequest]) (*connect.Response[pb.SyncDaemonNotesResponse], error) {
	if req.Header().Get("Authorization") != "Bearer "+f.token {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("daemon authentication required"))
	}
	if len(req.Msg.GetItems()) > 50 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a batch holds at most 50 items"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	resp := &pb.SyncDaemonNotesResponse{}
	for _, it := range req.Msg.GetItems() {
		res := &pb.NoteSyncResult{
			SourceNoteId: it.GetSourceNoteId(), SourceVersion: it.GetSourceVersion(), OrganizationId: "org-personal",
		}
		row := f.rows[it.GetSourceNoteId()]
		switch {
		case !it.GetSourceCreatedAt().IsValid():
			res.Outcome = pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_REJECTED
			res.Reason = "source_created_at is required"
		case row != nil && it.GetSourceVersion() <= row.version:
			res.Outcome = pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_STALE
		default:
			f.rows[it.GetSourceNoteId()] = &cloudRow{
				version: it.GetSourceVersion(), body: it.GetBody(), origin: it.GetRepoOriginUrl(), deleted: it.GetIsDeleted(),
			}
			res.Outcome = pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_SYNCED
		}
		resp.Results = append(resp.Results, res)
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeBosso) row(id string) *cloudRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.rows[id]; r != nil {
		cp := *r
		return &cp
	}
	return nil
}

// TestContract_CreateEditDeleteThroughGeneratedClient drives the worker over
// the real generated Connect client and handler, against a real SQLite outbox:
// a create, an edit and a delete each reach the cloud and settle synced.
func TestContract_CreateEditDeleteThroughGeneratedClient(t *testing.T) {
	bosso := &fakeBosso{token: "tok-1", rows: map[string]*cloudRow{}}
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(bosso)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	e := newEnv(t)
	e.cfg.Client = bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL)
	w := e.worker()
	ctx := context.Background()

	note := e.mustNote(e.repoID, "first draft", "gotcha")
	w.Drain(ctx)
	if r := bosso.row(note.ID); r == nil || r.version != 1 || r.body != "first draft" ||
		r.origin != "https://github.com/acme/widgets" || r.deleted {
		t.Fatalf("cloud row after create = %+v, want v1 with the body and canonical origin", r)
	}
	if s := e.state(note.ID); s.State != models.NoteSyncSynced || s.SyncedVersion != 1 {
		t.Fatalf("local sync after create = %+v, want synced v1", s)
	}

	body := "second draft"
	if _, err := e.notes.Update(ctx, updateBody(note.ID, body)); err != nil {
		t.Fatalf("update: %v", err)
	}
	w.Drain(ctx)
	if r := bosso.row(note.ID); r == nil || r.version != 2 || r.body != body {
		t.Fatalf("cloud row after edit = %+v, want v2 with the edited body", r)
	}
	if s := e.state(note.ID); s.State != models.NoteSyncSynced || s.SyncedVersion != 2 {
		t.Fatalf("local sync after edit = %+v, want synced v2", s)
	}

	if err := e.notes.Delete(ctx, note.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	res := w.Drain(ctx)
	if r := bosso.row(note.ID); r == nil || r.version != 3 || !r.deleted {
		t.Fatalf("cloud row after delete = %+v, want a v3 tombstone", r)
	}
	if res.States[models.NoteSyncSynced] != 1 || res.Purged != 1 {
		t.Errorf("delete drain = %+v, want the tombstone synced then purged", res)
	}
	counts, err := e.sync.CountByState(ctx)
	if err != nil || len(counts) != 0 {
		t.Errorf("outbox after the settled delete = %v, %v; want empty", counts, err)
	}
}

// TestContract_ReplayIsIdempotent resends an already-applied version (as a
// retry after a lost response would) and proves the cloud answers STALE and
// the outbox settles synced without a second cloud write.
func TestContract_ReplayIsIdempotent(t *testing.T) {
	bosso := &fakeBosso{token: "tok-1", rows: map[string]*cloudRow{}}
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(bosso)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	e := newEnv(t)
	note := e.mustNote(e.repoID, "body")
	// The cloud already holds v1 (an earlier send whose response was lost).
	bosso.rows[note.ID] = &cloudRow{version: 1, body: "body"}
	e.cfg.Client = bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL)

	e.worker().Drain(context.Background())

	if s := e.state(note.ID); s.State != models.NoteSyncSynced || s.SyncedVersion != 1 {
		t.Errorf("local sync after replay = %+v, want synced v1 (STALE settles)", s)
	}
}

func updateBody(id, body string) db.UpdateNoteParams {
	return db.UpdateNoteParams{ID: id, Body: &body}
}
