package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/notesync"
)

// countingNudger records how many times the note write path nudged it.
type countingNudger struct{ n atomic.Int32 }

func (c *countingNudger) Nudge() { c.n.Add(1) }

// newNoteSyncServer builds a note server wired to a real outbox and the given
// sync worker (nil for a local-only daemon).
func newNoteSyncServer(t *testing.T, worker NoteSyncNudger) *Server {
	t.Helper()
	database := setupServerTestDB(t)
	return &Server{
		notes:          db.NewNoteStore(database),
		noteSyncStates: db.NewNoteSyncStore(database),
		noteSyncWorker: worker,
		logger:         zerolog.Nop(),
	}
}

func TestNoteWrites_NudgeSyncOnlyOnSuccess(t *testing.T) {
	nudger := &countingNudger{}
	srv := newNoteSyncServer(t, nudger)
	ctx := context.Background()

	note := mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{RepoId: "repo-1", Body: "body"})
	body := "edited"
	if _, err := srv.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{Id: note.GetId(), Body: &body})); err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}
	if _, err := srv.DeleteNote(ctx, connect.NewRequest(&pb.DeleteNoteRequest{Id: note.GetId()})); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if got := nudger.n.Load(); got != 3 {
		t.Fatalf("nudges after create/update/delete = %d, want 3", got)
	}

	// Failed writes change nothing to sync, so they nudge nothing.
	_, err := srv.CreateNote(ctx, connect.NewRequest(&pb.CreateNoteRequest{RepoId: "repo-1", Body: "   "}))
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	_, err = srv.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{Id: "missing", Body: &body}))
	assertConnectCode(t, err, connect.CodeNotFound)
	if got := nudger.n.Load(); got != 3 {
		t.Errorf("nudges after failed writes = %d, want still 3", got)
	}
}

func TestSyncNotesNow_ReportsCountsAndNudges(t *testing.T) {
	nudger := &countingNudger{}
	srv := newNoteSyncServer(t, nudger)
	ctx := context.Background()
	mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{RepoId: "repo-1", Body: "a"})
	mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{RepoId: "repo-1", Body: "b"})
	before := nudger.n.Load()

	resp, err := srv.SyncNotesNow(ctx, connect.NewRequest(&pb.SyncNotesNowRequest{}))
	if err != nil {
		t.Fatalf("SyncNotesNow: %v", err)
	}
	if nudger.n.Load() != before+1 {
		t.Errorf("SyncNotesNow did not nudge the worker")
	}
	if !resp.Msg.GetIsWorkerConfigured() {
		t.Error("is_worker_configured = false with a worker wired")
	}
	got := resp.Msg.GetStateCounts()
	if len(got) != len(noteSyncStateOrder) {
		t.Fatalf("state counts = %v, want every state", got)
	}
	for i, c := range got {
		if c.GetState() != string(noteSyncStateOrder[i]) {
			t.Errorf("state %d = %q, want %q (fixed order)", i, c.GetState(), noteSyncStateOrder[i])
		}
		want := int64(0)
		if c.GetState() == "pending" {
			want = 2
		}
		if c.GetNoteCount() != want {
			t.Errorf("%s count = %d, want %d", c.GetState(), c.GetNoteCount(), want)
		}
	}
}

func TestSyncNotesNow_LocalOnlyDaemonStillAnswers(t *testing.T) {
	srv := newNoteSyncServer(t, nil)
	mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{RepoId: "repo-1", Body: "a"})

	resp, err := srv.SyncNotesNow(context.Background(), connect.NewRequest(&pb.SyncNotesNowRequest{}))
	if err != nil {
		t.Fatalf("SyncNotesNow on a local-only daemon: %v", err)
	}
	if resp.Msg.GetIsWorkerConfigured() {
		t.Error("is_worker_configured = true with no worker")
	}
	if c := resp.Msg.GetStateCounts()[0]; c.GetState() != "pending" || c.GetNoteCount() != 1 {
		t.Errorf("first count = %v, want pending 1", c)
	}
}

func TestSyncNotesNow_NoOutboxIsUnavailable(t *testing.T) {
	srv := &Server{logger: zerolog.Nop()}
	_, err := srv.SyncNotesNow(context.Background(), connect.NewRequest(&pb.SyncNotesNowRequest{}))
	assertConnectCode(t, err, connect.CodeUnavailable)
}

// hangingSyncClient blocks every SyncDaemonNotes call until its context ends.
type hangingSyncClient struct {
	once    sync.Once
	started chan struct{}
	ended   atomic.Bool
}

func (h *hangingSyncClient) SyncDaemonNotes(ctx context.Context, _ *connect.Request[pb.SyncDaemonNotesRequest]) (*connect.Response[pb.SyncDaemonNotesResponse], error) {
	h.once.Do(func() { close(h.started) })
	<-ctx.Done()
	h.ended.Store(true)
	return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
}

type staticTokens struct{}

func (staticTokens) Get() string                     { return "tok" }
func (staticTokens) Set(string)                      {}
func (staticTokens) CompareAndSwap(_, _ string) bool { return true }

// TestCreateNote_NeverBlockedByHangingSync drives CreateNote through the RPC
// handler while the real sync worker is stuck in a SyncDaemonNotes call that
// never answers: every create succeeds and returns while the call is still
// hanging, and the worker's per-call timeout is what ends it.
func TestCreateNote_NeverBlockedByHangingSync(t *testing.T) {
	database := setupServerTestDB(t)
	repos := db.NewRepoStore(database)
	repo, err := repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName: "widgets", LocalPath: "/tmp/widgets", OriginURL: "https://github.com/acme/widgets",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	client := &hangingSyncClient{started: make(chan struct{})}
	worker := notesync.New(notesync.Config{
		Store:       db.NewNoteSyncStore(database),
		Repos:       notesync.RepoStoreOrigins{Repos: repos},
		Client:      client,
		Tokens:      staticTokens{},
		Logger:      zerolog.Nop(),
		CallTimeout: 3 * time.Second,
		Interval:    time.Hour,
	})
	srv := &Server{
		notes:          db.NewNoteStore(database),
		noteSyncStates: db.NewNoteSyncStore(database),
		noteSyncWorker: worker,
		logger:         zerolog.Nop(),
	}
	mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{RepoId: repo.ID, Body: "first"})

	ctx, cancel := context.WithCancel(context.Background())
	done := safego.Go(zerolog.Nop(), func() { worker.Run(ctx) })
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never called Bosso")
	}

	for range 10 {
		mustCreateNoteRPC(t, srv, &pb.CreateNoteRequest{RepoId: repo.ID, Body: "written while sync hangs"})
	}
	if client.ended.Load() {
		t.Fatal("the hanging call ended before the creates returned; note creation waited on sync")
	}

	deadline := time.Now().Add(10 * time.Second)
	for !client.ended.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the worker's per-call timeout never fired")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
