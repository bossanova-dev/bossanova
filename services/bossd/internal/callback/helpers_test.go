package callback

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// migrationsDir resolves services/bossd/migrations relative to this test file.
func migrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// newStore returns a migrated in-memory GithubCallback store (single
// connection; fine for the sequential unit tests).
func newStore(t *testing.T) *db.SQLiteGithubCallbackStore {
	t.Helper()
	d, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := migrate.Run(d, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return db.NewGithubCallbackStore(d)
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
