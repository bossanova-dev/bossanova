package notesync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
)

// fakeClient is a programmable SyncDaemonNotes client. Each call records the
// request and its bearer token, then answers through respond (default: every
// item SYNCED).
type fakeClient struct {
	mu      sync.Mutex
	calls   []*pb.SyncDaemonNotesRequest
	tokens  []string
	respond func(ctx context.Context, call int, req *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error)
}

func (f *fakeClient) SyncDaemonNotes(ctx context.Context, req *connect.Request[pb.SyncDaemonNotesRequest]) (*connect.Response[pb.SyncDaemonNotesResponse], error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.Msg)
	f.tokens = append(f.tokens, req.Header().Get("Authorization"))
	n := len(f.calls)
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		respond = func(_ context.Context, _ int, r *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
			return allSynced(r), nil
		}
	}
	resp, err := respond(ctx, n, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeClient) call(i int) (*pb.SyncDaemonNotesRequest, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i], f.tokens[i]
}

// allSynced answers every item of r SYNCED into org-1.
func allSynced(r *pb.SyncDaemonNotesRequest) *pb.SyncDaemonNotesResponse {
	resp := &pb.SyncDaemonNotesResponse{}
	for _, it := range r.GetItems() {
		resp.Results = append(resp.Results, &pb.NoteSyncResult{
			SourceNoteId:   it.GetSourceNoteId(),
			SourceVersion:  it.GetSourceVersion(),
			Outcome:        pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_SYNCED,
			OrganizationId: "org-1",
		})
	}
	return resp
}

// tokenHolder is a minimal TokenHolder.
type tokenHolder struct {
	mu  sync.Mutex
	tok string
}

func (h *tokenHolder) Get() string { h.mu.Lock(); defer h.mu.Unlock(); return h.tok }
func (h *tokenHolder) Set(t string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tok = t
}

func (h *tokenHolder) CompareAndSwap(old, t string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tok != old {
		return false
	}
	h.tok = t
	return true
}

// clock is a settable test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// env is a real SQLite outbox plus a worker wired to a fake client.
type env struct {
	t      *testing.T
	notes  *db.SQLiteNoteStore
	sync   *db.SQLiteNoteSyncStore
	repos  *db.SQLiteRepoStore
	repoID string
	client *fakeClient
	tokens *tokenHolder
	clock  *clock
	logs   *bytes.Buffer
	cfg    Config
}

func newEnv(t *testing.T) *env {
	t.Helper()
	database := dbtest.New(t)
	e := &env{
		t:      t,
		notes:  db.NewNoteStore(database, db.WithNoteRetention(db.NoteRetention{})),
		sync:   db.NewNoteSyncStore(database),
		repos:  db.NewRepoStore(database),
		client: &fakeClient{},
		tokens: &tokenHolder{tok: "tok-1"},
		clock:  &clock{now: time.Now().UTC().Truncate(time.Millisecond)},
		logs:   &bytes.Buffer{},
	}
	e.repoID = e.mustRepo("widgets", "git@github.com:acme/widgets.git")
	e.cfg = Config{
		Store:  e.sync,
		Repos:  RepoStoreOrigins{Repos: e.repos},
		Client: e.client,
		Tokens: e.tokens,
		Logger: zerolog.New(&syncWriter{w: e.logs}),
		Now:    e.clock.Now,
		// Deterministic: no jitter unless a test overrides it.
		Jitter: func(time.Duration) time.Duration { return 0 },
	}
	return e
}

// syncWriter serialises log writes from the worker goroutine and the test.
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (e *env) worker() *Worker { return New(e.cfg) }

func (e *env) mustRepo(name, origin string) string {
	e.t.Helper()
	r, err := e.repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName: name, LocalPath: "/tmp/" + name, OriginURL: origin,
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt-" + name,
	})
	if err != nil {
		e.t.Fatalf("create repo: %v", err)
	}
	return r.ID
}

func (e *env) mustNote(repoID, body string, tags ...string) *models.Note {
	e.t.Helper()
	n, err := e.notes.Create(context.Background(), db.CreateNoteParams{RepoID: repoID, Body: body, Tags: tags})
	if err != nil {
		e.t.Fatalf("create note: %v", err)
	}
	return n
}

