package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPreparedWorktreeFailureTypes(t *testing.T) {
	cause := context.DeadlineExceeded
	err := &PreparedWorktreeKeptError{Cause: cause, WorktreePath: "/prepared", Branch: "topic", Attempts: 3, KeptUntil: time.Now()}
	if !errors.Is(err, cause) {
		t.Fatal("lost cause")
	}
	failed := &AgentStartFailedError{Cause: cause, Attempts: 3}
	if !failed.Transient() {
		t.Fatal("deadline failure not retryable")
	}
}

func TestKeptWorktreeCleanupRequiresMatchingMarker(t *testing.T) {
	// Missing markers must never suppress ordinary cleanup.
	sessions := newMockSessionStore()
	sessions.sessions["s"] = &models.Session{ID: "s", RepoID: "r", WorktreePath: t.TempDir(), BranchName: "topic", WorktreeCreatedAt: ptrTestOwnershipTime(), IsBranchCreated: true}
	repos := newMockRepoStore()
	repos.repos["r"] = &models.Repo{ID: "r", LocalPath: "/repo"}
	wt := newArtifactWorktreeManager()
	CleanUpBootstrapArtifacts(context.Background(), zerolog.Nop(), sessions, repos, wt, "s", &PreparedWorktreeKeptError{Cause: context.DeadlineExceeded})
	if len(wt.purgedBranches) != 1 || len(wt.reapedBranches) != 1 {
		t.Fatal("unmarked worktree retained")
	}
}

func keptFixture(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	path := filepath.Join(t.TempDir(), "prepared")
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	git("init", "-b", "main")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	git("worktree", "add", "-b", "topic", path)
	return repo, path
}
func writeKeptFixture(t *testing.T, path string, until time.Time) gitpkg.PreparedWorktreeMarker {
	t.Helper()
	head, err := gitpkg.PreparedWorktreeHead(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	marker := gitpkg.PreparedWorktreeMarker{Version: 1, SessionID: "s", RepoID: "r", Branch: "topic", BaseBranch: "main", IsBranchCreated: true, HeadSHA: head, KeptAt: until.Add(-24 * time.Hour), KeptUntil: until, Reason: "load"}
	if err := gitpkg.WritePreparedWorktreeMarker(context.Background(), path, marker); err != nil {
		t.Fatal(err)
	}
	return marker
}
func TestKeptWorktreeCleanupPreservesOnlyValidOutcome(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			repo, path := keptFixture(t)
			marker := writeKeptFixture(t, path, time.Now().Add(time.Hour))
			sessions := newMockSessionStore()
			sessions.sessions["s"] = &models.Session{ID: "s", RepoID: "r", WorktreePath: path, BranchName: "topic", WorktreeCreatedAt: ptrTestOwnershipTime(), IsBranchCreated: true}
			repos := newMockRepoStore()
			repos.repos["r"] = &models.Repo{ID: "r", LocalPath: repo}
			wt := newArtifactWorktreeManager()
			cause := errors.New("permanent")
			if valid {
				cause = &PreparedWorktreeKeptError{Cause: context.DeadlineExceeded, WorktreePath: path, Branch: "topic", KeptUntil: marker.KeptUntil}
			}
			CleanUpBootstrapArtifacts(context.Background(), zerolog.Nop(), sessions, repos, wt, "s", cause)
			want := 1
			if valid {
				want = 0
			}
			if len(wt.purgedBranches) != want || len(wt.reapedBranches) != want {
				t.Fatalf("purges=%v reaps=%v", wt.purgedBranches, wt.reapedBranches)
			}
		})
	}
}
func TestReapExpiredKeptWorktrees(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		expired, referenced, owned bool
		want                       int
	}{{"expired owned", true, false, true, 1}, {"expired existing branch", true, false, false, 1}, {"unexpired", false, false, true, 0}, {"referenced", true, true, true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			repo, path := keptFixture(t)
			now := time.Now()
			until := now.Add(time.Hour)
			if tc.expired {
				until = now.Add(-time.Hour)
			}
			marker := writeKeptFixture(t, path, until)
			marker.IsBranchCreated = tc.owned
			if err := gitpkg.WritePreparedWorktreeMarker(context.Background(), path, marker); err != nil {
				t.Fatal(err)
			}
			sessions := newMockSessionStore()
			if tc.referenced {
				sessions.sessions["live"] = &models.Session{ID: "live", RepoID: "another-repo", WorktreePath: path}
			}
			repos := newMockRepoStore()
			repos.repos["r"] = &models.Repo{ID: "r", LocalPath: repo}
			wt := newArtifactWorktreeManager()
			l := &Lifecycle{sessions: sessions, repos: repos, worktrees: wt, logger: zerolog.Nop()}
			got, err := l.reapExpiredKeptWorktrees(context.Background(), now)
			if err != nil || got != tc.want {
				t.Fatalf("got=%d err=%v", got, err)
			}
			wantReaps := 0
			if tc.owned {
				wantReaps = tc.want
			}
			if len(wt.purgedBranches) != tc.want || len(wt.reapedBranches) != wantReaps {
				t.Fatalf("purges=%v reaps=%v", wt.purgedBranches, wt.reapedBranches)
			}
		})
	}
}

