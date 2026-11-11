package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/ghusage"
)

func TestGraphQLRefusalFallsBackToREST_Callbacks_AC245501(t *testing.T) {
	clock := newFakeClock()
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			return rateLimitBody(clock, 0, time.Hour), nil
		case isPRView(args):
			return "", errors.New(graphQLExhausted)
		case args[0] == "api" && args[1] == "repos/o/r/pulls/7":
			return `{"state":"open","head":{"ref":"h","sha":"abc","repo":{"full_name":"o/r"}},"base":{"ref":"main","repo":{"full_name":"o/r"}},"mergeable":true,"mergeable_state":"clean","rebaseable":true}`, nil
		case args[0] == "api" && strings.Contains(args[1], "/reviews"):
			return `[{"node_id":"review","state":"CHANGES_REQUESTED","user":{"login":"robot[bot]"},"author_association":"NONE"}]`, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	for range 2 {
		status, err := p.GetPRStatus(context.Background(), "o/r", 7)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Degraded {
			t.Fatal("REST fallback was not marked degraded")
		}
		if status.HeadSHA != "abc" || status.State != vcs.PRStateOpen || status.Mergeable == nil || !*status.Mergeable || status.Rebaseable == nil || !*status.Rebaseable || status.LatestReviewState != vcs.ReviewStateChangesRequested || status.ReviewDecisionState != vcs.ReviewStateUnspecified {
			t.Fatalf("REST status = %+v", status)
		}
	}
	if gh.count(isPRView) != 1 {
		t.Fatal("open GraphQL breaker did not suppress the second primary read")
	}
}

func TestCheckSetRESTPreservesIdentityAndReceipts_Callbacks_AC245501(t *testing.T) {
	clock := newFakeClock()
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		switch {
		case isRateLimitProbe(args):
			return rateLimitBody(clock, 0, time.Hour), nil
		case args[0] == "pr":
			return "", errors.New(graphQLExhausted)
		case args[1] == "repos/o/r/pulls/7":
			return `{"head":{"sha":"abc"}}`, nil
		case strings.Contains(args[1], "/check-runs"):
			return `{"check_runs":[{"name":"test","status":"completed","conclusion":"success","check_suite":{"id":12},"output":{"title":"passed"}}]}{"check_runs":[{"name":"external","status":"in_progress"},{"name":"queued","status":"requested"}]}`, nil
		case strings.Contains(args[1], "/status?"):
			return `{"statuses":[{"context":"boss/build","state":"success"},{"context":"boss/verify","state":"pending","description":"running"}]}`, nil
		case strings.Contains(args[1], "/actions/runs?"):
			return `{"total_count":1,"workflow_runs":[{"name":"CI","check_suite_id":12}]}`, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	set, err := p.GetCheckSet(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Degraded || !set.HasBuildReceipt || !set.BuildReceiptSeen || len(set.Checks) != 4 {
		t.Fatalf("set = %+v", set)
	}
	if c := set.Checks[0]; c.ID != "CI/test" || c.Status != vcs.CheckStatusCompleted || c.Conclusion == nil || *c.Conclusion != vcs.CheckConclusionSuccess {
		t.Fatalf("joined check = %+v", c)
	}
	if c := set.Checks[1]; c.ID != "/external" || c.Status != vcs.CheckStatusInProgress {
		t.Fatalf("external = %+v", c)
	}
	if c := set.Checks[2]; c.Status != vcs.CheckStatusQueued || c.Unclassified {
		t.Fatalf("requested = %+v", c)
	}
	if c := set.Checks[3]; c.Name != "boss/verify" || c.Description != "running" {
		t.Fatalf("verify = %+v", c)
	}
}

func TestQuotaFallbackFailureAndNonQuotaErrors_Callbacks_AC245502(t *testing.T) {
	for _, tc := range []struct {
		name      string
		primary   error
		restCalls int
		limited   bool
	}{
		{"both refused", &rateLimitedError{quota: quotaGraphQL}, 1, true},
		{"not found", errors.New("HTTP 404"), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restCalls := 0
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				if args[0] == "pr" {
					return "", tc.primary
				}
				if !isRateLimitProbe(args) {
					restCalls++
				}
				return "", &rateLimitedError{quota: quotaCore}
			}))
			_, err := p.GetPRStatus(context.Background(), "o/r", 7)
			if err == nil || vcs.IsRateLimited(err) != tc.limited || restCalls != tc.restCalls {
				t.Fatalf("err=%v calls=%d", err, restCalls)
			}
		})
	}
}

