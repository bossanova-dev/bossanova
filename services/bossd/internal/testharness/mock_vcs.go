package testharness

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/recurser/bossalib/vcs"
)

// MockVCSMode controls how the MockVCSProvider responds to operations.
// Use SetMode to activate the desired mode; VCSModeSuccess is the default.
type MockVCSMode int

const (
	// VCSModeSuccess is the default — all operations succeed.
	VCSModeSuccess MockVCSMode = iota
	// VCSModeNoGitHub makes GitHubNWO-gated calls behave as if the repo has no
	// GitHub remote. Concretely: CreateDraftPR returns an error flagged as
	// ErrNoGitHub so the lifecycle knows to skip the PR path.
	VCSModeNoGitHub
	// VCSModePushFail causes every Push call to return an error.
	VCSModePushFail
	// VCSModeCreatePRFail causes every CreateDraftPR call to return an error.
	VCSModeCreatePRFail
)

var _ vcs.Provider = (*MockVCSProvider)(nil)

// ErrNoGitHub is returned by CreateDraftPR when mode is VCSModeNoGitHub.
var ErrNoGitHub = errors.New("no GitHub remote configured")

// MockVCSProvider is a mock VCS provider for E2E tests.
type MockVCSProvider struct {
	mu   sync.Mutex
	mode MockVCSMode

	CreateDraftPRCalls      []vcs.CreatePROpts
	MarkReadyForReviewCalls []markReadyCall
	MergePRCalls            []mergePRCall
	prCounter               atomic.Int32

	// PRStatus is the default returned by GetPRStatus when no per-PR
	// override has been set. Defaults to open+mergeable.
	PRStatus *vcs.PRStatus

	// prStatuses optionally overrides the default PRStatus on a per-PR basis.
	// Set via SetPRStatus.
	prStatuses map[int]*vcs.PRStatus

	// CheckResults is returned by GetCheckResults. Defaults to empty.
	CheckResults []vcs.CheckResult

	// FailedCheckLogs is returned by GetFailedCheckLogs.
	FailedCheckLogs string

	// ReviewComments is returned by GetReviewComments.
	ReviewComments []vcs.ReviewComment

	// OpenPRs is returned by ListOpenPRs.
	OpenPRs []vcs.PRSummary

	// ClosedPRs is returned by ListClosedPRs.
	ClosedPRs []vcs.PRSummary

	// CreateDraftPRFunc overrides the default CreateDraftPR behavior when set.
	CreateDraftPRFunc func(ctx context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error)

	// MergePRErr is returned by MergePR when set.
	MergePRErr error

	// PRMergeCommit is returned by GetPRMergeCommit when no per-PR override
	// is configured via PRMergeCommits. Empty → the mock falls back to a
	// deterministic "mock-merge-<prID>" sentinel.
	PRMergeCommit string

	// PRMergeCommits overrides the merge commit on a per-PR basis. Set via
	// SetPRMergeCommit. Primary use: drive the "merge commit not on base"
	// negative-path test.
	PRMergeCommits map[int]string

	// GetPRMergeCommitErr is returned by every GetPRMergeCommit call when
	// non-nil. Used to simulate vcs.ErrPRNotMerged.
	GetPRMergeCommitErr error

	// AllowedMergeStrategies is returned by GetAllowedMergeStrategies when
	// non-nil. nil → the mock returns ["merge","squash","rebase"] so the
	// configured strategy is always allowed by default.
	AllowedMergeStrategies []string

	// GetAllowedMergeStrategiesErr is returned by every
	// GetAllowedMergeStrategies call when non-nil.
	GetAllowedMergeStrategiesErr error

	// createPRError is returned by the next CreateDraftPR call when set,
	// then cleared.
	createPRError error
}

type markReadyCall struct {
	RepoPath string
	PRID     int
}

type mergePRCall struct {
	RepoPath string
	PRID     int
	Strategy string
}

// NewMockVCSProvider creates a mock VCS provider with sensible defaults.
func NewMockVCSProvider() *MockVCSProvider {
	mergeable := true
	return &MockVCSProvider{
		PRStatus: &vcs.PRStatus{
			State:     vcs.PRStateOpen,
			Mergeable: &mergeable,
		},
	}
}

// SetMode switches the provider to one of the named failure modes.
// VCSModeSuccess restores normal (all-pass) behavior.
func (m *MockVCSProvider) SetMode(mode MockVCSMode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
}

