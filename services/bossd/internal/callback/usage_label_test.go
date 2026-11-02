package callback

import (
	"context"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/ghusage"
)

// labelProvider is fakeProvider recording the usage label of every PR status
// read.
type labelProvider struct {
	*fakeProvider
	mu     sync.Mutex
	labels []string
}

func (p *labelProvider) GetPRStatus(ctx context.Context, rp string, n int) (*vcs.PRStatus, error) {
	p.mu.Lock()
	p.labels = append(p.labels, ghusage.LabelFrom(ctx))
	p.mu.Unlock()
	return p.fakeProvider.GetPRStatus(ctx, rp, n)
}

func (p *labelProvider) take(t *testing.T) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	got := p.labels
	p.labels = nil
	if len(got) == 0 {
		t.Fatal("no PR status read reached the provider")
	}
	return got
}

func onlyLabel(t *testing.T, path string, got []string, want string) {
	t.Helper()
	for i, label := range got {
		if label != want {
			t.Fatalf("%s: read %d label = %q, want %q", path, i, label, want)
		}
	}
}

// TestEvaluatorMetersItsReadsAsTheCallbackEvaluator pins the evaluator's
// labels: both its webhook path (EvaluatePR) and its periodic pass
// (ReconcileAll) read GitHub as daemon:callbacks from an unlabelled context,
// while the shared ChecksPassedReady read sets no label of its own, so the
// verify-event trigger's reads stay attributed to the trigger.
func TestEvaluatorMetersItsReadsAsTheCallbackEvaluator(t *testing.T) {
	store := newStore(t)
	mustCreate(t, store, db.CreateGitHubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 1,
		Trigger: models.GitHubCallbackTriggerMerged, Message: "m",
	})
	prov := &labelProvider{fakeProvider: &fakeProvider{status: prStatus(vcs.PRStateOpen)}}
	ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())

	if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 1); err != nil {
		t.Fatalf("EvaluatePR: %v", err)
	}
	onlyLabel(t, "EvaluatePR", prov.take(t), ghusage.LabelCallbacks)

	if err := ev.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	onlyLabel(t, "ReconcileAll", prov.take(t), ghusage.LabelCallbacks)

	verifyCtx := ghusage.WithLabel(context.Background(), ghusage.LabelVerifyEvent)
	if _, _, err := ev.ChecksPassedReady(verifyCtx, "acme", "widgets", 1); err != nil {
		t.Fatalf("ChecksPassedReady: %v", err)
	}
	onlyLabel(t, "ChecksPassedReady", prov.take(t), ghusage.LabelVerifyEvent)
}