func (e *env) state(noteID string) *models.NoteSyncState {
	e.t.Helper()
	n, err := e.notes.Get(context.Background(), noteID)
	if err != nil {
		e.t.Fatalf("get note %s: %v", noteID, err)
	}
	if n.Sync == nil {
		e.t.Fatalf("note %s has no sync state", noteID)
	}
	return n.Sync
}

func TestOutcomeMapping(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	retryAt := now.Add(20 * time.Minute)
	const jitter = 7 * time.Second
	w := New(Config{Now: func() time.Time { return now }, Jitter: func(limit time.Duration) time.Duration {
		if limit < jitter {
			return 0
		}
		return jitter
	}})
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	tests := []struct {
		name      string
		result    *pb.NoteSyncResult
		wantState models.NoteSyncStatus
		wantNext  *time.Time
	}{
		{"synced", &pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_SYNCED}, models.NoteSyncSynced, nil},
		{"stale", &pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_STALE}, models.NoteSyncSynced, nil},
		{"suppressed", &pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_SUPPRESSED}, models.NoteSyncSuppressed, nil},
		{"expired", &pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_EXPIRED}, models.NoteSyncExpired, nil},
		{"rejected", &pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_REJECTED, Reason: "body too big"}, models.NoteSyncRejected, nil},
		{
			"rate limited at retry_at plus jitter",
			&pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_RATE_LIMITED, RetryAt: timestamppb.New(retryAt)},
			models.NoteSyncRateLimited, at(20*time.Minute + jitter),
		},
		{
			"rate limited with a past retry_at backs off from now",
			&pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_RATE_LIMITED, RetryAt: timestamppb.New(now.Add(-time.Minute))},
			models.NoteSyncRateLimited, at(30*time.Second + jitter),
		},
		{
			"rate limited without retry_at backs off from now",
			&pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_RATE_LIMITED},
			models.NoteSyncRateLimited, at(30*time.Second + jitter),
		},
		{"not entitled re-checks in an hour", &pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_NOT_ENTITLED}, models.NoteSyncNotEntitled, at(time.Hour)},
		{"organization refused re-checks in an hour", &pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_ORGANIZATION_REFUSED}, models.NoteSyncRefused, at(time.Hour)},
		// Attempt 1: 30 s with equal jitter; the fixed jitter (7 s, under the
		// 15 s half) lands it at 15 s + 7 s.
		{"unspecified backs off as a failure", &pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_UNSPECIFIED}, models.NoteSyncFailed, at(15*time.Second + jitter)},
	}
	covered := map[pb.NoteSyncOutcome]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			covered[tt.result.GetOutcome()] = true
			state, details := w.outcome(tt.result, now, 1)
			if state != tt.wantState {
				t.Errorf("state = %s, want %s", state, tt.wantState)
			}
			switch {
			case tt.wantNext == nil && details.NextAttemptAt != nil:
				t.Errorf("next attempt = %v, want none", details.NextAttemptAt)
			case tt.wantNext != nil && (details.NextAttemptAt == nil || !details.NextAttemptAt.Equal(*tt.wantNext)):
				t.Errorf("next attempt = %v, want %v", details.NextAttemptAt, tt.wantNext)
			}
		})
	}
	// Every enum value is mapped deliberately.
	for v := range pb.NoteSyncOutcome_name {
		if !covered[pb.NoteSyncOutcome(v)] {
			t.Errorf("outcome %s has no mapping case", pb.NoteSyncOutcome(v))
		}
	}

	// The organization id is recorded whenever Bosso names one.
	_, details := w.outcome(&pb.NoteSyncResult{Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_SYNCED, OrganizationId: "org-9"}, now, 1)
	if details.OrganizationID == nil || *details.OrganizationID != "org-9" {
		t.Errorf("organization id = %v, want org-9", details.OrganizationID)
	}
}

// TestRateLimitJitterIsBoundedTo30s proves the jitter source is asked for at
// most 30 s past retry_at.
func TestRateLimitJitterIsBoundedTo30s(t *testing.T) {
	now := time.Now()
	var asked time.Duration
	w := New(Config{Now: func() time.Time { return now }, Jitter: func(limit time.Duration) time.Duration {
		asked = limit
		return limit - 1
	}})
	retryAt := now.Add(time.Minute)
	_, d := w.outcome(&pb.NoteSyncResult{
		Outcome: pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_RATE_LIMITED, RetryAt: timestamppb.New(retryAt),
	}, now, 1)
	if asked != 30*time.Second {
		t.Errorf("jitter limit = %v, want 30s", asked)
	}
	if d.NextAttemptAt.Before(retryAt) || d.NextAttemptAt.Sub(retryAt) >= 30*time.Second {
		t.Errorf("next attempt %v not within [retry_at, retry_at+30s)", d.NextAttemptAt)
	}
}

