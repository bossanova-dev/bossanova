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

	statuses     map[int]*vcs.PRStatus
	checks       map[int][]vcs.CheckResult
	reviews      map[int][]vcs.ReviewComment
	blockingBots map[int]map[string]bool
	counts       StubProviderCallCounts
}

// StubProviderCallCounts snapshots provider method invocation counts.
type StubProviderCallCounts struct {
	GetPRStatus           int
	GetCheckResults       int
	GetReviewComments     int
	BlockingThreadAuthors int
}

func NewStubProvider() *StubProvider {
	mergeable := true
	return &StubProvider{
		statuses: map[int]*vcs.PRStatus{
			0: {State: vcs.PRStateOpen, Mergeable: &mergeable},
		},
		checks:       make(map[int][]vcs.CheckResult),
		reviews:      make(map[int][]vcs.ReviewComment),
		blockingBots: make(map[int]map[string]bool),
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

// SetBlockingThreadAuthors declares which bot logins still own a review thread
// that blocks on prID — the freshness fixture the realtime promotion path reads
// through upstream.ReviewThreadFreshnessProvider (BOS-669).
//
// The default is deliberately EMPTY, mirroring production's fail-closed posture:
// a harness test that expects a bot COMMENTED review to be promoted must say so
// explicitly. Defaulting to "everything blocks" would make the stub fail open
// and hide exactly the regression the freshness gate exists to catch.
func (p *StubProvider) SetBlockingThreadAuthors(prID int, logins []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if logins == nil {
		delete(p.blockingBots, prID)
		return
	}
	blocking := make(map[string]bool, len(logins))
	for _, login := range logins {
		blocking[login] = true
	}
	p.blockingBots[prID] = blocking
}

func (p *StubProvider) BlockingThreadAuthors(_ context.Context, _ string, prID int, botUsers map[string]bool) (map[string]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts.BlockingThreadAuthors++
	blocking := make(map[string]bool)
	for login := range botUsers {
		if p.blockingBots[prID][login] {
			blocking[login] = true
		}
	}
	return blocking, nil
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

func (p *StubProvider) SearchPRsByTitleTag(context.Context, string, string) ([]vcs.PRSummary, error) {
	return nil, fmt.Errorf("stub provider: SearchPRsByTitleTag not implemented")
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
