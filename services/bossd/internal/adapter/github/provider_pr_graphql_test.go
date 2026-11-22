package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/rs/zerolog"
)

func TestPRStatusGraphQL(t *testing.T) {
	t.Run("fixture parity", func(t *testing.T) {
		files, err := filepath.Glob("testdata/pr_status_*.json")
		if err != nil || len(files) == 0 {
			t.Fatal(files, err)
		}
		for _, file := range files {
			t.Run(file, func(t *testing.T) {
				body, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var want prStatusFields
				if err := json.Unmarshal(body, &want); err != nil {
					t.Fatal(err)
				}
				if want.Reviews == nil {
					want.Reviews = []prReview{}
				}
				p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) { return statusGraphQL(string(body)), nil }))
				raw, err := p.readPRStatus(t.Context(), "o/r", 7)
				if err != nil || !reflect.DeepEqual(raw, want) {
					t.Fatalf("got %+v, err %v; want %+v", raw, err, want)
				}
			})
		}
	})
	t.Run("reviews in order", func(t *testing.T) {
		p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
			if slices.Contains(args, "cursor=next") {
				return prGraphQL(`{"state":"OPEN","reviews":{"nodes":[{"state":"APPROVED","authorAssociation":"MEMBER"}],"pageInfo":{}}}`), nil
			}
			return prGraphQL(`{"state":"OPEN","reviews":{"nodes":[{"state":"CHANGES_REQUESTED","authorAssociation":"MEMBER"}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}`), nil
		}))
		raw, err := p.readPRStatus(t.Context(), "o/r", 7)
		if err != nil || len(raw.Reviews) != 2 || raw.Reviews[0].State != "CHANGES_REQUESTED" || raw.Reviews[1].State != "APPROVED" {
			t.Fatalf("%+v %v", raw, err)
		}
	})
	for name, body := range map[string]string{"missing cursor": prGraphQL(`{"reviews":{"pageInfo":{"hasNextPage":true}}}`), "null PR": prGraphQL("null"), "null repo": `{"data":{"repository":null}}`, "graphql errors": `{"errors":[{"message":"denied"}]}`, "parse": "{"} {
		t.Run(name, func(t *testing.T) {
			p := New(zerolog.Nop(), WithRunGH(func(context.Context, ...string) (string, error) { return body, nil }))
			if _, err := p.readPRStatus(t.Context(), "o/r", 7); err == nil {
				t.Fatal("accepted invalid page")
			}
		})
	}
	t.Run("page cap", func(t *testing.T) {
		calls := 0
		p := New(zerolog.Nop(), WithRunGH(func(context.Context, ...string) (string, error) {
			calls++
			return prGraphQL(`{"reviews":{"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}`), nil
		}))
		if _, err := p.readPRStatus(t.Context(), "o/r", 7); err == nil || calls != openPRSnapshotMaxPages {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
}

func TestCheckSetGraphQL(t *testing.T) {
	for name, node := range map[string]string{"no commits": `{"commits":{"nodes":[]}}`, "null rollup": `{"commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}`, "zero contexts": `{"commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[]}}}}]}}`} {
		t.Run(name, func(t *testing.T) {
			p := New(zerolog.Nop(), WithRunGH(func(context.Context, ...string) (string, error) { return prGraphQL(node), nil }))
			set, err := p.fetchCheckSet(t.Context(), "o/r", 7)
			if err != nil || set.Checks == nil || len(set.Checks) != 0 {
				t.Fatalf("%+v %v", set, err)
			}
		})
	}
	t.Run("paging and combined states", func(t *testing.T) {
		p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
			if slices.Contains(args, "cursor=next") {
				return checksPage(`[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-01-01T00:00:00Z"},{"__typename":"StatusContext","context":"boss/build","state":"SUCCESS"},{"__typename":"StatusContext","context":"other","state":"NEW_STATE"}]`, false, ""), nil
			}
			return checksPage(`[{"__typename":"CheckRun","name":"test","status":"IN_PROGRESS","startedAt":"2026-01-02T00:00:00Z"}]`, true, "next"), nil
		}))
		set, err := p.fetchCheckSet(t.Context(), "o/r", 7)
		if err != nil || len(set.Checks) != 2 || set.Checks[0].Status != vcs.CheckStatusInProgress || !set.Checks[1].Unclassified || !set.HasBuildReceipt {
			t.Fatalf("%+v %v", set, err)
		}
	})
}

func TestPRMergeCommitGraphQL(t *testing.T) {
	for _, tc := range []struct{ node, sha string }{{`{"state":"MERGED","mergeCommit":{"oid":"sha"}}`, "sha"}, {`{"state":"OPEN"}`, ""}, {`{"state":"MERGED","mergeCommit":null}`, ""}} {
		p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
			if kind, _, ok := perPRQuery(args); !ok || kind != "merge" {
				return "", fmt.Errorf("unexpected args %v", args)
			}
			return prGraphQL(tc.node), nil
		}))
		sha, err := p.GetPRMergeCommit(t.Context(), "o/r", 7)
		if sha != tc.sha || (tc.sha == "" && !errors.Is(err, vcs.ErrPRNotMerged)) || (tc.sha != "" && err != nil) {
			t.Fatalf("sha=%s err=%v", sha, err)
		}
	}
}

func TestPRStatusGraphQLLaterPageRefusal(t *testing.T) {
	for _, kind := range []string{"status", "checks"} {
		t.Run(kind, func(t *testing.T) {
			pages := 0
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				if k, _, ok := perPRQuery(args); ok && k == kind {
					pages++
					if pages == 2 {
						return "", &rateLimitedError{quota: quotaGraphQL}
					}
					if kind == "checks" {
						return checksPage(`[{"__typename":"StatusContext","context":"old","state":"FAILURE"}]`, true, "next"), nil
					}
					return prGraphQL(`{"state":"OPEN","reviews":{"pageInfo":{"hasNextPage":true,"endCursor":"next"},"nodes":[]}}`), nil
				}
				if len(args) > 1 && args[1] == "rate_limit" {
					return `{}`, nil
				}
				if len(args) > 1 && strings.Contains(args[1], "/pulls/7/reviews") {
					return `[]`, nil
				}
				if len(args) > 1 && args[1] == "repos/o/r/pulls/7" {
					return `{"state":"closed","merged":true,"head":{"sha":"sha"}}`, nil
				}
				if len(args) > 1 && strings.Contains(args[1], "check-runs") {
					return `{"check_runs":[]}`, nil
				}
				if len(args) > 1 && strings.Contains(args[1], "/status?") {
					return `{"statuses":[]}`, nil
				}
				if len(args) > 1 && strings.Contains(args[1], "/actions/runs?") {
					return `{"workflow_runs":[]}`, nil
				}
				return "", fmt.Errorf("unexpected args %v", args)
			}))
			if kind == "status" {
				status, err := p.fetchPRStatus(t.Context(), "o/r", 7)
				if err != nil || !status.Degraded || status.State != vcs.PRStateMerged {
					t.Fatalf("%+v %v", status, err)
				}
			} else {
				set, err := p.fetchCheckSet(t.Context(), "o/r", 7)
				if err != nil || !set.Degraded || len(set.Checks) != 0 {
					t.Fatalf("%+v %v", set, err)
				}
			}
			if pages != 2 {
				t.Fatalf("got %d pages", pages)
			}
		})
	}
}
