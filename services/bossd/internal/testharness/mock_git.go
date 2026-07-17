package testharness

import (
	"context"
	"fmt"
	"os"
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
	VerifyPushedBranchCalls       []verifyPushedBranchCall
	EmptyTrashCalls               []emptyTrashCall
	PurgeWorktreeCalls            []purgeWorktreeCall
	DeleteLocalBranchCalls        []string

	// DeleteLocalBranchErr is returned by DeleteLocalBranch when non-nil.
	DeleteLocalBranchErr error
	// BranchSafeToDeleteResult is returned by BranchSafeToDelete when
	// BranchSafeToDeleteFn is nil (default false).
	BranchSafeToDeleteResult bool
	// BranchSafeToDeleteFn overrides BranchSafeToDelete when set.
	BranchSafeToDeleteFn func(ctx context.Context, repoPath, branchTip, baseBranch string) (bool, error)

	// CreateFunc overrides the default Create behavior when set.
	CreateFunc func(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error)

	// CreateFromExistingBranchFunc overrides the default CreateFromExistingBranch behavior when set.
	CreateFromExistingBranchFunc func(ctx context.Context, opts gitpkg.CreateFromExistingBranchOpts) (*gitpkg.CreateResult, error)

	// CloneFunc overrides the default Clone behavior when set.
	CloneFunc func(ctx context.Context, cloneURL, localPath string) error

	// PushFunc overrides the default Push behavior when set.
	PushFunc func(ctx context.Context, worktreePath, branch string) error

	// PushWithLeaseFunc overrides the default PushWithLease behavior when set.
	PushWithLeaseFunc func(ctx context.Context, worktreePath, branch, expectedRemoteSHA string) (string, error)

	// InjectPRNumbersCalls records every InjectPRNumbers invocation so cron
	// finalize tests can assert the tag-injection step ran with the right PR.
	InjectPRNumbersCalls []injectPRNumbersCall
	// InjectPRNumbersFunc overrides the default InjectPRNumbers behavior when set.
	InjectPRNumbersFunc func(ctx context.Context, worktreePath, branch string, prNumber int, baseRef string) error

	// VerifyPushedBranchAheadOfBaseFunc overrides the default verification behavior when set.
	VerifyPushedBranchAheadOfBaseFunc func(ctx context.Context, worktreePath, branch, baseBranch string) (*gitpkg.BranchVerification, error)

	// StatusFunc overrides the default Status behavior when set.
	StatusFunc func(ctx context.Context, worktreePath string) (string, error)

	// LatestCommitSubjectFunc overrides the default LatestCommitSubject behavior when set.
	LatestCommitSubjectFunc func(ctx context.Context, worktreePath string) (string, error)

	// CommitSubjectsFunc overrides the default CommitSubjects behavior when set.
	CommitSubjectsFunc func(ctx context.Context, worktreePath, baseRef string) ([]string, error)

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

	// SyncBaseBranchErr is returned by SyncBaseBranch when non-nil. Used to
	// assert the merge paths tolerate a deferred (or failed) local sync.
	SyncBaseBranchErr error
	// RetryDeferredBaseSyncsCalls counts RetryDeferredBaseSyncs invocations.
	RetryDeferredBaseSyncsCalls int

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
	WorktreePath      string
	Branch            string
	ExpectedRemoteSHA string
}

type injectPRNumbersCall struct {
	WorktreePath string
	Branch       string
	PRNumber     int
	BaseRef      string
}

type verifyPushedBranchCall struct {
	WorktreePath string
	Branch       string
	BaseBranch   string
}

type emptyTrashCall struct {
	RepoPath string
	Branches []string
}

type purgeWorktreeCall struct {
	RepoPath        string
	RepoName        string
	WorktreeBaseDir string
	Branch          string
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

	// Real `git worktree add ... origin/<base>` fails with an unhelpful
	// "invalid reference" error when BaseBranch is empty. Reject here so
	// tests catch any caller that forgets to populate it (e.g. the original
	// cron-scheduler bug where `Repos.Get(...).DefaultBaseBranch` was
	// never consulted).
	if opts.BaseBranch == "" {
		return nil, fmt.Errorf("MockWorktreeManager.Create: BaseBranch is empty (the real git command would fail with 'invalid reference: origin/')")
	}

	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, opts)
	}

	// Default: generate a worktree path and branch name from the title.
	branch := sanitize(opts.Title)
	path := fmt.Sprintf("/tmp/worktrees/%s/%s", sanitize(opts.RepoName), branch)
	// Materialise on disk so callers that stat the path (spawnChatTmux) work.
	_ = os.MkdirAll(path, 0o755)
	return &gitpkg.CreateResult{
		WorktreePath: path,
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

// DeleteLocalBranchCalls records each branch passed to DeleteLocalBranch so
// archive-gating tests can assert the local-only delete ran.
func (m *MockWorktreeManager) DeleteLocalBranch(_ context.Context, _, branch string) error {
	m.mu.Lock()
	m.DeleteLocalBranchCalls = append(m.DeleteLocalBranchCalls, branch)
	err := m.DeleteLocalBranchErr
	m.mu.Unlock()
	return err
}

// BranchSafeToDelete returns BranchSafeToDeleteResult (default false) unless
// BranchSafeToDeleteFn is set.
func (m *MockWorktreeManager) BranchSafeToDelete(ctx context.Context, repoPath, branchTip, baseBranch string) (bool, error) {
	m.mu.Lock()
	fn := m.BranchSafeToDeleteFn
	result := m.BranchSafeToDeleteResult
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, repoPath, branchTip, baseBranch)
	}
	return result, nil
}

