package testharness

import (
	"context"
	"fmt"
	"sync"

	"github.com/recurser/bossalib/vcs"
)

var _ vcs.Provider = (*StubProvider)(nil)

// StubProvider is a thread-safe VCS provider for realtime webhook harness tests.
type StubProvider struct {
	mu sync.Mutex

	statuses map[int]*vcs.PRStatus
	checks   map[int][]vcs.CheckResult
	reviews  map[int][]vcs.ReviewComment
	counts   StubProviderCallCounts
}

// StubProviderCallCounts snapshots provider method invocation counts.
type StubProviderCallCounts struct {
	GetPRStatus       int
	GetCheckResults   int
	GetReviewComments int
}

func NewStubProvider() *StubProvider {
	mergeable := true
	return &StubProvider{
		statuses: map[int]*vcs.PRStatus{
			0: {State: vcs.PRStateOpen, Mergeable: &mergeable},
		},
		checks:  make(map[int][]vcs.CheckResult),
		reviews: make(map[int][]vcs.ReviewComment),
	}
}

func (p *StubProvider) SetPRStatus(prID int, status *vcs.PRStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if status == nil {
		delete(p.statuses, prID)
		return
	}
	p.statuses[prID] = clonePRStatus(status)
}

func (p *StubProvider) SetCheckResults(prID int, checks []vcs.CheckResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if checks == nil {
		delete(p.checks, prID)
		return
	}
	p.checks[prID] = append([]vcs.CheckResult(nil), checks...)
}

func (p *StubProvider) SetReviewComments(prID int, reviews []vcs.ReviewComment) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if reviews == nil {
		delete(p.reviews, prID)
		return
	}
	p.reviews[prID] = append([]vcs.ReviewComment(nil), reviews...)
}

func (p *StubProvider) CallCounts() StubProviderCallCounts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts
}

func (p *StubProvider) GetPRStatus(_ context.Context, _ string, prID int) (*vcs.PRStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts.GetPRStatus++
	if status, ok := p.statuses[prID]; ok {
		return clonePRStatus(status), nil
	}
	return clonePRStatus(p.statuses[0]), nil
}

func (p *StubProvider) GetCheckResults(_ context.Context, _ string, prID int) ([]vcs.CheckResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts.GetCheckResults++
	return append([]vcs.CheckResult(nil), p.checks[prID]...), nil
}

func (p *StubProvider) CreateDraftPR(context.Context, vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return nil, fmt.Errorf("stub provider: CreateDraftPR not implemented")
}

func (p *StubProvider) GetFailedCheckLogs(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("stub provider: GetFailedCheckLogs not implemented")
}

func (p *StubProvider) MarkReadyForReview(context.Context, string, int) error {
	return fmt.Errorf("stub provider: MarkReadyForReview not implemented")
}

func (p *StubProvider) GetReviewComments(_ context.Context, _ string, prID int) ([]vcs.ReviewComment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts.GetReviewComments++
	return append([]vcs.ReviewComment(nil), p.reviews[prID]...), nil
}

func (p *StubProvider) ListOpenPRs(context.Context, string) ([]vcs.PRSummary, error) {
	return nil, fmt.Errorf("stub provider: ListOpenPRs not implemented")
}

func (p *StubProvider) ListClosedPRs(context.Context, string) ([]vcs.PRSummary, error) {
	return nil, fmt.Errorf("stub provider: ListClosedPRs not implemented")
}

func (p *StubProvider) MergePR(context.Context, string, int, string) error {
	return fmt.Errorf("stub provider: MergePR not implemented")
}

func (p *StubProvider) UpdatePRTitle(context.Context, string, int, string) error {
	return fmt.Errorf("stub provider: UpdatePRTitle not implemented")
}

func (p *StubProvider) GetPRMergeCommit(context.Context, string, int) (string, error) {
	return "", fmt.Errorf("stub provider: GetPRMergeCommit not implemented")
}

func (p *StubProvider) GetAllowedMergeStrategies(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("stub provider: GetAllowedMergeStrategies not implemented")
}

func clonePRStatus(status *vcs.PRStatus) *vcs.PRStatus {
	if status == nil {
		return nil
	}
	cp := *status
	if status.Mergeable != nil {
		mergeable := *status.Mergeable
		cp.Mergeable = &mergeable
	}
	return &cp
}
