package github

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
)

const (
	testOrigin     = "https://github.com/o/r"
	testNWO        = "o/r"
	testInstallTok = "ghs_test-installation-token-do-not-print"
	testSHA        = "0123456789abcdef0123456789abcdef01234567"
)

// fakeTokenSource answers Token from its fields and records Invalidate calls.
type fakeTokenSource struct {
	mu          sync.Mutex
	token       InstallationToken
	ok          bool
	invalidated []string
}

func (s *fakeTokenSource) Token(context.Context, string) (InstallationToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.ok
}

func (s *fakeTokenSource) Invalidate(nwo, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = append(s.invalidated, nwo+" "+token)
}

func (s *fakeTokenSource) invalidations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.invalidated)
}

func installationSource(perms map[string]string) *fakeTokenSource {
	return &fakeTokenSource{ok: true, token: InstallationToken{Value: testInstallTok, InstallationID: 42, Permissions: perms}}
}

// tokenCall is one gh invocation and the token it ran with ("" = user login).
type tokenCall struct {
	args  []string
	token string
}

// tokenGH records the token every gh invocation runs with and answers through
// reply.
type tokenGH struct {
	mu    sync.Mutex
	calls []tokenCall
	reply func(args []string, token string) (string, error)
}

func (g *tokenGH) run(ctx context.Context, args ...string) (string, error) {
	token := ghTokenFrom(ctx)
	g.mu.Lock()
	g.calls = append(g.calls, tokenCall{args: slices.Clone(args), token: token})
	g.mu.Unlock()
	if g.reply == nil {
		return "{}", nil
	}
	return g.reply(args, token)
}

func (g *tokenGH) snapshot() []tokenCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

func (g *tokenGH) reset() {
	g.mu.Lock()
	g.calls = nil
	g.mu.Unlock()
}

// routedInvocations calls every exported Provider method that may run gh.
var routedInvocations = map[string]func(context.Context, *Provider){
	"GetPRStatus":     func(ctx context.Context, p *Provider) { _, _ = p.GetPRStatus(ctx, testOrigin, 7) },
	"GetCheckResults": func(ctx context.Context, p *Provider) { _, _ = p.GetCheckResults(ctx, testOrigin, 7) },
	"GetCheckSet":     func(ctx context.Context, p *Provider) { _, _ = p.GetCheckSet(ctx, testOrigin, 7) },
	"GetReviewComments": func(ctx context.Context, p *Provider) {
		_, _ = p.GetReviewComments(ctx, testOrigin, 7)
	},
	"GetReviewObservation": func(ctx context.Context, p *Provider) {
		_, _ = p.GetReviewObservation(ctx, testOrigin, 7)
	},
	"GetReviewInlineComments": func(ctx context.Context, p *Provider) {
		_, _ = p.GetReviewInlineComments(ctx, testOrigin, 7, 99)
	},
	"BlockingThreadAuthors": func(ctx context.Context, p *Provider) {
		_, _ = p.BlockingThreadAuthors(ctx, testOrigin, 7, map[string]bool{"bot": true})
	},
	"FindPRCommentsWithMarker": func(ctx context.Context, p *Provider) {
		_, _ = p.FindPRCommentsWithMarker(ctx, testOrigin, 7, "<!-- m -->")
	},
	"ListOpenPRs":         func(ctx context.Context, p *Provider) { _, _ = p.ListOpenPRs(ctx, testOrigin) },
	"ListClosedPRs":       func(ctx context.Context, p *Provider) { _, _ = p.ListClosedPRs(ctx, testOrigin) },
	"SearchPRsByTitleTag": func(ctx context.Context, p *Provider) { _, _ = p.SearchPRsByTitleTag(ctx, testOrigin, "BOS-1") },
	"GetBranchHeadSHA":    func(ctx context.Context, p *Provider) { _, _ = p.GetBranchHeadSHA(ctx, testOrigin, "main") },
	"ListWorkflowRuns":    func(ctx context.Context, p *Provider) { _, _ = p.ListWorkflowRuns(ctx, testOrigin, testSHA) },
	"GetFailedCheckLogs":  func(ctx context.Context, p *Provider) { _, _ = p.GetFailedCheckLogs(ctx, testOrigin, "1") },
	"GetPRMergeCommit":    func(ctx context.Context, p *Provider) { _, _ = p.GetPRMergeCommit(ctx, testOrigin, 7) },
	"GetAllowedMergeStrategies": func(ctx context.Context, p *Provider) {
		_, _ = p.GetAllowedMergeStrategies(ctx, testOrigin)
	},
	"OpenIssueURL": func(ctx context.Context, p *Provider) { _, _, _ = p.OpenIssueURL(ctx, "github.com", "o", "r", 3) },
	"PostCommitStatus": func(ctx context.Context, p *Provider) {
		_ = p.PostCommitStatus(ctx, testOrigin, testSHA, vcs.CommitStatus{State: "success", Context: "boss/verify"})
	},
	"CreateDraftPR": func(ctx context.Context, p *Provider) {
		_, _ = p.CreateDraftPR(ctx, vcs.CreatePROpts{RepoPath: testOrigin, HeadBranch: "h", BaseBranch: "main", Title: "t", Draft: true})
	},
	"MarkReadyForReview": func(ctx context.Context, p *Provider) { _ = p.MarkReadyForReview(ctx, testOrigin, 7) },
	"MergePR":            func(ctx context.Context, p *Provider) { _ = p.MergePR(ctx, testOrigin, 7, vcs.MergePROpts{}) },
	"UpdatePRTitle":      func(ctx context.Context, p *Provider) { _ = p.UpdatePRTitle(ctx, testOrigin, 7, "t") },
	"UpdatePRBody":       func(ctx context.Context, p *Provider) { _ = p.UpdatePRBody(ctx, testOrigin, 7, "b") },
	"QuotaStatus":        func(ctx context.Context, p *Provider) { _, _ = p.QuotaStatus(ctx) },
}