func TestBackoffSchedule(t *testing.T) {
	now := time.Now()
	full := New(Config{Now: func() time.Time { return now }, Jitter: func(limit time.Duration) time.Duration { return limit - 1 }})
	none := New(Config{Now: func() time.Time { return now }, Jitter: func(time.Duration) time.Duration { return 0 }})
	want := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute,
	}
	for i, d := range want {
		attempt := i + 1
		if got := full.backoff(now, attempt).Sub(now); got != d {
			t.Errorf("attempt %d max backoff = %v, want %v", attempt, got, d)
		}
		if got := none.backoff(now, attempt).Sub(now); got != d/2 {
			t.Errorf("attempt %d min backoff = %v, want %v", attempt, got, d/2)
		}
	}
	if got := full.backoff(now, 1000).Sub(now); got != 30*time.Minute {
		t.Errorf("huge attempt backoff = %v, want the 30m cap", got)
	}
}

func TestDrain_SyncsPendingNotes(t *testing.T) {
	e := newEnv(t)
	a := e.mustNote(e.repoID, "first body", "gotcha")
	b := e.mustNote(e.repoID, "second body")

	res := e.worker().Drain(context.Background())

	if res.Sent != 2 || res.States[models.NoteSyncSynced] != 2 {
		t.Fatalf("drain = %+v, want 2 sent and synced", res)
	}
	for _, n := range []*models.Note{a, b} {
		if s := e.state(n.ID); s.State != models.NoteSyncSynced || s.SyncedVersion != 1 ||
			s.OrganizationID == nil || *s.OrganizationID != "org-1" {
			t.Errorf("note %s sync = %+v, want synced v1 in org-1", n.ID, s)
		}
	}
	req, auth := e.client.call(0)
	if auth != "Bearer tok-1" {
		t.Errorf("Authorization = %q, want the daemon session token", auth)
	}
	byID := map[string]*pb.NoteSyncItem{}
	for _, it := range req.GetItems() {
		byID[it.GetSourceNoteId()] = it
	}
	it := byID[a.ID]
	if it == nil || it.GetSourceVersion() != 1 || it.GetRepoOriginUrl() != "https://github.com/acme/widgets" ||
		it.GetBody() != "first body" || len(it.GetTags()) != 1 || it.GetTags()[0] != "gotcha" ||
		!it.GetSourceCreatedAt().AsTime().Equal(a.CreatedAt) || it.GetIsDeleted() {
		t.Errorf("item = %+v, want v1 with the normalised origin, body, tags and creation time", it)
	}

	// Nothing is due any more: a second drain sends nothing.
	if res := e.worker().Drain(context.Background()); res.Sent != 0 || e.client.callCount() != 1 {
		t.Errorf("second drain = %+v after %d calls, want nothing sent", res, e.client.callCount())
	}
}

func TestDrain_NoOriginIsRejectedLocally(t *testing.T) {
	e := newEnv(t)
	local := e.mustRepo("local", "")
	n := e.mustNote(local, "local only")

	res := e.worker().Drain(context.Background())

	if e.client.callCount() != 0 || res.Sent != 0 {
		t.Fatalf("a note with no origin was sent (%d calls)", e.client.callCount())
	}
	s := e.state(n.ID)
	if s.State != models.NoteSyncRejected || s.LastError == nil || *s.LastError != reasonNoOrigin {
		t.Errorf("sync = %+v, want rejected %q", s, reasonNoOrigin)
	}
}

