package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/ghusage"
	"github.com/rs/zerolog"
)

type mergeReadUsage struct {
	mu   sync.Mutex
	rows []string
}

func (u *mergeReadUsage) Record(label, quota, identity string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rows = append(u.rows, label+"|"+quota+"|"+identity)
}
func (u *mergeReadUsage) RecordSkip(label, quota, identity, _ string) {
	u.Record(label, quota, identity)
}
func (u *mergeReadUsage) RecordNotModified(label, quota, identity string) {
	u.Record(label, quota, identity)
}

func operatorMergeProvider(t testing.TB) (*Provider, *mergeReadUsage, *[]string) {
	t.Helper()
	usage := &mergeReadUsage{}
	var fallback []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := ""
		switch r.URL.Path {
		case "/graphql":
			var request struct {
				Query     string `json:"query"`
				Variables struct {
					Number int `json:"number"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			switch {
			case strings.Contains(request.Query, "reviewDecision"):
				if request.Variables.Number != 7 {
					t.Errorf("PR number=%d", request.Variables.Number)
				}
				body = statusGraphQL(`{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","headRefOid":"sha","reviewDecision":"APPROVED","reviews":[]}`)
			case strings.Contains(request.Query, "statusCheckRollup"):
				body = checksGraphQL(`[{"name":"build","workflow":"ci","state":"SUCCESS"}]`)
			case strings.Contains(request.Query, "mergeCommit"):
				body = prGraphQL(`{"state":"MERGED","mergeCommit":{"oid":"merge-sha"}}`)
			case strings.Contains(request.Query, "reviewThreads"):
				body = prGraphQL(`{"reviewThreads":{"nodes":[],"pageInfo":{}}}`)
			}
		case "/repos/o/r/pulls/7":
			body = `{"mergeable":true,"mergeable_state":"clean","rebaseable":true}`
		case "/repos/o/r/pulls/7/reviews":
			body = `[{"id":1,"state":"COMMENTED","body":"","user":{"login":"robot[bot]"},"author_association":"NONE"}]`
		case "/repos/o/r/pulls/7/comments":
			body = `[]`
		case "/repos/o/r":
			body = `{"allow_merge_commit":true,"allow_squash_merge":true,"allow_rebase_merge":true}`
		}
		if body == "" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unhandled", http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	p := New(zerolog.Nop(), withAPIExecConfig(apiExecConfig{baseURL: server.URL + "/", transport: server.Client().Transport, userToken: func(context.Context) (string, error) { return "test-token", nil }, fallback: func(_ context.Context, args ...string) (string, error) {
		fallback = append(fallback, strings.Join(args, " "))
		return "", errors.New("unexpected subprocess")
	}}), WithUsageRecorder(usage))
	return p, usage, &fallback
}

func operatorMergeReads(ctx context.Context, p *Provider) error {
	status, err := p.GetPRStatus(ctx, "https://github.com/o/r", 7)
	if err != nil {
		return err
	}
	checks, err := p.GetCheckResults(ctx, "https://github.com/o/r", 7)
	if err != nil {
		return err
	}
	reviews, err := p.GetReviewComments(ctx, "https://github.com/o/r", 7)
	if err != nil {
		return err
	}
	if display := vcs.ComputeDisplayStatus(status, checks, reviews); display.Status != vcs.DisplayStatusApproved {
		return fmt.Errorf("green merge displayed as %v", display.Status)
	}
	if _, err = p.GetReviewObservation(ctx, "https://github.com/o/r", 7); err != nil {
		return err
	}
	strategies, err := p.GetAllowedMergeStrategies(ctx, "https://github.com/o/r")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(strategies, []string{"merge", "squash", "rebase"}) {
		return fmt.Errorf("strategies=%v", strategies)
	}
	sha, err := p.GetPRMergeCommit(ctx, "https://github.com/o/r", 7)
	if err != nil {
		return err
	}
	if sha != "merge-sha" {
		return fmt.Errorf("merge commit=%s", sha)
	}
	return nil
}

func TestOperatorMergeReadsMakeNoSubprocessCall(t *testing.T) {
	p, usage, fallback := operatorMergeProvider(t)
	ctx := ghusage.WithLabel(t.Context(), ghusage.LabelMerge)
	if err := operatorMergeReads(ctx, p); err != nil {
		t.Fatal(err)
	}
	if len(*fallback) > 0 {
		t.Fatalf("subprocess calls: %v", *fallback)
	}
	graphql := 0
	for _, row := range usage.rows {
		if !strings.HasPrefix(row, ghusage.LabelMerge+"|") || !strings.HasSuffix(row, "|user") {
			t.Fatalf("attribution changed: %s", row)
		}
		if strings.Contains(row, "|graphql|") {
			graphql++
		}
	}
	// Status, checks, review threads, and merge commit each issue one query.
	if graphql != 4 {
		t.Fatalf("GraphQL calls=%d want 4; usage=%v", graphql, usage.rows)
	}
	if len(usage.rows) != 8 {
		t.Fatalf("metered %d calls, want 8: %v", len(usage.rows), usage.rows)
	}
}

func BenchmarkOperatorMergeReadsWarm(b *testing.B) {
	p, _, fallback := operatorMergeProvider(b)
	ctx := ghusage.WithLabel(b.Context(), ghusage.LabelMerge)
	if err := operatorMergeReads(ctx, p); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if err := operatorMergeReads(ctx, p); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if len(*fallback) > 0 {
		b.Fatal(*fallback)
	}
}