func newRoutedProvider(gh *tokenGH, src InstallationTokenSource, opts ...ProviderOption) *Provider {
	opts = append([]ProviderOption{WithRunGH(gh.run), WithSleepFunc(func(time.Duration) {})}, opts...)
	p := New(zerolog.Nop(), opts...)
	if src != nil {
		p.SetInstallationTokens(src)
	}
	return p
}

func TestEveryProviderMethodHasARoute(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(&Provider{})
	methods := map[string]bool{}
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		methods[name] = true
		r, ok := methodRoutes[name]
		if !ok {
			t.Errorf("exported method %s has no route in methodRoutes", name)
			continue
		}
		if _, invoked := routedInvocations[name]; r != routeNone && !invoked {
			t.Errorf("routed method %s is missing from routedInvocations, so no test proves its identity", name)
		}
	}
	for name := range methodRoutes {
		if !methods[name] {
			t.Errorf("methodRoutes names %s, which is not an exported *Provider method", name)
		}
	}
}

// identityWant is the token each of a method's gh calls must carry.
func identityWant(r route, statusWrite bool) string {
	switch {
	case r == routeRead, r == routeStatusWrite && statusWrite:
		return testInstallTok
	}
	return ""
}

func TestRoutedMethodsRunAsTheirIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		src         func() InstallationTokenSource
		statusWrite bool
		noToken     bool
	}{
		{name: "token with statuses write", src: func() InstallationTokenSource {
			return installationSource(map[string]string{"pull_requests": "read", "statuses": "write"})
		}, statusWrite: true},
		{name: "token without statuses write", src: func() InstallationTokenSource {
			return installationSource(map[string]string{"pull_requests": "read", "statuses": "read"})
		}},
		{name: "no source attached", src: func() InstallationTokenSource { return nil }, noToken: true},
		{name: "source answers user login", src: func() InstallationTokenSource { return &fakeTokenSource{} }, noToken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, name := range slices.Sorted(maps.Keys(routedInvocations)) {
				gh := &tokenGH{}
				p := newRoutedProvider(gh, tc.src())
				routedInvocations[name](context.Background(), p)
				want := identityWant(methodRoutes[name], tc.statusWrite)
				if tc.noToken {
					want = ""
				}
				calls := gh.snapshot()
				if len(calls) == 0 {
					t.Errorf("%s made no gh call; the test proves nothing for it", name)
				}
				for _, c := range calls {
					if c.token != want {
						t.Errorf("%s: gh %v ran with token %q, want %q", name, c.args, c.token, want)
					}
				}
			}
		})
	}
}

func TestOperatorActionsKeepTheUserLoginForTheirReads(t *testing.T) {
	t.Parallel()
	gh := &tokenGH{}
	p := newRoutedProvider(gh, installationSource(map[string]string{"statuses": "write"}))
	ctx := withRoute(context.Background(), "MergePR", testNWO)
	_, _ = p.GetPRStatus(ctx, testOrigin, 7)
	_, _ = p.GetAllowedMergeStrategies(ctx, testOrigin)
	for _, c := range gh.snapshot() {
		if c.token != "" {
			t.Fatalf("read inside an operator action ran gh %v with the installation token", c.args)
		}
	}
}