func TestDrain_DeleteSendsTombstone(t *testing.T) {
	e := newEnv(t)
	n := e.mustNote(e.repoID, "doomed")
	if err := e.notes.Delete(context.Background(), n.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	res := e.worker().Drain(context.Background())

	if res.States[models.NoteSyncSynced] != 1 {
		t.Fatalf("drain = %+v, want the tombstone synced", res)
	}
	req, _ := e.client.call(0)
	it := req.GetItems()[0]
	if !it.GetIsDeleted() || it.GetSourceVersion() != 2 || it.GetRepoOriginUrl() != "https://github.com/acme/widgets" ||
		!it.GetSourceCreatedAt().AsTime().Equal(n.CreatedAt) || it.GetBody() != "" {
		t.Errorf("tombstone item = %+v, want deleted v2 routed by origin and dated by creation", it)
	}
}

func TestDrain_TransportFailureBacksOffThenResendsSameVersion(t *testing.T) {
	e := newEnv(t)
	n := e.mustNote(e.repoID, "body")
	e.client.respond = func(_ context.Context, call int, r *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
		if call == 1 {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("bosso down"))
		}
		return allSynced(r), nil
	}
	w := e.worker()

	w.Drain(context.Background())
	s := e.state(n.ID)
	if s.State != models.NoteSyncFailed || s.AttemptCount != 1 || s.NextAttemptAt == nil ||
		!s.NextAttemptAt.Equal(e.clock.Now().Add(15*time.Second)) {
		t.Fatalf("after failure sync = %+v, want failed, attempt 1, next at now+15s (30s equal-jittered)", s)
	}

	// Not yet due: nothing is resent.
	w.Drain(context.Background())
	if e.client.callCount() != 1 {
		t.Fatalf("resent before the backoff elapsed (%d calls)", e.client.callCount())
	}

	e.clock.advance(31 * time.Second)
	w.Drain(context.Background())
	if e.client.callCount() != 2 {
		t.Fatalf("calls = %d, want the retry", e.client.callCount())
	}
	first, _ := e.client.call(0)
	second, _ := e.client.call(1)
	if first.GetItems()[0].GetSourceVersion() != second.GetItems()[0].GetSourceVersion() {
		t.Errorf("retry sent v%d, want the same v%d", second.GetItems()[0].GetSourceVersion(), first.GetItems()[0].GetSourceVersion())
	}
	if s := e.state(n.ID); s.State != models.NoteSyncSynced || s.SyncedVersion != 1 || s.AttemptCount != 2 {
		t.Errorf("after retry sync = %+v, want synced v1 after 2 attempts", s)
	}
}

func TestDrain_InFlightEditStaysPending(t *testing.T) {
	e := newEnv(t)
	n := e.mustNote(e.repoID, "v1 body")
	e.client.respond = func(ctx context.Context, _ int, r *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
		// The user edits the note while version 1 is on the wire.
		body := "v2 body"
		if _, err := e.notes.Update(ctx, db.UpdateNoteParams{ID: n.ID, Body: &body}); err != nil {
			return nil, err
		}
		return allSynced(r), nil
	}

	res := e.worker().Drain(context.Background())

	if res.Superseded != 1 {
		t.Errorf("drain = %+v, want the v1 outcome superseded", res)
	}
	s := e.state(n.ID)
	if s.State != models.NoteSyncPending || s.SourceVersion != 2 || s.SyncedVersion != 0 {
		t.Errorf("sync = %+v, want pending v2 never synced", s)
	}

	// The next drain sends version 2 with the new body.
	e.client.respond = nil
	e.worker().Drain(context.Background())
	req, _ := e.client.call(1)
	if it := req.GetItems()[0]; it.GetSourceVersion() != 2 || it.GetBody() != "v2 body" {
		t.Errorf("follow-up item = %+v, want v2 with the edited body", it)
	}
	if s := e.state(n.ID); s.State != models.NoteSyncSynced || s.SyncedVersion != 2 {
		t.Errorf("sync = %+v, want synced v2", s)
	}
}

func TestDrain_PerItemOutcomesRecorded(t *testing.T) {
	e := newEnv(t)
	limited := e.mustNote(e.repoID, "a")
	refused := e.mustNote(e.repoID, "b")
	retryAt := e.clock.Now().Add(10 * time.Minute)
	e.client.respond = func(_ context.Context, _ int, r *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
		resp := &pb.SyncDaemonNotesResponse{}
		for _, it := range r.GetItems() {
			res := &pb.NoteSyncResult{SourceNoteId: it.GetSourceNoteId(), SourceVersion: it.GetSourceVersion()}
			if it.GetSourceNoteId() == limited.ID {
				res.Outcome = pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_RATE_LIMITED
				res.RetryAt = timestamppb.New(retryAt)
			} else {
				res.Outcome = pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_ORGANIZATION_REFUSED
			}
			resp.Results = append(resp.Results, res)
		}
		return resp, nil
	}

	e.worker().Drain(context.Background())

	if s := e.state(limited.ID); s.State != models.NoteSyncRateLimited || s.NextAttemptAt == nil || !s.NextAttemptAt.Equal(retryAt) {
		t.Errorf("rate limited sync = %+v, want rate_limited at %v", s, retryAt)
	}
	if s := e.state(refused.ID); s.State != models.NoteSyncRefused || s.NextAttemptAt == nil ||
		!s.NextAttemptAt.Equal(e.clock.Now().Add(time.Hour)) {
		t.Errorf("refused sync = %+v, want refused re-checked in an hour", s)
	}
}

