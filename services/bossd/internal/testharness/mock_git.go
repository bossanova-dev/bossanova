package testharness

import (
	"context"
	"fmt"
	"strings"
	"sync"

	gitpkg "github.com/recurser/bossd/internal/git"
)

var _ gitpkg.WorktreeManager = (*MockWorktreeManager)(nil)

// MockWorktreeManager is a mock WorktreeManager that records calls and
// returns configurable results without touching the filesystem.
type MockWorktreeManager struct {
	mu sync.Mutex

	CreateCalls                   []gitpkg.CreateOpts
	CreateFromExistingBranchCalls []gitpkg.CreateFromExistingBranchOpts
	CloneCalls                    []cloneCall
	ArchiveCalls                  []string
	ResurrectCalls                []gitpkg.ResurrectOpts
	PushCalls                     []pushCall
	EmptyTrashCalls               []emptyTrashCall

	// CreateFunc overrides the default Create behavior when set.
	CreateFunc func(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error)

	// CloneFunc overrides the default Clone behavior when set.
	CloneFunc func(ctx context.Context, cloneURL, localPath string) error

	// PushFunc overrides the default Push behavior when set.
	PushFunc func(ctx context.Context, worktreePath, branch string) error

	// DetectOriginURLResult is returned by DetectOriginURL.
	DetectOriginURLResult string

	// MergeLocalBranchCalls records every invocation so tests can verify the
	// local-only merge path of MergeSession.
	MergeLocalBranchCalls []mergeLocalCall
	// MergeLocalBranchErr is returned by MergeLocalBranch when non-nil.
	MergeLocalBranchErr error

	// IsAncestorFn overrides the default IsAncestor behavior (always true)
	// so tests can simulate verification failures (merge commit not on
	// origin/<base>, the madverts-core regression case).
	IsAncestorFn func(ctx context.Context, localPath, ref, target string) (bool, error)

	// FetchBaseErr is returned by FetchBase when non-nil. Used to simulate
	// network failures during post-merge verification.
	FetchBaseErr error

	// createErrorOnCall maps 1-indexed Create call numbers to errors.
	// When the call counter matches, the next Create returns this error
	// and the entry is consumed.
	createErrorOnCall map[int]error
	// pushError is returned by the next Push call when set, then cleared.
	pushError error
}

// mergeLocalCall records a single invocation of MergeLocalBranch. Tracked
// under mu so server and orchestrator goroutines can't race.
type mergeLocalCall struct {
	LocalPath string
	Base      string
	Head      string
	Strategy  string
}

type cloneCall struct {
	CloneURL  string
	LocalPath string
}

type pushCall struct {
	WorktreePath string
	Branch       string
}

type emptyTrashCall struct {
	RepoPath string
	Branches []string
}

// NewMockWorktreeManager creates a mock worktree manager with sensible defaults.
func NewMockWorktreeManager() *MockWorktreeManager {
	return &MockWorktreeManager{
		DetectOriginURLResult: "https://github.com/test/repo.git",
	}
}

func (m *MockWorktreeManager) Create(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	m.mu.Lock()
	m.CreateCalls = append(m.CreateCalls, opts)
	callNum := len(m.CreateCalls)
	injectedErr, hasInjected := m.createErrorOnCall[callNum]
	if hasInjected {
		delete(m.createErrorOnCall, callNum)
	}
	m.mu.Unlock()

	if hasInjected {
		return nil, injectedErr
	}

	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, opts)
	}

	// Default: generate a worktree path and branch name from the title.
	branch := sanitize(opts.Title)
	return &gitpkg.CreateResult{
		WorktreePath: fmt.Sprintf("/tmp/worktrees/%s/%s", sanitize(opts.RepoName), branch),
		BranchName:   branch,
	}, nil
}

// SetCreateError causes the next Create call to return err. After firing
// once it is cleared, so subsequent Create calls fall through to the
// default behavior (or CreateFunc if set).
func (m *MockWorktreeManager) SetCreateError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErrorOnCall == nil {
		m.createErrorOnCall = make(map[int]error)
	}
	m.createErrorOnCall[len(m.CreateCalls)+1] = err
}

// SetCreateErrorOnCall causes the Nth Create call (1-indexed, counted across
// the lifetime of the mock) to return err. Use 1 to fail the very next call.
func (m *MockWorktreeManager) SetCreateErrorOnCall(n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErrorOnCall == nil {
		m.createErrorOnCall = make(map[int]error)
	}
	m.createErrorOnCall[n] = err
}