// GitHub omits a repo's merge settings for a token without contents: write,
// which the read-only installation token lacks, so they are read as the user.
func TestMergeStrategiesAreReadOnTheUserLogin(t *testing.T) {
	t.Parallel()
	gh := &tokenGH{reply: func(_ []string, token string) (string, error) {
		if token != "" {
			return `{"allow_merge_commit":null,"allow_squash_merge":null,"allow_rebase_merge":null}`, nil
		}
		return `{"allow_merge_commit":false,"allow_squash_merge":true,"allow_rebase_merge":false}`, nil
	}}
	p := newRoutedProvider(gh, installationSource(map[string]string{"contents": "read", "statuses": "write"}))
	got, err := p.GetAllowedMergeStrategies(context.Background(), testOrigin)
	if err != nil {
		t.Fatalf("GetAllowedMergeStrategies: %v", err)
	}
	if !slices.Equal(got, []string{"squash"}) {
		t.Fatalf("allowed strategies = %v, want [squash] from the user login", got)
	}
	for _, c := range gh.snapshot() {
		if c.token != "" {
			t.Fatalf("gh %v ran with the installation token", c.args)
		}
	}
}

func TestNonGitHubReposUseTheUserLogin(t *testing.T) {
	t.Parallel()
	gh := &tokenGH{}
	p := newRoutedProvider(gh, installationSource(nil))
	_, _ = p.GetPRStatus(context.Background(), "https://gitlab.com/o/r", 7)
	_, _, _ = p.OpenIssueURL(context.Background(), "github.example.com", "o", "r", 3)
	calls := gh.snapshot()
	if len(calls) < 2 {
		t.Fatalf("gh calls = %d, want both methods to run gh", len(calls))
	}
	for _, c := range calls {
		if c.token != "" {
			t.Fatalf("gh %v ran with the installation token for a repo off github.com", c.args)
		}
	}
}

// refusalStderr is the stderr gh prints for each installation token refusal.
var refusalStderr = map[string]string{
	"bad credentials": "gh: HTTP 401: Bad credentials (https://api.github.com/repos/o/r/actions/jobs/1/logs)",
	"not accessible":  "gh: Resource not accessible by integration (HTTP 403)",
}