func TestDrain_PermissionDeniedMarksEveryItemNotEntitled(t *testing.T) {
	e := newEnv(t)
	a := e.mustNote(e.repoID, "a")
	b := e.mustNote(e.repoID, "b")
	e.client.respond = func(context.Context, int, *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("cloud access revoked"))
	}

	e.worker().Drain(context.Background())

	for _, n := range []*models.Note{a, b} {
		if s := e.state(n.ID); s.State != models.NoteSyncNotEntitled || s.NextAttemptAt == nil ||
			!s.NextAttemptAt.Equal(e.clock.Now().Add(time.Hour)) {
			t.Errorf("note %s sync = %+v, want not_entitled re-checked in an hour", n.ID, s)
		}
	}
}

func TestDrain_UnauthenticatedReRegistersOnceAndRetries(t *testing.T) {
	t.Run("retry succeeds", func(t *testing.T) {
		e := newEnv(t)
		n := e.mustNote(e.repoID, "body")
		var reRegisters atomic.Int32
		e.cfg.ReRegister = func(context.Context) (string, error) {
			reRegisters.Add(1)
			return "tok-2", nil
		}
		e.client.respond = func(_ context.Context, call int, r *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
			if call == 1 {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
			}
			return allSynced(r), nil
		}

		e.worker().Drain(context.Background())

		if reRegisters.Load() != 1 || e.client.callCount() != 2 {
			t.Fatalf("re-registers = %d, calls = %d; want exactly 1 and 2", reRegisters.Load(), e.client.callCount())
		}
		if _, auth := e.client.call(1); auth != "Bearer tok-2" {
			t.Errorf("retry Authorization = %q, want the re-registered token", auth)
		}
		if e.tokens.Get() != "tok-2" {
			t.Errorf("shared token = %q, want rotated to tok-2", e.tokens.Get())
		}
		if s := e.state(n.ID); s.State != models.NoteSyncSynced {
			t.Errorf("sync = %+v, want synced after the retry", s)
		}
	})

	t.Run("still rejected backs off", func(t *testing.T) {
		e := newEnv(t)
		n := e.mustNote(e.repoID, "body")
		var reRegisters atomic.Int32
		e.cfg.ReRegister = func(context.Context) (string, error) {
			reRegisters.Add(1)
			return "tok-2", nil
		}
		e.client.respond = func(context.Context, int, *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
		}

		e.worker().Drain(context.Background())

		if reRegisters.Load() != 1 || e.client.callCount() != 2 {
			t.Fatalf("re-registers = %d, calls = %d; want exactly 1 and 2 (no loop)", reRegisters.Load(), e.client.callCount())
		}
		if s := e.state(n.ID); s.State != models.NoteSyncFailed || s.NextAttemptAt == nil {
			t.Errorf("sync = %+v, want failed with a backoff", s)
		}
	})
}

