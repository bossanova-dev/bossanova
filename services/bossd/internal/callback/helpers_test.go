package callback

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

var (
	callbackSchemaOnce sync.Once
	callbackSchemaPath string
	callbackSchemaErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if callbackSchemaPath != "" {
		_ = os.Remove(callbackSchemaPath)
		_ = os.Remove(callbackSchemaPath + "-shm")
		_ = os.Remove(callbackSchemaPath + "-wal")
	}
	os.Exit(code)
}

// migrationsDir resolves services/bossd/migrations relative to this test file.
func migrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// newStore returns an isolated GithubCallback store. The migration chain is
// expensive under -race, so it is run once into a closed template database and
// each sequential unit test receives a copy. Keep the pool at one connection
// to retain the in-memory fixture's deterministic behavior.
func newStore(t *testing.T) *db.SQLiteGithubCallbackStore {
	t.Helper()
	template := callbackSchemaTemplate(t)
	path := filepath.Join(t.TempDir(), "cb.db")
	contents, err := os.ReadFile(template)
	if err != nil {
		t.Fatalf("read callback schema template: %v", err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("copy callback schema template: %v", err)
	}
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("open callback db: %v", err)
	}
	d.SetMaxOpenConns(1)
	d.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = d.Close() })
	return db.NewGithubCallbackStore(d)
}

func callbackSchemaTemplate(t *testing.T) string {
	t.Helper()
	callbackSchemaOnce.Do(func() {
		f, err := os.CreateTemp("", "bossd-callback-schema-*.db")
		if err != nil {
			callbackSchemaErr = err
			return
		}
		callbackSchemaPath = f.Name()
		if err := f.Close(); err != nil {
			callbackSchemaErr = err
			return
		}
		d, err := db.Open(callbackSchemaPath)
		if err != nil {
			callbackSchemaErr = err
			return
		}
		callbackSchemaErr = migrate.Run(d, os.DirFS(migrationsDir()))
		if err := d.Close(); err != nil && callbackSchemaErr == nil {
			callbackSchemaErr = err
		}
	})
	if callbackSchemaErr != nil {
		t.Fatalf("prepare callback schema template: %v", callbackSchemaErr)
	}
	return callbackSchemaPath
}

func TestNewStoreKeepsCallbackRowsIsolated(t *testing.T) {
	first := newStore(t)
	mustCreate(t, first, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 1,
		Trigger: models.GithubCallbackTriggerMerged, Message: "first",
	})

	second := newStore(t)
	callbacks, err := second.List(context.Background(), db.ListGithubCallbacksFilter{})
	if err != nil {
		t.Fatalf("list callbacks in second store: %v", err)
	}
	if len(callbacks) != 0 {
		t.Fatalf("second store has %d callbacks, want none", len(callbacks))
	}

	callbacks, err = first.List(context.Background(), db.ListGithubCallbacksFilter{})
	if err != nil {
		t.Fatalf("list callbacks in first store: %v", err)
	}
	if len(callbacks) != 1 {
		t.Fatalf("first store has %d callbacks, want 1", len(callbacks))
	}
}

// newFileStore returns a migrated file-backed store that supports real
// concurrency (multiple connections), used for lease/trigger race tests.
func newFileStore(t *testing.T) *db.SQLiteGithubCallbackStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cb.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Run(d, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return db.NewGithubCallbackStore(d)
}

// fakeProvider is a fake vcs provider returning canned PR status + checks.
type fakeProvider struct {
	status    *vcs.PRStatus
	statusErr error
	checks    []vcs.CheckResult
	checksErr error
}

func (f *fakeProvider) GetPRStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeProvider) GetCheckResults(_ context.Context, _ string, _ int) ([]vcs.CheckResult, error) {
	return f.checks, f.checksErr
}

// scriptedProvider is a mutex-guarded vcs provider whose status/checks can be
// mutated between rounds, unlike fakeProvider's fixed snapshot — used by flow
// tests that need to flip PR state (e.g. Draft) across successive evaluations.
type scriptedProvider struct {
	mu     sync.Mutex
	status *vcs.PRStatus
	checks []vcs.CheckResult
}

func (s *scriptedProvider) set(status *vcs.PRStatus, checks []vcs.CheckResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.checks = checks
}

func (s *scriptedProvider) GetPRStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, nil
}

func (s *scriptedProvider) GetCheckResults(_ context.Context, _ string, _ int) ([]vcs.CheckResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks, nil
}

// conclusion returns a pointer to a CheckConclusion.
func conclusion(c vcs.CheckConclusion) *vcs.CheckConclusion { return &c }

// completedCheck builds a completed check with the given conclusion.
func completedCheck(id string, c vcs.CheckConclusion) vcs.CheckResult {
	return vcs.CheckResult{ID: id, Name: id, Status: vcs.CheckStatusCompleted, Conclusion: conclusion(c)}
}

// pendingCheck builds an in-progress check.
func pendingCheck(id string) vcs.CheckResult {
	return vcs.CheckResult{ID: id, Name: id, Status: vcs.CheckStatusInProgress}
}

// mustCreate inserts an active callback and fails the test on error.
func mustCreate(t *testing.T, store db.GithubCallbackStore, params db.CreateGithubCallbackParams) *models.GithubCallback {
	t.Helper()
	cb, err := store.Create(context.Background(), params)
	if err != nil {
		t.Fatalf("create callback: %v", err)
	}
	return cb
}

// getState fetches the current state of a callback.
func getState(t *testing.T, store db.GithubCallbackStore, id string) models.GithubCallbackState {
	t.Helper()
	cb, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get callback %s: %v", id, err)
	}
	return cb.State
}

// captureDeliverer records the (id, message) of every delivery.
type captureDeliverer struct {
	mu   chan struct{} // used as a 1-slot lock
	ids  []string
	msgs []string
	err  error
}

func newCaptureDeliverer(err error) *captureDeliverer {
	c := &captureDeliverer{mu: make(chan struct{}, 1), err: err}
	c.mu <- struct{}{}
	return c
}

func (c *captureDeliverer) Deliver(_ context.Context, id, message string) error {
	<-c.mu
	defer func() { c.mu <- struct{}{} }()
	if c.err != nil {
		return c.err
	}
	c.ids = append(c.ids, id)
	c.msgs = append(c.msgs, message)
	return nil
}

func (c *captureDeliverer) count() int {
	<-c.mu
	defer func() { c.mu <- struct{}{} }()
	return len(c.ids)
}

func (c *captureDeliverer) lastMessage() string {
	<-c.mu
	defer func() { c.mu <- struct{}{} }()
	if len(c.msgs) == 0 {
		return ""
	}
	return c.msgs[len(c.msgs)-1]
}
