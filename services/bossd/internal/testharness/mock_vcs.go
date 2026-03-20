package testharness

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/recurser/bossalib/vcs"
)

var _ vcs.Provider = (*MockVCSProvider)(nil)

// MockVCSProvider is a mock VCS provider for E2E tests.
type MockVCSProvider struct {
	mu sync.Mutex

	CreateDraftPRCalls      []vcs.CreatePROpts
	MarkReadyForReviewCalls []markReadyCall
	MergePRCalls            []mergePRCall
	prCounter               atomic.Int32

	// PRStatus is returned by GetPRStatus. Defaults to open.
	PRStatus *vcs.PRStatus

	// CheckResults is returned by GetCheckResults. Defaults to empty.
	CheckResults []vcs.CheckResult

	// FailedCheckLogs is returned by GetFailedCheckLogs.
	FailedCheckLogs string

	// ReviewComments is returned by GetReviewComments.
	ReviewComments []vcs.ReviewComment

	// OpenPRs is returned by ListOpenPRs.
	OpenPRs []vcs.PRSummary

	// CreateDraftPRFunc overrides the default CreateDraftPR behavior when set.
	CreateDraftPRFunc func(ctx context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error)

	// MergePRErr is returned by MergePR when set.
	MergePRErr error
}

type markReadyCall struct {
	RepoPath string
	PRID     int
}

type mergePRCall struct {
	RepoPath string
	PRID     int
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

func (m *MockVCSProvider) CreateDraftPR(ctx context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
	m.mu.Lock()
	m.CreateDraftPRCalls = append(m.CreateDraftPRCalls, opts)
	m.mu.Unlock()

	if m.CreateDraftPRFunc != nil {
		return m.CreateDraftPRFunc(ctx, opts)
	}

	prNum := int(m.prCounter.Add(1))
	return &vcs.PRInfo{
		Number: prNum,
		URL:    fmt.Sprintf("https://github.com/test/repo/pull/%d", prNum),
	}, nil
}

func (m *MockVCSProvider) GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	return m.PRStatus, nil
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

func (m *MockVCSProvider) MergePR(ctx context.Context, repoPath string, prID int) error {
	m.mu.Lock()
	m.MergePRCalls = append(m.MergePRCalls, mergePRCall{RepoPath: repoPath, PRID: prID})
	m.mu.Unlock()
	return m.MergePRErr
}
