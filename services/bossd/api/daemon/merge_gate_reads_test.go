package server

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
)

// rendezvousProvider is a mergeGateProvider whose gate reads each wait until
// every one of them has started, so a gate that issued them one after another
// would never finish: the first read would wait for a second that never comes.
type rendezvousProvider struct {
	*mergeGateProvider
	arrived chan string
	release chan struct{}
}

func (p *rendezvousProvider) wait(read string) {
	p.arrived <- read
	<-p.release
}

func (p *rendezvousProvider) GetPRStatus(ctx context.Context, o string, pr int) (*vcs.PRStatus, error) {
	p.wait("PR status")
	return p.mergeGateProvider.GetPRStatus(ctx, o, pr)
}

func (p *rendezvousProvider) GetCheckResults(ctx context.Context, o string, pr int) ([]vcs.CheckResult, error) {
	p.wait("check results")
	return p.mergeGateProvider.GetCheckResults(ctx, o, pr)
}

func (p *rendezvousProvider) GetReviewComments(ctx context.Context, o string, pr int) ([]vcs.ReviewComment, error) {
	p.wait("review comments")
	return p.mergeGateProvider.GetReviewComments(ctx, o, pr)
}

func (p *rendezvousProvider) GetReviewObservation(ctx context.Context, o string, pr int) (vcs.ReviewObservation, error) {
	p.wait("review observation")
	return p.mergeGateProvider.GetReviewObservation(ctx, o, pr)
}

// The live gate's reads are each a gh subprocess, so they run at once: the
// gate takes one read's time, not the sum of four.
func TestLiveMergeBlockIssuesItsReadsAtOnce(t *testing.T) {
	prov := &rendezvousProvider{
		mergeGateProvider: &mergeGateProvider{prStatus: openCleanPRStatus(), checks: livePassingChecks(), observation: vcs.ReviewObservation{Bot: 1}},
		arrived:           make(chan string),
		release:           make(chan struct{}),
	}
	srv := mergeGateServer(t, prov.mergeGateProvider, vcs.DisplayStatusPassing)
	srv.provider = prov

	type gate struct {
		mb      *vcs.MergeBlockReason
		verdict mergeVerdict
		err     error
	}
	got := make(chan gate, 1)
	go func() {
		mb, verdict, err := srv.liveMergeBlock(context.Background(), "https://github.com/acme/repo", 42, "", mergeSessionBlocks)
		got <- gate{mb, verdict, err}
	}()

	inFlight := map[string]bool{}
	deadline := testDeadline(t)
	for len(inFlight) < 4 {
		select {
		case read := <-prov.arrived:
			inFlight[read] = true
		case <-deadline:
			t.Fatalf("reads in flight together = %v, want all four before any returns", inFlight)
		}
	}
	close(prov.release)
	g := <-got
	if g.err != nil || g.mb != nil || !g.verdict.verified {
		t.Fatalf("gate = %+v, want a passed verdict once every read returned", g)
	}
}