func TestInstallationRefusalFallsBackToTheUserOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		refusal        string
		userFails      bool
		wantInvalidate bool
	}{
		{refusal: "bad credentials", wantInvalidate: true},
		{refusal: "not accessible"},
		{refusal: "bad credentials", userFails: true, wantInvalidate: true},
		{refusal: "not accessible", userFails: true},
	} {
		t.Run(tc.refusal+map[bool]string{true: ", user fails too", false: ""}[tc.userFails], func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			gh := &tokenGH{reply: func(args []string, token string) (string, error) {
				switch {
				case token != "":
					return "", &ghError{args: args, stderr: refusalStderr[tc.refusal], err: errors.New("exit status 1")}
				case tc.userFails:
					return "", &ghError{args: args, stderr: "gh: HTTP 500: boom", err: errors.New("exit status 1")}
				}
				return "user logs", nil
			}}
			src := installationSource(nil)
			p := New(zerolog.New(&logs), WithRunGH(gh.run))
			p.SetInstallationTokens(src)

			out, err := p.GetFailedCheckLogs(context.Background(), testOrigin, "1")
			calls := gh.snapshot()
			if len(calls) != 2 || calls[0].token != testInstallTok || calls[1].token != "" {
				t.Fatalf("gh calls = %+v, want one installation call then one user retry", calls)
			}
			if tc.userFails {
				if err == nil || !strings.Contains(err.Error(), "boom") {
					t.Fatalf("err = %v, want the user retry's failure as-is", err)
				}
			} else if err != nil || out != "user logs" {
				t.Fatalf("GetFailedCheckLogs = %q, %v; want the user retry's result", out, err)
			}
			wantInvalidations := []string(nil)
			if tc.wantInvalidate {
				wantInvalidations = []string{testNWO + " " + testInstallTok}
			}
			if got := src.invalidations(); !slices.Equal(got, wantInvalidations) {
				t.Fatalf("Invalidate calls = %q, want %q", got, wantInvalidations)
			}
			if !strings.Contains(logs.String(), "retrying once on the user login") {
				t.Fatalf("no refusal warning logged:\n%s", logs.String())
			}
			for _, rendered := range []string{logs.String(), errText(err)} {
				if strings.Contains(rendered, testInstallTok) {
					t.Fatalf("the installation token leaked:\n%s", rendered)
				}
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestInstallationAndUserPoolsPauseIndependently(t *testing.T) {
	t.Parallel()
	for _, exhausted := range []string{"user", "installation"} {
		t.Run(exhausted+" exhausted", func(t *testing.T) {
			t.Parallel()
			clock := newFakeClock()
			exhaustedToken := map[string]string{"user": "", "installation": testInstallTok}[exhausted]
			gh := &tokenGH{reply: func(args []string, token string) (string, error) {
				switch {
				case isRateLimitProbe(args) && token == exhaustedToken:
					return rateLimitBody(clock, 0, 20*time.Minute), nil
				case isRateLimitProbe(args):
					return rateLimitBody(clock, 4000, 20*time.Minute), nil
				case isPRView(args) && token == exhaustedToken:
					return "", errors.New(graphQLExhausted)
				case isPRView(args):
					return draftPRView, nil
				}
				return "{}", nil
			}}
			p := newRoutedProvider(gh, installationSource(nil), WithNowFunc(clock.Now))
			ctx := context.Background()
			read := func() error { _, err := p.GetPRStatus(ctx, testOrigin, 7); return err }
			userRead := func() error { _, err := p.GetPRStatus(withRoute(ctx, "MergePR", testNWO), testOrigin, 7); return err }
			exhaustedRead, otherRead := read, userRead
			if exhausted == "user" {
				exhaustedRead, otherRead = userRead, read
			}

			err := exhaustedRead()
			if !errors.Is(err, vcs.ErrRateLimited) {
				t.Fatalf("exhausted pool read err = %v, want vcs.ErrRateLimited", err)
			}
			wantPool := map[string]string{"user": "for user", "installation": "for installation:42"}[exhausted]
			if !strings.Contains(err.Error(), wantPool) {
				t.Fatalf("err = %q, want it to name the pool (%q)", err, wantPool)
			}
			for _, c := range gh.snapshot() {
				if isRateLimitProbe(c.args) && c.token != exhaustedToken {
					t.Fatalf("rate_limit probe ran with token %q, want the refused identity's %q", c.token, exhaustedToken)
				}
			}
			gh.reset()
			if err := exhaustedRead(); !errors.Is(err, vcs.ErrRateLimited) || len(gh.snapshot()) != 0 {
				t.Fatalf("paused pool read err = %v after %d gh calls, want a breaker refusal with none", err, len(gh.snapshot()))
			}
			if err := otherRead(); err != nil {
				t.Fatalf("other pool read err = %v, want it to keep running", err)
			}
			if n := len(gh.snapshot()); n != 1 {
				t.Fatalf("other pool gh calls = %d, want 1", n)
			}
		})
	}
}

// writeStubGH puts a gh on a fresh PATH that prints the tokens it sees, or
// fails with fail on stderr when fail is set.
func writeStubGH(t *testing.T, fail string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"GH_TOKEN=${GH_TOKEN-unset} GITHUB_TOKEN=${GITHUB_TOKEN-unset}\"\n"
	if fail != "" {
		script = "#!/bin/sh\necho '" + fail + "' >&2\nexit 1\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
}

// The stub-gh tests mutate the process env, so they cannot run in parallel.
func TestDefaultRunGHSetsTheTokenOnlyOnTheGHProcess(t *testing.T) {
	writeStubGH(t, "")
	t.Setenv("GH_TOKEN", "parent-gh-token")
	t.Setenv("GITHUB_TOKEN", "parent-github-token")
	ctx := context.Background()

	userP := New(zerolog.Nop())
	userP.SetInstallationTokens(&fakeTokenSource{})
	out, err := userP.GetFailedCheckLogs(ctx, testOrigin, "1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "GH_TOKEN=parent-gh-token GITHUB_TOKEN=parent-github-token\n"; out != want {
		t.Fatalf("user login gh saw %q, want the inherited env %q", out, want)
	}

	p := New(zerolog.Nop())
	p.SetInstallationTokens(installationSource(nil))
	out, err = p.GetFailedCheckLogs(ctx, testOrigin, "1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "GH_TOKEN=" + testInstallTok + " GITHUB_TOKEN=unset\n"; out != want {
		t.Fatalf("installation gh saw %q, want %q", out, want)
	}
	if got := os.Getenv("GH_TOKEN"); got != "parent-gh-token" {
		t.Fatalf("daemon GH_TOKEN = %q after the call, want it unchanged", got)
	}
}

func TestDefaultRunGHWithoutATokenKeepsTheInheritedEnv(t *testing.T) {
	ctx := withIdentity(context.Background(), userIdentity)
	if env := ghEnv(ctx); env != nil {
		t.Fatalf("user login cmd.Env = %v, want nil (inherit)", env)
	}
	if env := ghEnv(context.Background()); env != nil {
		t.Fatalf("unrouted cmd.Env = %v, want nil (inherit)", env)
	}
}

func TestFailedInstallationCallErrorOmitsTheToken(t *testing.T) {
	writeStubGH(t, "gh: HTTP 500: server error")
	var logs bytes.Buffer
	p := New(zerolog.New(&logs))
	p.SetInstallationTokens(installationSource(nil))
	_, err := p.GetFailedCheckLogs(context.Background(), testOrigin, "1")
	if err == nil || !strings.Contains(err.Error(), "server error") {
		t.Fatalf("err = %v, want the stub's failure", err)
	}
	if strings.Contains(err.Error(), testInstallTok) || strings.Contains(logs.String(), testInstallTok) {
		t.Fatalf("the installation token leaked into the error or logs:\n%v\n%s", err, logs.String())
	}
}

// recordedUsage is one UsageRecorder.Record call.
type recordedUsage struct{ label, quota, identity string }

type recordedSkip struct{ label, quota, identity, outcome string }

type fakeUsageRecorder struct {
	mu      sync.Mutex
	records []recordedUsage
	skips   []recordedSkip
}

func (r *fakeUsageRecorder) Record(label, quota, identity string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, recordedUsage{label, quota, identity})
}

func (r *fakeUsageRecorder) RecordSkip(label, quota, identity, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skips = append(r.skips, recordedSkip{label, quota, identity, outcome})
}

func TestLedgerRecordsTheCallIdentity_GitHubUsage_AC226501(t *testing.T) {
	t.Parallel()
	usage := &fakeUsageRecorder{}
	gh := &tokenGH{reply: func(args []string, token string) (string, error) {
		if token != "" && args[0] == "pr" && args[1] == "merge" {
			t.Errorf("merge ran with the installation token")
		}
		return "{}", nil
	}}
	p := newRoutedProvider(gh, installationSource(nil), WithUsageRecorder(usage))
	ctx := context.Background()
	_, _ = p.GetFailedCheckLogs(ctx, testOrigin, "1")
	_ = p.MergePR(ctx, testOrigin, 7, vcs.MergePROpts{})

	want := []recordedUsage{
		{label: "daemon:unattributed", quota: "core", identity: "installation:42"},
		{label: "daemon:unattributed", quota: "graphql", identity: "user"},
	}
	usage.mu.Lock()
	defer usage.mu.Unlock()
	if !slices.Equal(usage.records, want) {
		t.Fatalf("ledger records = %+v, want %+v", usage.records, want)
	}
	for _, r := range usage.records {
		if strings.Contains(r.identity, testInstallTok) {
			t.Fatalf("a ledger record carries the token: %+v", r)
		}
	}
}

// TestBreakerSkipIsMeteredUnderTheRefusedIdentity: a read refused because its
// installation's pool is paused is metered as a breaker skip against that
// installation, never the user, and is not sent.
func TestBreakerSkipIsMeteredUnderTheRefusedIdentity_GitHubUsage_AC226501(t *testing.T) {
	t.Parallel()
	usage := &fakeUsageRecorder{}
	gh := &tokenGH{}
	p := newRoutedProvider(gh, installationSource(nil), WithUsageRecorder(usage))
	p.limits.pause(quotaPool{identity: "installation:42", quota: quotaCore}, time.Now().Add(time.Hour))

	if _, err := p.GetFailedCheckLogs(context.Background(), testOrigin, "1"); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("GetFailedCheckLogs err = %v, want vcs.ErrRateLimited from the paused installation pool", err)
	}
	want := []recordedSkip{{label: "daemon:unattributed", quota: "core", identity: "installation:42", outcome: "breaker"}}
	usage.mu.Lock()
	defer usage.mu.Unlock()
	if !slices.Equal(usage.skips, want) || len(usage.records) != 0 {
		t.Fatalf("skips = %+v, sent = %+v; want skips %+v and nothing sent", usage.skips, usage.records, want)
	}
	if calls := gh.snapshot(); len(calls) != 0 {
		t.Fatalf("gh ran %d call(s) while the installation pool was paused", len(calls))
	}
}

func (r *fakeUsageRecorder) RecordNotModified(string, string, string) {}