func TestBackgroundSnapshotFallsThroughToREST_Callbacks_AC245501(t *testing.T) {
	clock := newFakeClock()
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if isRateLimitProbe(args) {
			return fmt.Sprintf(`{"resources":{"graphql":{"limit":5000,"remaining":100,"reset":%d},"core":{"limit":5000,"remaining":4000,"reset":%d}}}`, clock.Now().Add(time.Hour).Unix(), clock.Now().Add(time.Hour).Unix()), nil
		}
		if args[0] == "api" && args[1] == "repos/o/r/pulls/7" {
			return `{"state":"open","draft":true,"head":{"sha":"abc"}}`, nil
		}
		if args[0] == "api" && strings.Contains(args[1], "/reviews") {
			return `[]`, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithBackgroundQuotaFloors(testStandardFloor, testReserveFloor))
	status, err := p.GetPRStatus(vcs.WithCachedReads(context.Background()), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Degraded || status.HeadSHA != "abc" || gh.count(isPRView) != 0 {
		t.Fatalf("status = %+v", status)
	}
}

func TestFallbackObeysCoreFloorAndUsage_Callbacks_AC245502(t *testing.T) {
	for _, coreRemaining := range []int{100, 4000} {
		t.Run(fmt.Sprint(coreRemaining), func(t *testing.T) {
			clock := newFakeClock()
			ledger := newTestLedger(t, clock)
			gh := &scriptedGH{reply: func(args []string) (string, error) {
				if isRateLimitProbe(args) {
					return fmt.Sprintf(`{"resources":{"graphql":{"limit":5000,"remaining":100,"reset":%d},"core":{"limit":5000,"remaining":%d,"reset":%d}}}`, clock.Now().Add(time.Hour).Unix(), coreRemaining, clock.Now().Add(time.Hour).Unix()), nil
				}
				if args[0] == "api" && args[1] == "repos/o/r/pulls/7" {
					return `{"state":"open","draft":true,"head":{"sha":"abc"}}`, nil
				}
				if args[0] == "api" && strings.Contains(args[1], "/reviews") {
					return `[]`, nil
				}
				return "", fmt.Errorf("unexpected call %v", args)
			}}
			p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithUsageRecorder(ledger), WithBackgroundQuotaFloors(testStandardFloor, testReserveFloor))
			ctx := ghusage.WithLabel(vcs.WithCachedReads(context.Background()), ghusage.LabelPRPoller)
			_, err := p.GetPRStatus(ctx, "o/r", 7)
			if vcs.IsRateLimited(err) != (coreRemaining == 100) {
				t.Fatalf("err=%v", err)
			}
			counts := readCounts(t, ledger, clock)
			foundCore := false
			for _, c := range counts {
				if c.Label != ghusage.LabelPRPoller {
					t.Fatalf("usage lost caller label: %+v", c)
				}
				if c.Quota == ghusage.QuotaCore {
					foundCore = true
					if coreRemaining == 100 && (c.FloorSkips != 1 || c.Calls != 0) {
						t.Fatalf("core floor usage=%+v", c)
					}
					if coreRemaining == 4000 && c.Calls != 2 {
						t.Fatalf("core read usage=%+v", c)
					}
				}
			}
			if !foundCore {
				t.Fatal("fallback did not meter core quota")
			}
			if coreRemaining == 100 && gh.count(isRESTCall) != 0 {
				t.Fatal("core floor contacted GitHub")
			}
		})
	}
}