func (m *MockVCSProvider) CreateDraftPR(ctx context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
	m.mu.Lock()
	m.CreateDraftPRCalls = append(m.CreateDraftPRCalls, opts)
	injectedErr := m.createPRError
	m.createPRError = nil
	mode := m.mode
	m.mu.Unlock()

	if injectedErr != nil {
		return nil, injectedErr
	}
	switch mode {
	case VCSModeSuccess, VCSModePushFail:
		// Success: fall through to normal behavior.
		// PushFail: push failure is handled on the git mock; PR creation proceeds.
	case VCSModeNoGitHub:
		return nil, ErrNoGitHub
	case VCSModeCreatePRFail:
		return nil, errors.New("mock: CreateDraftPR failed (VCSModeCreatePRFail)")
	}

	if m.CreateDraftPRFunc != nil {
		return m.CreateDraftPRFunc(ctx, opts)
	}

	prNum := int(m.prCounter.Add(1))
	return &vcs.PRInfo{
		Number: prNum,
		URL:    fmt.Sprintf("https://github.com/test/repo/pull/%d", prNum),
	}, nil
}

// SetCreatePRError causes the next CreateDraftPR call to return err. After
// firing once it is cleared.
func (m *MockVCSProvider) SetCreatePRError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createPRError = err
}

// SetMergePRError sets the error returned by every subsequent MergePR call.
// Pass nil to clear. The MergePRErr field is exported and may also be set
// directly, but callers in resilience tests should prefer this setter so
// the mu-guarded write is safe under concurrent access.
func (m *MockVCSProvider) SetMergePRError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MergePRErr = err
}

func (m *MockVCSProvider) GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if status, ok := m.prStatuses[prID]; ok {
		return status, nil
	}
	return m.PRStatus, nil
}

// SetPRStatus overrides the status returned by GetPRStatus for a specific PR.
// Pass status=nil to clear the override and fall back to PRStatus.
func (m *MockVCSProvider) SetPRStatus(prID int, status *vcs.PRStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prStatuses == nil {
		m.prStatuses = make(map[int]*vcs.PRStatus)
	}
	if status == nil {
		delete(m.prStatuses, prID)
		return
	}
	m.prStatuses[prID] = status
}

func (m *MockVCSProvider) GetCheckResults(ctx context.Context, repoPath string, prID int) ([]vcs.CheckResult, error) {
	return m.CheckResults, nil
}

func (m *MockVCSProvider) GetFailedCheckLogs(ctx context.Context, repoPath string, checkID string) (string, error) {
	return m.FailedCheckLogs, nil
}

func (m *MockVCSProvider) MarkReadyForReview(ctx context.Context, repoPath string, prID int) error {
	m.mu.Lock()
	m.MarkReadyForReviewCalls = append(m.MarkReadyForReviewCalls, markReadyCall{RepoPath: repoPath, PRID: prID})
	m.mu.Unlock()
	return nil
}

func (m *MockVCSProvider) GetReviewComments(ctx context.Context, repoPath string, prID int) ([]vcs.ReviewComment, error) {
	return m.ReviewComments, nil
}

func (m *MockVCSProvider) ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	return m.OpenPRs, nil
}

func (m *MockVCSProvider) ListClosedPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	return m.ClosedPRs, nil
}

func (m *MockVCSProvider) UpdatePRTitle(_ context.Context, _ string, _ int, _ string) error {
	return nil
}

func (m *MockVCSProvider) MergePR(ctx context.Context, repoPath string, prID int, strategy string) error {
	m.mu.Lock()
	m.MergePRCalls = append(m.MergePRCalls, mergePRCall{RepoPath: repoPath, PRID: prID, Strategy: strategy})
	err := m.MergePRErr
	m.mu.Unlock()
	return err
}

// GetPRMergeCommit returns the mock-configured merge commit for the PR.
// Defaults to PRMergeCommit (or "mock-merge-<prID>" if unset) so the happy
// path of post-merge verification works without explicit per-test setup.
func (m *MockVCSProvider) GetPRMergeCommit(_ context.Context, _ string, prID int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.GetPRMergeCommitErr != nil {
		return "", m.GetPRMergeCommitErr
	}
	if sha, ok := m.PRMergeCommits[prID]; ok {
		return sha, nil
	}
	if m.PRMergeCommit != "" {
		return m.PRMergeCommit, nil
	}
	return fmt.Sprintf("mock-merge-%d", prID), nil
}

// SetPRMergeCommit overrides the merge commit returned for a specific PR.
// Pass sha="" to clear the override and fall back to PRMergeCommit.
func (m *MockVCSProvider) SetPRMergeCommit(prID int, sha string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.PRMergeCommits == nil {
		m.PRMergeCommits = make(map[int]string)
	}
	if sha == "" {
		delete(m.PRMergeCommits, prID)
		return
	}
	m.PRMergeCommits[prID] = sha
}

// GetAllowedMergeStrategies returns AllowedMergeStrategies when set, else
// ["merge", "squash", "rebase"] so all strategies are permitted by default.
func (m *MockVCSProvider) GetAllowedMergeStrategies(_ context.Context, _ string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.GetAllowedMergeStrategiesErr != nil {
		return nil, m.GetAllowedMergeStrategiesErr
	}
	if m.AllowedMergeStrategies != nil {
		out := make([]string, len(m.AllowedMergeStrategies))
		copy(out, m.AllowedMergeStrategies)
		return out, nil
	}
	return []string{"merge", "squash", "rebase"}, nil
}