func (m *MockWorktreeManager) Clone(ctx context.Context, cloneURL, localPath string) error {
	m.mu.Lock()
	m.CloneCalls = append(m.CloneCalls, cloneCall{CloneURL: cloneURL, LocalPath: localPath})
	m.mu.Unlock()

	if m.CloneFunc != nil {
		return m.CloneFunc(ctx, cloneURL, localPath)
	}
	return nil
}

func (m *MockWorktreeManager) Archive(ctx context.Context, worktreePath string) error {
	m.mu.Lock()
	m.ArchiveCalls = append(m.ArchiveCalls, worktreePath)
	m.mu.Unlock()
	return nil
}

func (m *MockWorktreeManager) Resurrect(ctx context.Context, opts gitpkg.ResurrectOpts) error {
	m.mu.Lock()
	m.ResurrectCalls = append(m.ResurrectCalls, opts)
	m.mu.Unlock()
	return nil
}

func (m *MockWorktreeManager) EmptyTrash(ctx context.Context, repoPath string, branches []string) error {
	m.mu.Lock()
	m.EmptyTrashCalls = append(m.EmptyTrashCalls, emptyTrashCall{RepoPath: repoPath, Branches: branches})
	m.mu.Unlock()
	return nil
}

func (m *MockWorktreeManager) EmptyCommit(_ context.Context, _, _ string) error {
	return nil
}

func (m *MockWorktreeManager) Push(ctx context.Context, worktreePath, branch string) error {
	m.mu.Lock()
	m.PushCalls = append(m.PushCalls, pushCall{WorktreePath: worktreePath, Branch: branch})
	injectedErr := m.pushError
	m.pushError = nil
	m.mu.Unlock()

	if injectedErr != nil {
		return injectedErr
	}
	if m.PushFunc != nil {
		return m.PushFunc(ctx, worktreePath, branch)
	}
	return nil
}

// SetPushError causes the next Push call to return err. After firing
// once it is cleared.
func (m *MockWorktreeManager) SetPushError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pushError = err
}

func (m *MockWorktreeManager) DetectOriginURL(ctx context.Context, repoPath string) (string, error) {
	return m.DetectOriginURLResult, nil
}

func (m *MockWorktreeManager) IsGitRepo(ctx context.Context, path string) bool {
	return true
}

func (m *MockWorktreeManager) DetectDefaultBranch(ctx context.Context, repoPath string) (string, error) {
	return "main", nil
}

func (m *MockWorktreeManager) EnsureBaseBranchReadyForSync(_ context.Context, _, _ string) error {
	return nil
}

func (m *MockWorktreeManager) SyncBaseBranch(_ context.Context, _, _ string) error {
	return nil
}

func (m *MockWorktreeManager) IsAncestor(ctx context.Context, localPath, ref, target string) (bool, error) {
	m.mu.Lock()
	fn := m.IsAncestorFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, localPath, ref, target)
	}
	return true, nil
}

func (m *MockWorktreeManager) MergeLocalBranch(_ context.Context, localPath, base, head, strategy string) error {
	m.mu.Lock()
	m.MergeLocalBranchCalls = append(m.MergeLocalBranchCalls, mergeLocalCall{
		LocalPath: localPath, Base: base, Head: head, Strategy: strategy,
	})
	err := m.MergeLocalBranchErr
	m.mu.Unlock()
	return err
}

func (m *MockWorktreeManager) FetchBase(_ context.Context, _, _ string) error {
	m.mu.Lock()
	err := m.FetchBaseErr
	m.mu.Unlock()
	return err
}

func (m *MockWorktreeManager) CreateFromExistingBranch(_ context.Context, opts gitpkg.CreateFromExistingBranchOpts) (*gitpkg.CreateResult, error) {
	m.mu.Lock()
	m.CreateFromExistingBranchCalls = append(m.CreateFromExistingBranchCalls, opts)
	m.mu.Unlock()
	return &gitpkg.CreateResult{
		WorktreePath: fmt.Sprintf("/tmp/worktrees/%s", opts.BranchName),
		BranchName:   opts.BranchName,
	}, nil
}

// sanitize converts a title to a simple branch-safe string.
func sanitize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}
