package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/rs/zerolog"
)

func TestGetPRStatus_RESTSupplementRefusal(t *testing.T) {
	for _, supplement := range []string{"mergeability", "review authors"} {
		for _, failure := range []string{"none", "quota", "not found", "trusted association"} {
			t.Run(supplement+"/"+failure, func(t *testing.T) {
				association := "NONE"
				if supplement == "mergeability" || failure == "trusted association" {
					association = "MEMBER"
				}
				reviewCalls := 0
				fakeGH := func(_ context.Context, args ...string) (string, error) {
					if isPRView(args) {
						return fmt.Sprintf(`{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":"REVIEW_REQUIRED","reviews":[{"id":"PRR_bot","state":"CHANGES_REQUESTED","authorAssociation":%q}]}`, association), nil
					}
					isReview := len(args) > 1 && strings.Contains(args[1], "/reviews")
					if isReview {
						reviewCalls++
					}
					if (supplement == "review authors") == isReview {
						switch failure {
						case "quota":
							return "", errors.New("HTTP 403: API rate limit exceeded for user ID 1")
						case "not found":
							return "", errors.New("HTTP 404: Not Found")
						}
					}
					if isReview {
						return `[{"node_id":"PRR_bot","user":{"login":"review-bot[bot]"},"author_association":"NONE","state":"CHANGES_REQUESTED"}]`, nil
					}
					return `{"mergeable":true,"mergeable_state":"clean","rebaseable":true}`, nil
				}
				status, err := New(zerolog.Nop(), WithRunGH(fakeGH)).GetPRStatus(context.Background(), "owner/repo", 42)
				if err != nil {
					t.Fatalf("GetPRStatus: %v", err)
				}
				if want := failure == "quota"; status.Degraded != want {
					t.Fatalf("Degraded = %v, want %v", status.Degraded, want)
				}
				if status.Mergeable == nil || !*status.Mergeable || status.MergeStateStatus != vcs.MergeStateStatusClean {
					t.Fatalf("mergeability fallback lost: %+v", status)
				}
				if supplement == "mergeability" && (failure == "quota" || failure == "not found") && status.Rebaseable != nil {
					t.Fatalf("fallback Rebaseable = %v", status.Rebaseable)
				}
				wantReview := vcs.ReviewStateChangesRequested
				if supplement == "review authors" && (failure == "quota" || failure == "not found") {
					wantReview = vcs.ReviewStateRequired
				}
				if status.LatestReviewState != wantReview {
					t.Fatalf("review = %v, want %v", status.LatestReviewState, wantReview)
				}
				if failure == "trusted association" && reviewCalls != 0 {
					t.Fatal("trusted association unexpectedly read REST authors")
				}
			})
		}
	}
}

func TestGetPRStatus_RESTSupplementCoreFloor(t *testing.T) {
	clock := newFakeClock()
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if isRateLimitProbe(args) {
			reset := clock.Now().Add(time.Hour).Unix()
			return fmt.Sprintf(`{"resources":{"graphql":{"limit":5000,"remaining":4000,"reset":%d},"core":{"limit":5000,"remaining":100,"reset":%d}}}`, reset, reset), nil
		}
		if isPRView(args) {
			return `{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN"}`, nil
		}
		return "", fmt.Errorf("unexpected gh call %v", args)
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now), WithBackgroundQuotaFloors(testStandardFloor, testReserveFloor))
	status, err := p.GetPRStatus(vcs.WithCachedReads(context.Background()), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetPRStatus: %v", err)
	}
	if !status.Degraded || status.Mergeable == nil || !*status.Mergeable || status.Rebaseable != nil {
		t.Fatalf("floor-refused status = %+v", status)
	}
	if gh.count(isRESTCall) != 0 {
		t.Fatal("core floor contacted GitHub")
	}
}