func TestRESTStatusStateMapping(t *testing.T) {
	for _, tc := range []struct {
		state  string
		merged bool
		want   vcs.PRState
	}{
		{"open", false, vcs.PRStateOpen}, {"closed", false, vcs.PRStateClosed}, {"closed", true, vcs.PRStateMerged},
	} {
		t.Run(fmt.Sprintf("%s/%v", tc.state, tc.merged), func(t *testing.T) {
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				if args[0] == "pr" {
					return "", &rateLimitedError{quota: quotaGraphQL}
				}
				if isRateLimitProbe(args) {
					return `{}`, nil
				}
				if strings.Contains(args[1], "/reviews") {
					return `[]`, nil
				}
				return fmt.Sprintf(`{"state":%q,"merged":%v,"head":{"ref":"fork","sha":"abc"},"base":{"ref":"main","repo":{"full_name":"o/r"}},"mergeable":null}`, tc.state, tc.merged), nil
			}))
			status, err := p.GetPRStatus(context.Background(), "o/r", 7)
			if err != nil {
				t.Fatal(err)
			}
			if status.State != tc.want || status.Mergeable != nil || !status.IsCrossRepository || !status.Degraded {
				t.Fatalf("status=%+v", status)
			}
		})
	}
}

func TestRESTCheckWorkflowFailureIsNotRateLimited(t *testing.T) {
	p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
		if args[0] == "pr" {
			return "", &rateLimitedError{quota: quotaGraphQL}
		}
		if isRateLimitProbe(args) {
			return `{}`, nil
		}
		if strings.Contains(args[1], "/actions/runs?") {
			return "", errors.New("workflow list unavailable")
		}
		if strings.Contains(args[1], "/check-runs?") {
			return `{"check_runs":[]}`, nil
		}
		if strings.Contains(args[1], "/status?") {
			return `{"statuses":[]}`, nil
		}
		return `{"head":{"sha":"abc"}}`, nil
	}))
	_, err := p.GetCheckSet(context.Background(), "o/r", 7)
	if err == nil || vcs.IsRateLimited(err) || !strings.Contains(err.Error(), "workflow list unavailable") {
		t.Fatalf("err=%v", err)
	}
}

func TestRESTUnknownReviewDecisionCannotDisplaySatisfied(t *testing.T) {
	for _, reviews := range []string{`[]`, `[{"state":"APPROVED","user":{"login":"maintainer"},"author_association":"MEMBER"}]`} {
		t.Run(reviews, func(t *testing.T) {
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				if args[0] == "pr" {
					return "", &rateLimitedError{quota: quotaGraphQL}
				}
				if isRateLimitProbe(args) {
					return `{}`, nil
				}
				if strings.Contains(args[1], "/reviews") {
					return reviews, nil
				}
				return `{"state":"open","head":{"sha":"abc"},"mergeable":true,"mergeable_state":"blocked","rebaseable":false}`, nil
			}))
			status, err := p.GetPRStatus(context.Background(), "o/r", 7)
			if err != nil {
				t.Fatal(err)
			}
			conclusion := vcs.CheckConclusionSuccess
			checks := []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &conclusion}}
			if display := vcs.ComputeDisplayStatus(status, checks, nil); display.Status != vcs.DisplayStatusChecking {
				t.Fatalf("unknown REST review decision displayed as %v, want Checking", display.Status)
			}
		})
	}
}

func TestRESTStatusFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		err     error
		limited bool
	}{
		{"outage", "", errors.New("REST unavailable"), false},
		{"forbidden", "", errors.New("HTTP 403 forbidden"), false},
		{"malformed", "invalid JSON", nil, false},
		{"quota", "", &rateLimitedError{quota: quotaCore}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				if args[0] == "pr" {
					return "", &rateLimitedError{quota: quotaGraphQL}
				}
				if isRateLimitProbe(args) {
					return `{}`, nil
				}
				return tc.reply, tc.err
			}))
			_, err := p.GetPRStatus(context.Background(), "o/r", 7)
			if err == nil || vcs.IsRateLimited(err) != tc.limited {
				t.Fatalf("err=%v rateLimited=%v, want %v", err, vcs.IsRateLimited(err), tc.limited)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("REST cause lost: %v", err)
			}
		})
	}
}