type keptSetupManager struct {
	*mockWorktreeManager
	setupErr error
}

func (m *keptSetupManager) Create(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	r, err := m.mockWorktreeManager.Create(ctx, opts)
	if r != nil {
		r.SetupErr = m.setupErr
	}
	return r, err
}
func TestStartSessionKeepsPreparedWorktree(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		degraded, brokenMarker bool
	}{{"healthy", false, false}, {"degraded", true, false}, {"marker failure", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, path := keptFixture(t)
			h := retryIntegrationHarness(t)
			h.wt.worktreePath = path
			sess := h.sessions.sessions["sess-1"]
			sess.WorktreePath = path
			sess.BranchName = "test-session"
			sess.WorktreeCreatedAt = ptrTestOwnershipTime()
			sess.IsBranchCreated = true
			if tc.brokenMarker {
				h.wt.worktreePath = t.TempDir()
				sess.WorktreePath = h.wt.worktreePath
			}
			if tc.degraded {
				h.lc.worktrees = &keptSetupManager{mockWorktreeManager: h.wt, setupErr: errors.New("setup failed")}
			}
			runner := &retryPreflightRunner{mockAgentRunner: h.agentRun, failures: 99}
			h.lc.agentRunner = runner
			err := h.lc.StartSession(context.Background(), "sess-1", StartSessionOpts{IsTmuxUnattended: true, DeferPR: true, HeadlessCapabilityProfile: pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1})
			var kept *PreparedWorktreeKeptError
			want := !tc.degraded && !tc.brokenMarker
			if errors.As(err, &kept) != want {
				t.Fatalf("kept=%v want=%v err=%v", kept, want, err)
			}
			if want {
				if kept.Attempts != 3 {
					t.Fatalf("attempts=%d", kept.Attempts)
				}
				if _, err := gitpkg.ReadPreparedWorktreeMarker(context.Background(), path); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
			}
			var failed *AgentStartFailedError
			if !errors.As(err, &failed) || status.Code(failed.Cause) != codes.DeadlineExceeded {
				t.Fatalf("lost typed launch failure: %v", err)
			}
		})
	}
}

type keptMarkerRefreshStore struct {
	db.SessionStore
	refresh func()
}

func (s *keptMarkerRefreshStore) List(ctx context.Context, repoID string) ([]*models.Session, error) {
	s.refresh()
	return s.SessionStore.List(ctx, repoID)
}
func TestReapExpiredKeptWorktreesRechecksMarker(t *testing.T) {
	repo, path := keptFixture(t)
	now := time.Now()
	writeKeptFixture(t, path, now.Add(-time.Hour))
	sessions := &keptMarkerRefreshStore{SessionStore: newMockSessionStore(), refresh: func() { writeKeptFixture(t, path, now.Add(time.Hour)) }}
	repos := newMockRepoStore()
	repos.repos["r"] = &models.Repo{ID: "r", LocalPath: repo}
	wt := newArtifactWorktreeManager()
	l := &Lifecycle{sessions: sessions, repos: repos, worktrees: wt, logger: zerolog.Nop()}
	got, err := l.reapExpiredKeptWorktrees(context.Background(), now)
	if err != nil || got != 0 || len(wt.purgedBranches) != 0 {
		t.Fatalf("refreshed marker purged: reaped=%d purges=%v err=%v", got, wt.purgedBranches, err)
	}
}

func TestReapExpiredKeptWorktreesSkipsPendingPR(t *testing.T) {
	for _, branch := range []string{"topic", ""} {
		t.Run("branch="+branch, func(t *testing.T) {
			repo, path := keptFixture(t)
			now := time.Now()
			writeKeptFixture(t, path, now.Add(-time.Hour))
			pr := 123
			release, _, err := AcquireTargetStart(context.Background(), "r", branch, &pr)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			sessions := newMockSessionStore()
			// A PR bootstrap has claimed its row but has not recorded its path yet.
			sessions.sessions["live"] = &models.Session{ID: "live", RepoID: "r", BranchName: branch, PRNumber: &pr}
			repos := newMockRepoStore()
			repos.repos["r"] = &models.Repo{ID: "r", LocalPath: repo}
			wt := newArtifactWorktreeManager()
			l := &Lifecycle{sessions: sessions, repos: repos, worktrees: wt, logger: zerolog.Nop()}
			got, err := l.reapExpiredKeptWorktrees(context.Background(), now)
			if err != nil || got != 0 || len(wt.purgedBranches) != 0 {
				t.Fatalf("pending PR worktree purged: reaped=%d purges=%v err=%v", got, wt.purgedBranches, err)
			}
		})
	}
}