func (m *MockWorktreeManager) PurgeWorktree(_ context.Context, repoPath, repoName, worktreeBaseDir, branch string) {
	m.mu.Lock()
	m.PurgeWorktreeCalls = append(m.PurgeWorktreeCalls, purgeWorktreeCall{
		RepoPath:        repoPath,
		RepoName:        repoName,
		WorktreeBaseDir: worktreeBaseDir,
		Branch:          branch,
	})
	m.mu.Unlock()
}

func (m *MockWorktreeManager) EmptyCommit(_ context.Context, _, _ string) error {
	return nil
}

func (m *MockWorktreeManager) VerifyCurrentBranch(_ context.Context, _, _ string) error {
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

func (m *MockWorktreeManager) PushWithLease(ctx context.Context, worktreePath, branch, expectedRemoteSHA string) (string, error) {
	if expectedRemoteSHA == "" {
		return "", fmt.Errorf("expected remote SHA is required")
	}

	m.mu.Lock()
	m.PushCalls = append(m.PushCalls, pushCall{
		WorktreePath:      worktreePath,
		Branch:            branch,
		ExpectedRemoteSHA: expectedRemoteSHA,
	})
	injectedErr := m.pushError
	m.pushError = nil
	override := m.PushWithLeaseFunc
	m.mu.Unlock()

	if injectedErr != nil {
		return "", injectedErr
	}
	if override != nil {
		return override(ctx, worktreePath, branch, expectedRemoteSHA)
	}
	return "pushed-head-sha", nil
}

func (m *MockWorktreeManager) InjectPRNumbers(ctx context.Context, worktreePath, branch string, prNumber int, baseRef string) error {
	m.mu.Lock()
	m.InjectPRNumbersCalls = append(m.InjectPRNumbersCalls, injectPRNumbersCall{
		WorktreePath: worktreePath,
		Branch:       branch,
		PRNumber:     prNumber,
		BaseRef:      baseRef,
	})
	override := m.InjectPRNumbersFunc
	m.mu.Unlock()

	if override != nil {
		return override(ctx, worktreePath, branch, prNumber, baseRef)
	}
	return nil
}

func (m *MockWorktreeManager) VerifyPushedBranchAheadOfBase(ctx context.Context, worktreePath, branch, baseBranch string) (*gitpkg.BranchVerification, error) {
	m.mu.Lock()
	m.VerifyPushedBranchCalls = append(m.VerifyPushedBranchCalls, verifyPushedBranchCall{
		WorktreePath: worktreePath,
		Branch:       branch,
		BaseBranch:   baseBranch,
	})
	override := m.VerifyPushedBranchAheadOfBaseFunc
	m.mu.Unlock()

	if override != nil {
		return override(ctx, worktreePath, branch, baseBranch)
	}
	return &gitpkg.BranchVerification{
		HeadSHA:       "head-sha",
		BaseSHA:       "base-sha",
		RemoteHeadSHA: "head-sha",
		AheadCount:    1,
	}, nil
}

// SetPushError causes the next Push call to return err. After firing
// once it is cleared.
func (m *MockWorktreeManager) SetPushError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pushError = err
}

func (m *MockWorktreeManager) Status(ctx context.Context, worktreePath string) (string, error) {
	if m.StatusFunc != nil {
		return m.StatusFunc(ctx, worktreePath)
	}
	return "", nil
}

func (m *MockWorktreeManager) LatestCommitSubject(ctx context.Context, worktreePath string) (string, error) {
	if m.LatestCommitSubjectFunc != nil {
		return m.LatestCommitSubjectFunc(ctx, worktreePath)
	}
	return "", nil
}

func (m *MockWorktreeManager) CommitSubjects(ctx context.Context, worktreePath, baseRef string) ([]string, error) {
	if m.CommitSubjectsFunc != nil {
		return m.CommitSubjectsFunc(ctx, worktreePath, baseRef)
	}
	return nil, nil
}

func (m *MockWorktreeManager) BranchDebugSnapshot(_ context.Context, _, _, _ string) (*gitpkg.BranchDebugSnapshot, error) {
	return &gitpkg.BranchDebugSnapshot{}, nil
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

func (m *MockWorktreeManager) SyncBaseBranch(_ context.Context, _, _ string) error {
	return m.SyncBaseBranchErr
}

func (m *MockWorktreeManager) RetryDeferredBaseSyncs(_ context.Context) {
	m.RetryDeferredBaseSyncsCalls++
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

func (m *MockWorktreeManager) CreateFromExistingBranch(ctx context.Context, opts gitpkg.CreateFromExistingBranchOpts) (*gitpkg.CreateResult, error) {
	m.mu.Lock()
	m.CreateFromExistingBranchCalls = append(m.CreateFromExistingBranchCalls, opts)
	fn := m.CreateFromExistingBranchFunc
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, opts)
	}
	path := fmt.Sprintf("/tmp/worktrees/%s", opts.BranchName)
	_ = os.MkdirAll(path, 0o755)
	return &gitpkg.CreateResult{
		WorktreePath: path,
		BranchName:   opts.BranchName,
	}, nil
}

// sanitize converts a title to a simple branch-safe string.
func sanitize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}
