package prstate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/adapter/github"
	"github.com/recurser/bossd/internal/ghusage"
	"github.com/recurser/bossd/internal/prstate"
)

const (
	quotaRepo   = "https://github.com/o/r"
	quotaPR     = 7
	quotaHead   = "0123456789abcdef0123456789abcdef01234567"
	quotaToken  = "ghs_prstate-test-installation-token"
	quotaInstID = 42
)

// cannedGH answers the gh invocations a PR-state read makes with canned JSON:
// one open green PR whose head has one finished workflow run and no reviews.
// An invocation it has no answer for fails the test, so a new read the reader
// starts making is noticed here rather than silently answered.
func cannedGH(t *testing.T) func(ctx context.Context, args ...string) (string, error) {
	t.Helper()
	return func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(call, "pr view"):
			return fmt.Sprintf(`{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","isDraft":false,`+
				`"title":"t","body":"","headRefName":"feature","baseRefName":"main","headRefOid":%q,`+
				`"isCrossRepository":false,"reviewDecision":"","reviews":[]}`, quotaHead), nil
		case strings.HasPrefix(call, "pr checks"):
			return `[{"name":"build","state":"SUCCESS","workflow":"ci.yml","description":""}]`, nil
		case strings.HasPrefix(call, "api repos/o/r/actions/runs"):
			return fmt.Sprintf(`{"total_count":1,"workflow_runs":[{"name":"ci","event":"pull_request",`+
				`"status":"completed","conclusion":"success","head_sha":%q}]}`, quotaHead), nil
		case call == "api repos/o/r/pulls/7 -i":
			// The REST mergeability read an open, non-draft PR status adds.
			return `{"mergeable":true,"mergeable_state":"clean","rebaseable":true}`, nil
		case strings.HasPrefix(call, "api repos/o/r/pulls/7/"):
			return `[]`, nil
		case strings.HasPrefix(call, "api graphql"):
			// The open-PR snapshot a cached read tries first. With no open PR
			// in it, every read falls back to its own per-PR call.
			return `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}`, nil
		}
		t.Errorf("unexpected gh call %q", call)
		return "", errors.New("unexpected gh call")
	}
}

type usageCall struct{ label, quota, identity, outcome string }

// recordingUsage records every ledger write the provider's funnel makes.
type recordingUsage struct {
	mu    sync.Mutex
	calls []usageCall
}

func (u *recordingUsage) Record(label, quota, identity string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, usageCall{label: label, quota: quota, identity: identity})
}

func (u *recordingUsage) RecordSkip(label, quota, identity, outcome string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, usageCall{label: label, quota: quota, identity: identity, outcome: outcome})
}

func (u *recordingUsage) RecordNotModified(label, quota, identity string) {
	u.RecordSkip(label, quota, identity, ghusage.OutcomeNotModified)
}

func (u *recordingUsage) snapshot() []usageCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]usageCall(nil), u.calls...)
}

// installationTokens grants one installation token for every repo.
type installationTokens struct{}

func (installationTokens) Token(context.Context, string) (github.InstallationToken, bool) {
	return github.InstallationToken{
		Value:          quotaToken,
		InstallationID: quotaInstID,
		Permissions:    map[string]string{"pull_requests": "read", "checks": "read", "actions": "read"},
	}, true
}

func (installationTokens) Invalidate(string, string) {}

// TestPRStateReadsSpendInstallationQuota_GitHubUsage_AC230601 proves that the
// daemon-served PR-state read, run through a real GitHub provider, meters every
// call it sends against the repo's installation pool when an installation
// token source is attached, and against the user login when none is.
func TestPRStateReadsSpendInstallationQuota_GitHubUsage_AC230601(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		tokens   github.InstallationTokenSource
		identity string
	}{
		{name: "installation token source attached", tokens: installationTokens{}, identity: ghusage.InstallationIdentity(quotaInstID)},
		{name: "no token source", identity: ghusage.IdentityUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			usage := &recordingUsage{}
			p := github.New(zerolog.Nop(), github.WithRunGH(cannedGH(t)), github.WithUsageRecorder(usage))
			if tc.tokens != nil {
				p.SetInstallationTokens(tc.tokens)
			}
			reader := prstate.NewReader(p, nil)
			// A cached read (the default) and a fresh one, each reading every
			// part of the PR's state.
			for _, fresh := range []bool{false, true} {
				snap, err := reader.Read(context.Background(), quotaRepo, quotaPR, prstate.Options{
					Fresh: fresh, IncludeReviewComments: true, IncludeWorkflowRuns: true,
				})
				if err != nil {
					t.Fatalf("Read(fresh=%t): %v", fresh, err)
				}
				if snap.Status.HeadSHA != quotaHead || !snap.RunsRead {
					t.Fatalf("Read(fresh=%t) = %+v, want the canned PR's head and its runs", fresh, snap)
				}
			}

			calls := usage.snapshot()
			if len(calls) == 0 {
				t.Fatal("the provider metered no calls; the reads under test never reached GitHub")
			}
			for _, c := range calls {
				if c.label != ghusage.LabelPRState || c.identity != tc.identity || c.outcome != "" {
					t.Errorf("metered %+v, want a sent call labelled %q on identity %q", c, ghusage.LabelPRState, tc.identity)
				}
				if strings.Contains(c.identity, quotaToken) {
					t.Fatalf("a ledger record carries the token: %+v", c)
				}
			}
		})
	}
}