type blockedKeptPurgeManager struct {
	*artifactWorktreeManager
	entered chan struct{}
	resume  chan struct{}
}

func (m *blockedKeptPurgeManager) PurgeWorktree(ctx context.Context, repo, name, base, path string) error {
	close(m.entered)
	select {
	case <-m.resume:
		return m.artifactWorktreeManager.PurgeWorktree(ctx, repo, name, base, path)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestReapExpiredKeptWorktreesSerializesNewClaims(t *testing.T) {
	repo, path := keptFixture(t)
	now := time.Now()
	writeKeptFixture(t, path, now.Add(-time.Hour))
	repos := newMockRepoStore()
	repos.repos["r"] = &models.Repo{ID: "r", LocalPath: repo}
	wt := &blockedKeptPurgeManager{artifactWorktreeManager: newArtifactWorktreeManager(), entered: make(chan struct{}), resume: make(chan struct{})}
	l := &Lifecycle{sessions: newMockSessionStore(), repos: repos, worktrees: wt, logger: zerolog.Nop()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := l.reapExpiredKeptWorktrees(ctx, now); done <- err }()
	select {
	case <-wt.entered:
	case <-ctx.Done():
		t.Fatal("sweep did not reach purge")
	}
	// PR and branch creation both insert their row under this gate. No new
	// claim may slip between the sweep's row check and destructive cleanup.
	release, _, err := acquireStartPathWithin(ctx, "r", 20*time.Millisecond)
	if err == nil {
		release()
	}
	close(wt.resume)
	if sweepErr := <-done; sweepErr != nil {
		t.Fatal(sweepErr)
	}
	if err == nil {
		t.Fatal("new session claim entered during expired worktree purge")
	}
	release, _, err = AcquireStartPath(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// The manager returns a prepared linked worktree while the real launch path
// decides when the marker may be consumed.
type reusedKeptManager struct {
	*mockWorktreeManager
	owned bool
}

func (m *reusedKeptManager) Create(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	r, err := m.mockWorktreeManager.Create(ctx, opts)
	if err == nil {
		r.Reused = true
		r.IsBranchCreated = m.owned
		if opts.OnWorktreeAdded != nil {
			err = opts.OnWorktreeAdded(r.WorktreePath)
		}
		if opts.OnWorktreeReady != nil {
			opts.OnWorktreeReady(ctx, r.BranchName, r.WorktreePath, m.owned)
		}
	}
	return r, err
}
func TestStartSessionReuseKeptMarker(t *testing.T) {
	for _, fail := range []bool{false, true} {
		for _, owned := range []bool{false, true} {
			t.Run(fmt.Sprintf("failure=%v/owned=%v", fail, owned), func(t *testing.T) {
				_, path := keptFixture(t)
				old := writeKeptFixture(t, path, time.Now().Add(time.Hour))
				h := retryIntegrationHarness(t)
				h.wt.worktreePath = path
				h.lc.worktrees = &reusedKeptManager{mockWorktreeManager: h.wt, owned: owned}
				if fail {
					h.lc.agentRunner = &retryPreflightRunner{mockAgentRunner: h.agentRun, failures: 99}
				}
				var output bytes.Buffer
				err := h.lc.StartSession(context.Background(), "sess-1", StartSessionOpts{IsTmuxUnattended: true, DeferPR: true, SetupOutput: &output, HeadlessCapabilityProfile: pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1})
				marker, markerErr := gitpkg.ReadPreparedWorktreeMarker(context.Background(), path)
				if fail {
					var kept *PreparedWorktreeKeptError
					if !errors.As(err, &kept) || markerErr != nil || !marker.KeptUntil.After(old.KeptUntil) || marker.Claimed {
						t.Fatalf("failure=%v marker=%+v markerErr=%v", err, marker, markerErr)
					}
				} else if err != nil || !errors.Is(markerErr, os.ErrNotExist) {
					t.Fatalf("start=%v marker=%v", err, markerErr)
				}
				sess := h.sessions.sessions["sess-1"]
				if sess.WorktreeCreatedAt == nil || sess.IsBranchCreated != owned {
					t.Fatalf("ownership=%+v", sess)
				}
				if !strings.Contains(output.String(), "reusing prepared worktree kept at "+path+"; setup script skipped") {
					t.Fatalf("progress=%s", output.String())
				}
			})
		}
	}
}