func TestDrain_SlowAuthRejectionDoesNotStarveRetry(t *testing.T) {
	e := newEnv(t)
	n := e.mustNote(e.repoID, "body")
	e.cfg.CallTimeout = 300 * time.Millisecond
	e.cfg.ReRegister = func(context.Context) (string, error) { return "tok-2", nil }
	// Each attempt uses most of one CallTimeout; a deadline shared across both
	// attempts would expire during the retry.
	e.client.respond = func(ctx context.Context, call int, r *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
		select {
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
		if call == 1 {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
		}
		return allSynced(r), nil
	}

	e.worker().Drain(context.Background())

	if e.client.callCount() != 2 {
		t.Fatalf("calls = %d, want the rejected attempt plus one retry", e.client.callCount())
	}
	if s := e.state(n.ID); s.State != models.NoteSyncSynced {
		t.Errorf("sync = %+v, want synced: the retry gets its own CallTimeout", s)
	}
}

func TestDrain_BatchBounds(t *testing.T) {
	e := newEnv(t)
	for i := range 120 {
		e.mustNote(e.repoID, fmt.Sprintf("note %d", i))
	}
	e.cfg.MaxBatches = 2

	res := e.worker().Drain(context.Background())

	if res.Batches != 2 || res.Sent != 100 || e.client.callCount() != 2 {
		t.Fatalf("drain = %+v over %d calls, want 2 batches of 50", res, e.client.callCount())
	}
	for i := range e.client.callCount() {
		if req, _ := e.client.call(i); len(req.GetItems()) > DefaultBatchSize {
			t.Errorf("call %d carried %d items, over the %d cap", i, len(req.GetItems()), DefaultBatchSize)
		}
	}
	counts, err := e.sync.CountByState(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts[models.NoteSyncPending] != 20 || counts[models.NoteSyncSynced] != 100 {
		t.Errorf("counts = %v, want 20 pending left for the next tick", counts)
	}

	// The next tick drains the rest in one short batch.
	if res := e.worker().Drain(context.Background()); res.Batches != 1 || res.Sent != 20 {
		t.Errorf("next drain = %+v, want one batch of 20", res)
	}
}

func TestDrain_RateLimitedBatchEndsTheTick(t *testing.T) {
	e := newEnv(t)
	for i := range 120 {
		e.mustNote(e.repoID, fmt.Sprintf("note %d", i))
	}
	retryAt := e.clock.Now().Add(10 * time.Minute)
	e.client.respond = func(_ context.Context, _ int, r *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
		resp := allSynced(r)
		// One item over quota is enough to close the window for the tick.
		resp.Results[0].Outcome = pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_RATE_LIMITED
		resp.Results[0].RetryAt = timestamppb.New(retryAt)
		return resp, nil
	}

	res := e.worker().Drain(context.Background())

	if res.Batches != 1 || e.client.callCount() != 1 {
		t.Fatalf("drain = %+v over %d calls, want the tick to stop after the rate-limited batch", res, e.client.callCount())
	}
	counts, err := e.sync.CountByState(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts[models.NoteSyncPending] != 70 || counts[models.NoteSyncRateLimited] != 1 {
		t.Errorf("counts = %v, want 70 pending left due for the next tick", counts)
	}
}

func TestDrain_WaitsForSessionToken(t *testing.T) {
	e := newEnv(t)
	n := e.mustNote(e.repoID, "body")
	e.tokens.Set("")

	e.worker().Drain(context.Background())

	if e.client.callCount() != 0 {
		t.Fatal("sent without a session token")
	}
	if s := e.state(n.ID); s.State != models.NoteSyncPending || s.AttemptCount != 0 {
		t.Errorf("sync = %+v, want untouched pending (no attempt burned)", s)
	}
}

// purgeRecorder wraps a Store to observe the purge horizon.
type purgeRecorder struct {
	Store
	olderThan []time.Time
}

func (p *purgeRecorder) PurgeSettledTombstones(ctx context.Context, olderThan time.Time) (int64, error) {
	p.olderThan = append(p.olderThan, olderThan)
	return p.Store.PurgeSettledTombstones(ctx, olderThan)
}

func TestDrain_PurgesTombstonesWithNinetyDayHorizon(t *testing.T) {
	e := newEnv(t)
	n := e.mustNote(e.repoID, "doomed")
	if err := e.notes.Delete(context.Background(), n.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rec := &purgeRecorder{Store: e.sync}
	e.cfg.Store = rec

	res := e.worker().Drain(context.Background())

	if len(rec.olderThan) != 1 || !rec.olderThan[0].Equal(e.clock.Now().Add(-90*24*time.Hour)) {
		t.Errorf("purge horizons = %v, want one at now-90d", rec.olderThan)
	}
	if res.Purged != 1 {
		t.Errorf("purged = %d, want the synced tombstone removed after the tick", res.Purged)
	}
}

func TestDrain_LogsCountsNeverContent(t *testing.T) {
	e := newEnv(t)
	e.mustNote(e.repoID, "SECRET-BODY-TEXT", "secret-tag")
	e.client.respond = func(context.Context, int, *pb.SyncDaemonNotesRequest) (*pb.SyncDaemonNotesResponse, error) {
		return nil, connect.NewError(connect.CodeInternal, errors.New("boom"))
	}
	e.worker().Drain(context.Background())
	e.client.respond = nil
	e.clock.advance(time.Hour)
	e.worker().Drain(context.Background())

	logs := e.logs.String()
	if !strings.Contains(logs, `"synced":1`) {
		t.Errorf("logs lack per-state counts: %s", logs)
	}
	for _, leak := range []string{"SECRET-BODY-TEXT", "secret-tag"} {
		if strings.Contains(logs, leak) {
			t.Errorf("logs leak %q: %s", leak, logs)
		}
	}
}

func TestNudgeNeverBlocksAndCoalesces(t *testing.T) {
	w := New(Config{})
	done := make(chan struct{})
	go func() {
		for range 1000 {
			w.Nudge()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Nudge blocked")
	}
	if len(w.nudge) != 1 {
		t.Errorf("queued nudges = %d, want 1 (coalesced)", len(w.nudge))
	}
	var nilWorker *Worker
	nilWorker.Nudge() // must not panic
}

// hangingClient blocks every call until its context ends, recording the
// context error it saw.
type hangingClient struct {
	started chan struct{}
	once    sync.Once
	errs    chan error
}

func (h *hangingClient) SyncDaemonNotes(ctx context.Context, _ *connect.Request[pb.SyncDaemonNotesRequest]) (*connect.Response[pb.SyncDaemonNotesResponse], error) {
	h.once.Do(func() { close(h.started) })
	<-ctx.Done()
	select {
	case h.errs <- ctx.Err():
	default:
	}
	return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
}

// TestHangingClientNeverBlocksNoteWrites proves a stuck Bosso cannot stall the
// note write path: while the worker's call hangs, creates and nudges complete
// immediately, and the call is cut off by the per-call timeout.
func TestHangingClientNeverBlocksNoteWrites(t *testing.T) {
	e := newEnv(t)
	hc := &hangingClient{started: make(chan struct{}), errs: make(chan error, 1)}
	e.cfg.Client = hc
	e.cfg.CallTimeout = 2 * time.Second
	e.cfg.Interval = time.Hour
	first := e.mustNote(e.repoID, "first")
	w := e.worker()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := safego.Go(zerolog.Nop(), func() { w.Run(ctx) })

	select {
	case <-hc.started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never called the client")
	}
	for i := range 20 {
		e.mustNote(e.repoID, fmt.Sprintf("while hanging %d", i))
		w.Nudge()
	}
	// Every create and nudge returned while the call was still outstanding:
	// the write path never waited on it.
	select {
	case err := <-hc.errs:
		t.Fatalf("the hanging call ended (%v) before the writes finished; they waited on sync", err)
	default:
	}

	select {
	case err := <-hc.errs:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("hanging call ended with %v, want the per-call deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("per-call timeout never fired")
	}
	// The worker records the timed-out batch as a retryable failure.
	deadline := time.Now().Add(5 * time.Second)
	for e.state(first.ID).State != models.NoteSyncFailed {
		if time.Now().After(deadline) {
			t.Fatalf("timed-out note sync = %+v, want failed (retried with backoff)", e.state(first.ID))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// TestRunReturnsOnShutdown proves the worker goroutine is joinable: cancelling
// its context ends Run even mid-call.
func TestRunReturnsOnShutdown(t *testing.T) {
	e := newEnv(t)
	hc := &hangingClient{started: make(chan struct{}), errs: make(chan error, 1)}
	e.cfg.Client = hc
	e.cfg.CallTimeout = time.Hour
	n := e.mustNote(e.repoID, "body")
	w := e.worker()

	ctx, cancel := context.WithCancel(context.Background())
	done := safego.Go(zerolog.Nop(), func() { w.Run(ctx) })
	<-hc.started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	// A shutdown mid-call leaves the claimed row due rather than failing it.
	if s := e.state(n.ID); s.State != models.NoteSyncPending {
		t.Errorf("sync after shutdown = %+v, want still pending", s)
	}
}

func TestRepoStoreOriginsMissingRepo(t *testing.T) {
	e := newEnv(t)
	origin, err := RepoStoreOrigins{Repos: e.repos}.OriginURL(context.Background(), "no-such-repo")
	if err != nil || origin != "" {
		t.Errorf("OriginURL(missing) = %q, %v; want empty, nil", origin, err)
	}
	if _, err := (RepoStoreOrigins{Repos: errRepos{}}).OriginURL(context.Background(), "x"); err == nil {
		t.Error("a repo store failure was swallowed")
	}
}

type errRepos struct{ db.RepoStore }

func (errRepos) Get(context.Context, string) (*models.Repo, error) {
	return nil, sql.ErrConnDone
}
