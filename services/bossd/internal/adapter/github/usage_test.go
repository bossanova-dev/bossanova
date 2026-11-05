package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/kit/vcs"

	"github.com/recurser/bossd/internal/ghusage"
)

func newTestLedger(t *testing.T, clock *clocktest.Clock) *ghusage.Ledger {
	t.Helper()
	return ghusage.New(t.TempDir(), zerolog.Nop(), ghusage.WithNow(clock.Now))
}

func readCounts(t *testing.T, ledger *ghusage.Ledger, clock *clocktest.Clock) []ghusage.Count {
	t.Helper()
	usage, err := ledger.Read(clock.Now(), time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return usage.Counts
}

// exhaustedGraphQLGH refuses every `pr view` for a spent GraphQL quota and
// answers the rate_limit probe with no GraphQL remaining.
func exhaustedGraphQLGH(clock *clocktest.Clock) *scriptedGH {
	return &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			return rateLimitBody(clock, 0, 20*time.Minute), nil
		case isPRView(args):
			return "", errors.New(graphQLExhausted)
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
}

// TestBreakerRefusalRecordsOneBreakerSkip_GitHubUsage_AC222301: a call the
// open breaker refuses appends exactly one breaker record with the call's
// quota and the context's label, and never reaches gh.
func TestBreakerRefusalRecordsOneBreakerSkip_GitHubUsage_AC222301(t *testing.T) {
	clock := newFakeClock()
	gh := exhaustedGraphQLGH(clock)
	ledger := newTestLedger(t, clock)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithUsageRecorder(ledger))

	// The first call is sent, refused by GitHub, and opens the breaker.
	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("first call err = %v, want vcs.ErrRateLimited", err)
	}
	sent := gh.count(isPRView)
	pollerCtx := ghusage.WithLabel(context.Background(), ghusage.LabelPRPoller)
	if _, err := p.GetPRStatus(pollerCtx, "o/r", 8); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("refused call err = %v, want vcs.ErrRateLimited", err)
	}
	if got := gh.count(isPRView); got != sent {
		t.Fatalf("pr view calls = %d after a breaker refusal, want %d (no invocation)", got, sent)
	}

	want := []ghusage.Count{
		{Label: ghusage.LabelUnattributed, Quota: ghusage.QuotaGraphQL, Identity: ghusage.IdentityUser, Calls: 1},
		{Label: ghusage.LabelPRPoller, Quota: ghusage.QuotaGraphQL, Identity: ghusage.IdentityUser, BreakerSkips: 1},
	}
	if got := readCounts(t, ledger, clock); !reflect.DeepEqual(got, want) {
		t.Fatalf("ledger = %+v, want %+v", got, want)
	}
}

// TestFloorRefusalRecordsOneFloorSkip_GitHubUsage_AC222301: a background read
// the quota floor refuses appends exactly one floor record and no breaker
// record, and the floor's own rate_limit probe appends nothing.
func TestFloorRefusalRecordsOneFloorSkip_GitHubUsage_AC222301(t *testing.T) {
	clock := newFakeClock()
	gh := newFloorGH(graphQLAt(clock, 1500, 20*time.Minute))
	ledger := newTestLedger(t, clock)
	p := newFloorProvider(gh, clock, WithUsageRecorder(ledger))

	ctx := ghusage.WithLabel(vcs.WithCachedReads(context.Background()), ghusage.LabelDisplayPoller)
	if _, err := p.GetPRStatus(ctx, "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("background read below the floor err = %v, want vcs.ErrRateLimited", err)
	}
	if gh.count(isRateLimitProbe) == 0 {
		t.Fatal("the floor never probed the quota; the refusal under test did not happen")
	}

	want := []ghusage.Count{{Label: ghusage.LabelDisplayPoller, Quota: ghusage.QuotaGraphQL, Identity: ghusage.IdentityUser, FloorSkips: 1}}
	if got := readCounts(t, ledger, clock); !reflect.DeepEqual(got, want) {
		t.Fatalf("ledger = %+v, want %+v", got, want)
	}
}

// TestSentRefusalRecordsOneCallAndNoSkip_GitHubUsage_AC222301: a call GitHub
// itself refuses for a spent quota was sent, so it appends one call record and
// no skip; the post-call breaker check, the reset probe and the QuotaStatus
// probe append nothing.
func TestSentRefusalRecordsOneCallAndNoSkip_GitHubUsage_AC222301(t *testing.T) {
	clock := newFakeClock()
	gh := exhaustedGraphQLGH(clock)
	ledger := newTestLedger(t, clock)
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithUsageRecorder(ledger))

	ctx := ghusage.WithLabel(context.Background(), ghusage.LabelCallbacks)
	if _, err := p.GetPRStatus(ctx, "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("refused call err = %v, want vcs.ErrRateLimited", err)
	}
	clock.Advance(2 * quotaReadingTTL) // expire the reading so QuotaStatus probes again
	if _, err := p.QuotaStatus(ctx); err != nil {
		t.Fatalf("QuotaStatus: %v", err)
	}
	if got := gh.count(isRateLimitProbe); got < 2 {
		t.Fatalf("rate_limit probes = %d, want the reset probe and the QuotaStatus probe", got)
	}

	want := []ghusage.Count{{Label: ghusage.LabelCallbacks, Quota: ghusage.QuotaGraphQL, Identity: ghusage.IdentityUser, Calls: 1}}
	if got := readCounts(t, ledger, clock); !reflect.DeepEqual(got, want) {
		t.Fatalf("ledger = %+v, want %+v", got, want)
	}
}

// TestSkipOutcomeClassifiesTheRefusal pins the floor/breaker split on the
// refusal error itself.
func TestSkipOutcomeClassifiesTheRefusal(t *testing.T) {
	floor := &rateLimitedError{quota: quotaCore, belowFloor: &quotaShortfall{remaining: 1, limit: 10, floor: 0.35}}
	if got := skipOutcome(floor); got != ghusage.OutcomeFloor {
		t.Fatalf("floor refusal outcome = %q, want %q", got, ghusage.OutcomeFloor)
	}
	if got := skipOutcome(&rateLimitedError{quota: quotaCore}); got != ghusage.OutcomeBreaker {
		t.Fatalf("breaker refusal outcome = %q, want %q", got, ghusage.OutcomeBreaker)
	}
}
