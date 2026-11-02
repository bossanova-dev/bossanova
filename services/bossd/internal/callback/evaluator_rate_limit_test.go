package callback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// TestEvaluatePR_QuotaRefusalWarnsOncePerWindow pins the evaluator's log
// gate: two refused PR reads in one quota window write one Warn, the callback
// stays active and undelivered, and a refusal after the window warns again.
func TestEvaluatePR_QuotaRefusalWarnsOncePerWindow_Callbacks_AC221701(t *testing.T) {
	refused := fmt.Errorf("get PR: %w", vcs.ErrRateLimited)
	for _, tc := range []struct {
		name string
		msg  string
		prov *fakeProvider
	}{
		{"pr status", "callback evaluator: get PR status failed", &fakeProvider{statusErr: refused}},
		{"check results", "callback evaluator: get check results failed",
			&fakeProvider{status: prStatus(vcs.PRStateOpen), checksErr: refused}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			cb := mustCreate(t, store, db.CreateGitHubCallbackParams{
				TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
				Trigger: models.GitHubCallbackTriggerMerged, Message: "on merge",
			})
			clock := clocktest.New(time.Unix(1_800_000_000, 0))
			var logs bytes.Buffer
			ev := NewEvaluator(store, tc.prov, clock.Now, zerolog.New(&logs).Level(zerolog.DebugLevel))

			for i := 0; i < 2; i++ {
				if err := ev.EvaluatePR(context.Background(), "acme", "widgets", 7); !errors.Is(err, vcs.ErrRateLimited) {
					t.Fatalf("evaluation %d err = %v, want vcs.ErrRateLimited", i, err)
				}
			}
			if got := countLogLines(logs.String(), "warn", tc.msg); got != 1 {
				t.Fatalf("%q logged %d Warn lines for two refusals in one window, want 1:\n%s", tc.msg, got, logs.String())
			}
			if got := countLogLines(logs.String(), "debug", tc.msg); got != 1 {
				t.Fatalf("%q logged %d Debug lines for the repeat, want 1", tc.msg, got)
			}
			if got := getState(t, store, cb.ID); got != models.GitHubCallbackStateActive {
				t.Fatalf("callback state = %q after refused reads, want active", got)
			}

			clock.Advance(5 * time.Minute)
			_ = ev.EvaluatePR(context.Background(), "acme", "widgets", 7)
			if got := countLogLines(logs.String(), "warn", tc.msg); got != 2 {
				t.Fatalf("%q logged %d Warn lines after the window ended, want 2", tc.msg, got)
			}
		})
	}
}

func countLogLines(logs, level, msg string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"level":"`+level+`"`) && strings.Contains(line, `"message":"`+msg) {
			n++
		}
	}
	return n
}

// reserveObservingProvider records, per read, whether the evaluator marked its
// ctx reserve-tier.
type reserveObservingProvider struct {
	*fakeProvider
	mu       sync.Mutex
	unmarked []string
	reads    int
}

func (p *reserveObservingProvider) observe(ctx context.Context, read string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if !vcs.ReserveQuotaReads(ctx) {
		p.unmarked = append(p.unmarked, read)
	}
}

func (p *reserveObservingProvider) GetPRStatus(ctx context.Context, rp string, pr int) (*vcs.PRStatus, error) {
	p.observe(ctx, "status")
	return p.fakeProvider.GetPRStatus(ctx, rp, pr)
}

func (p *reserveObservingProvider) GetCheckResults(ctx context.Context, rp string, pr int) ([]vcs.CheckResult, error) {
	p.observe(ctx, "checks")
	return p.fakeProvider.GetCheckResults(ctx, rp, pr)
}

func (p *reserveObservingProvider) ListWorkflowRuns(ctx context.Context, rp, sha string) ([]vcs.WorkflowRun, error) {
	p.observe(ctx, "workflow runs")
	return p.fakeProvider.ListWorkflowRuns(ctx, rp, sha)
}

// TestEvaluatorReadsAreReserveTier_Callbacks_AC222201 pins that every read the
// evaluator makes, from the webhook path and from ChecksPassedReady alike,
// carries the reserve-tier mark, so it keeps reading between the floors.
func TestEvaluatorReadsAreReserveTier_Callbacks_AC222201(t *testing.T) {
	for name, read := range map[string]func(*Evaluator) error{
		"EvaluatePR": func(ev *Evaluator) error { return ev.EvaluatePR(context.Background(), "acme", "widgets", 7) },
		"ChecksPassedReady": func(ev *Evaluator) error {
			_, _, err := ev.ChecksPassedReady(context.Background(), "acme", "widgets", 7)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			mustCreate(t, store, db.CreateGitHubCallbackParams{
				TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
				Trigger: models.GitHubCallbackTriggerMerged, Message: "on merge",
			})
			prov := &reserveObservingProvider{fakeProvider: &fakeProvider{
				status: prStatus(vcs.PRStateOpen),
				checks: []vcs.CheckResult{completedCheck("build", vcs.CheckConclusionSuccess)},
			}}
			ev := NewEvaluator(store, prov, fixedNow(), zerolog.Nop())

			if err := read(ev); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if prov.reads != 3 {
				t.Fatalf("%s made %d reads, want 3 (status, checks, workflow runs)", name, prov.reads)
			}
			if len(prov.unmarked) > 0 {
				t.Fatalf("%s reads without the reserve-tier mark: %v", name, prov.unmarked)
			}
		})
	}
}
