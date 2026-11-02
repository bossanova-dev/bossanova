package ghtoken

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
)

const (
	sshOrigin       = "git@github.com:owner/repo.git"
	canonicalOrigin = "https://github.com/owner/repo"
	installToken    = "ghs_fake-installation-token-do-not-print"
	sessionToken    = "daemon-session-1"
)

var _ TokenHolder = (*fakeGHTokenHolder)(nil)

// issuedCall is one IssueGitHubInstallationToken request the fake saw.
type issuedCall struct {
	origin string
	bearer string
}

// issueFunc answers the n-th IssueInstallationToken request.
type issueFunc func(ctx context.Context, n int, call issuedCall) (MintedToken, error)

// fakeTokenIssuer answers IssueInstallationToken through respond and records
// every request.
type fakeTokenIssuer struct {
	mu      sync.Mutex
	calls   []issuedCall
	respond issueFunc
}

func (f *fakeTokenIssuer) IssueInstallationToken(ctx context.Context, sessionToken, repoOriginURL string) (MintedToken, error) {
	call := issuedCall{origin: repoOriginURL, bearer: sessionToken}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	n := len(f.calls)
	f.mu.Unlock()
	return f.respond(ctx, n, call)
}

func (f *fakeTokenIssuer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeTokenIssuer) countFor(origin string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.origin == origin {
			n++
		}
	}
	return n
}

func (f *fakeTokenIssuer) call(i int) issuedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

type fakeGHTokenHolder struct {
	mu  sync.Mutex
	tok string
}

func (h *fakeGHTokenHolder) Get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tok
}

func (h *fakeGHTokenHolder) Set(tok string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tok = tok
}

func (h *fakeGHTokenHolder) CompareAndSwap(old, tok string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tok != old {
		return false
	}
	h.tok = tok
	return true
}

type fakeGHTokenRepos struct{ repos []*models.Repo }

func (r fakeGHTokenRepos) List(context.Context) ([]*models.Repo, error) { return r.repos, nil }

// ghtokenClock is a settable clock.
type ghtokenClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *ghtokenClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *ghtokenClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

var epoch = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func minted(expiresAt time.Time) MintedToken {
	return MintedToken{
		Token:          installToken,
		ExpiresAt:      expiresAt,
		InstallationID: 4242,
		AccountLogin:   "owner",
		Permissions:    map[string]string{"contents": "read", "pull_requests": "read"},
	}
}

func refusal(code connect.Code) error {
	return connect.NewError(code, errors.New("refused"))
}

type harness struct {
	src    *Source
	issuer *fakeTokenIssuer
	tokens *fakeGHTokenHolder
	clock  *ghtokenClock
}

func newHarness(t *testing.T, cfg Config, respond issueFunc) harness {
	t.Helper()
	h := harness{
		issuer: &fakeTokenIssuer{respond: respond},
		tokens: &fakeGHTokenHolder{tok: sessionToken},
		clock:  &ghtokenClock{now: epoch},
	}
	cfg.Client = h.issuer
	cfg.Tokens = h.tokens
	cfg.Now = h.clock.Now
	if cfg.Jitter == nil {
		cfg.Jitter = func(time.Duration) time.Duration { return 0 }
	}
	h.src = New(cfg)
	return h
}

func TestCredentialWithoutUpstreamMakesNoCall(t *testing.T) {
	t.Parallel()
	issuer := &fakeTokenIssuer{respond: func(context.Context, int, issuedCall) (MintedToken, error) {
		return minted(epoch.Add(time.Hour)), nil
	}}
	cases := []struct {
		name   string
		cfg    Config
		origin string
		want   Reason
	}{
		{"local-only daemon, GitHub repo", Config{}, sshOrigin, ReasonNotSignedIn},
		{"local-only daemon, non-GitHub repo", Config{}, "git@gitlab.com:owner/repo.git", ReasonNotGitHub},
		{"empty daemon token", Config{Client: issuer, Tokens: &fakeGHTokenHolder{}}, sshOrigin, ReasonNotSignedIn},
		{"signed in, non-GitHub repo", Config{Client: issuer, Tokens: &fakeGHTokenHolder{tok: sessionToken}}, "https://gitlab.com/owner/repo", ReasonNotGitHub},
		{"signed in, unparseable origin", Config{Client: issuer, Tokens: &fakeGHTokenHolder{tok: sessionToken}}, "not a url", ReasonNotGitHub},
	}
	for _, tc := range cases {
		got := New(tc.cfg).Credential(context.Background(), tc.origin)
		if got.Identity != IdentityUser || got.Reason != tc.want || got.Token() != "" {
			t.Errorf("%s: Credential = %v (token %t), want user/%s", tc.name, got, got.Token() != "", tc.want)
		}
	}
	if n := issuer.count(); n != 0 {
		t.Fatalf("RPC calls = %d, want 0", n)
	}
}

func TestCredentialMintsCachesAndRefreshes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{}, func(_ context.Context, n int, _ issuedCall) (MintedToken, error) {
		if n == 1 {
			return minted(epoch.Add(time.Hour)), nil
		}
		return minted(epoch.Add(2 * time.Hour)), nil
	})
	ctx := context.Background()

	got := h.src.Credential(ctx, sshOrigin)
	if got.Identity != IdentityInstallation || got.Reason != ReasonInstallation || got.InstallationID != 4242 ||
		got.AccountLogin != "owner" || got.Token() != installToken || !got.ExpiresAt.Equal(epoch.Add(time.Hour)) {
		t.Fatalf("first Credential = %v, want the installation token", got)
	}
	if got.Permissions["contents"] != "read" {
		t.Errorf("Permissions = %v, want the granted map", got.Permissions)
	}
	if c := h.issuer.call(0); c.origin != canonicalOrigin || c.bearer != sessionToken {
		t.Fatalf("request = %+v, want origin %q and the daemon session bearer", c, canonicalOrigin)
	}

	// Inside the margin: 4 minutes left is still more than refreshMargin.
	h.clock.set(epoch.Add(56 * time.Minute))
	if got := h.src.Credential(ctx, "https://github.com/owner/repo.git"); got.Token() != installToken {
		t.Fatalf("cached Credential = %v, want the cached token", got)
	}
	if n := h.issuer.count(); n != 1 {
		t.Fatalf("RPC calls inside the refresh margin = %d, want 1", n)
	}

	// Past expires_at - refreshMargin: mint again.
	h.clock.set(epoch.Add(time.Hour - refreshMargin + time.Second))
	got = h.src.Credential(ctx, sshOrigin)
	if n := h.issuer.count(); n != 2 {
		t.Fatalf("RPC calls past the refresh margin = %d, want 2", n)
	}
	if !got.ExpiresAt.Equal(epoch.Add(2 * time.Hour)) {
		t.Fatalf("refreshed ExpiresAt = %s, want the new mint's", got.ExpiresAt)
	}
}

func TestCredentialRefusalsWaitMaxRetryInterval(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code connect.Code
		want Reason
	}{
		{connect.CodePermissionDenied, ReasonNoInstallation},
		{connect.CodeFailedPrecondition, ReasonAppUnavailable},
		{connect.CodeUnimplemented, ReasonUpstreamUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{}, func(context.Context, int, issuedCall) (MintedToken, error) {
				return MintedToken{}, refusal(tc.code)
			})
			ctx := context.Background()
			for _, at := range []time.Duration{0, time.Minute, MaxRetryInterval - time.Second} {
				h.clock.set(epoch.Add(at))
				got := h.src.Credential(ctx, sshOrigin)
				if got.Identity != IdentityUser || got.Reason != tc.want || got.Token() != "" {
					t.Fatalf("at +%s Credential = %v, want user/%s", at, got, tc.want)
				}
			}
			if n := h.issuer.count(); n != 1 {
				t.Fatalf("RPC calls before MaxRetryInterval = %d, want 1", n)
			}
			h.clock.set(epoch.Add(MaxRetryInterval))
			h.src.Credential(ctx, sshOrigin)
			if n := h.issuer.count(); n != 2 {
				t.Fatalf("RPC calls at MaxRetryInterval = %d, want 2", n)
			}
		})
	}
}

func TestCredentialUpstreamErrorBackoffIsBounded(t *testing.T) {
	t.Parallel()
	for _, jitter := range []struct {
		name string
		fn   func(time.Duration) time.Duration
	}{
		{"no jitter", func(time.Duration) time.Duration { return 0 }},
		{"maximum jitter", func(limit time.Duration) time.Duration { return max(limit-1, 0) }},
	} {
		t.Run(jitter.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{
				Jitter: jitter.fn,
				Repos:  fakeGHTokenRepos{repos: []*models.Repo{{ID: "r1", OriginURL: sshOrigin}}},
			}, func(context.Context, int, issuedCall) (MintedToken, error) {
				return MintedToken{}, refusal(connect.CodeUnavailable)
			})
			ctx := context.Background()
			now := epoch
			var prev time.Duration
			for i := range 12 {
				got := h.src.Credential(ctx, sshOrigin)
				if got.Identity != IdentityUser || got.Reason != ReasonUpstreamError {
					t.Fatalf("failure %d: Credential = %v, want user/upstream_error", i+1, got)
				}
				if n := h.issuer.count(); n != i+1 {
					t.Fatalf("failure %d: RPC calls = %d, want %d", i+1, n, i+1)
				}
				report, err := h.src.Report(ctx)
				if err != nil {
					t.Fatal(err)
				}
				wait := report[0].NextAttemptAt.Sub(now)
				if wait <= 0 || wait > MaxRetryInterval {
					t.Fatalf("failure %d: wait %s outside (0, %s]", i+1, wait, MaxRetryInterval)
				}
				if wait < prev {
					t.Fatalf("failure %d: wait %s shrank from %s", i+1, wait, prev)
				}
				prev = wait
				// Just before the next attempt: no call.
				h.clock.set(report[0].NextAttemptAt.Add(-time.Nanosecond))
				h.src.Credential(ctx, sshOrigin)
				if n := h.issuer.count(); n != i+1 {
					t.Fatalf("failure %d: RPC made before NextAttemptAt", i+1)
				}
				now = report[0].NextAttemptAt
				h.clock.set(now)
			}
			if prev != MaxRetryInterval {
				t.Fatalf("final wait = %s, want the %s ceiling", prev, MaxRetryInterval)
			}
		})
	}
}

func TestCredentialServesCachedTokenThroughFailedRefresh(t *testing.T) {
	t.Parallel()
	expiry := epoch.Add(time.Hour)
	h := newHarness(t, Config{}, func(_ context.Context, n int, _ issuedCall) (MintedToken, error) {
		if n == 1 {
			return minted(expiry), nil
		}
		return MintedToken{}, refusal(connect.CodeUnavailable)
	})
	ctx := context.Background()
	h.src.Credential(ctx, sshOrigin)

	for _, at := range []time.Time{
		expiry.Add(-refreshMargin + time.Second), // refresh due, and it fails
		expiry.Add(-time.Minute),                 // refresh due again, fails again
		expiry.Add(-staleGuard - time.Second),    // still usable
	} {
		h.clock.set(at)
		if got := h.src.Credential(ctx, sshOrigin); got.Identity != IdentityInstallation || got.Token() != installToken {
			t.Fatalf("at expiry-%s Credential = %v, want the still-valid cached token", expiry.Sub(at), got)
		}
	}
	if n := h.issuer.count(); n < 2 {
		t.Fatalf("RPC calls = %d, want the refresh to have been attempted", n)
	}
	h.clock.set(expiry.Add(-staleGuard))
	if got := h.src.Credential(ctx, sshOrigin); got.Identity != IdentityUser || got.Reason != ReasonUpstreamError || got.Token() != "" {
		t.Fatalf("at expiry-staleGuard Credential = %v, want user/upstream_error", got)
	}
}

func TestCredentialSkewedExpiryDoesNotHotLoop(t *testing.T) {
	t.Parallel()
	// bosso hands back a token already inside bossd's refresh margin.
	h := newHarness(t, Config{}, func(context.Context, int, issuedCall) (MintedToken, error) {
		return minted(epoch.Add(2 * time.Minute)), nil
	})
	ctx := context.Background()
	for _, at := range []time.Duration{0, time.Second, minAttemptGap - time.Second} {
		h.clock.set(epoch.Add(at))
		if got := h.src.Credential(ctx, sshOrigin); got.Token() != installToken {
			t.Fatalf("at +%s Credential = %v, want the short-lived token", at, got)
		}
	}
	if n := h.issuer.count(); n != 1 {
		t.Fatalf("RPC calls within %s = %d, want 1", minAttemptGap, n)
	}
	h.clock.set(epoch.Add(minAttemptGap))
	h.src.Credential(ctx, sshOrigin)
	if n := h.issuer.count(); n != 2 {
		t.Fatalf("RPC calls at %s = %d, want 2", minAttemptGap, n)
	}
}

func TestCredentialReRegistersOnceOnUnauthenticated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		reRegister   func(context.Context) (string, error)
		retryFails   bool
		wantIdentity Identity
		wantReason   Reason
		wantCalls    int
	}{
		{"re-register and retry", func(context.Context) (string, error) { return "daemon-session-2", nil }, false, IdentityInstallation, ReasonInstallation, 2},
		{"re-register fails", func(context.Context) (string, error) { return "", errors.New("register refused") }, false, IdentityUser, ReasonNotSignedIn, 1},
		{"re-register returns empty token", func(context.Context) (string, error) { return "", nil }, false, IdentityUser, ReasonNotSignedIn, 1},
		{"retry still rejected", func(context.Context) (string, error) { return "daemon-session-2", nil }, true, IdentityUser, ReasonNotSignedIn, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			reRegisters := 0
			h := newHarness(t, Config{ReRegister: func(ctx context.Context) (string, error) {
				mu.Lock()
				reRegisters++
				mu.Unlock()
				return tc.reRegister(ctx)
			}}, func(_ context.Context, n int, call issuedCall) (MintedToken, error) {
				if n == 1 || tc.retryFails {
					return MintedToken{}, refusal(connect.CodeUnauthenticated)
				}
				if call.bearer != "daemon-session-2" {
					return MintedToken{}, fmt.Errorf("retry bearer %q, want the adopted token", call.bearer)
				}
				return minted(epoch.Add(time.Hour)), nil
			})
			got := h.src.Credential(context.Background(), sshOrigin)
			if got.Identity != tc.wantIdentity || got.Reason != tc.wantReason {
				t.Fatalf("Credential = %v, want %s/%s", got, tc.wantIdentity, tc.wantReason)
			}
			if n := h.issuer.count(); n != tc.wantCalls {
				t.Fatalf("RPC calls = %d, want %d", n, tc.wantCalls)
			}
			mu.Lock()
			defer mu.Unlock()
			if reRegisters != 1 {
				t.Fatalf("re-registers = %d, want exactly 1", reRegisters)
			}
			if tc.wantCalls == 2 && h.tokens.Get() != "daemon-session-2" {
				t.Fatalf("session token = %q, want the adopted re-registered token", h.tokens.Get())
			}
			// A failed re-register waits the refusal interval before asking again.
			if tc.wantReason == ReasonNotSignedIn {
				h.clock.set(epoch.Add(MaxRetryInterval - time.Second))
				h.src.Credential(context.Background(), sshOrigin)
				if n := h.issuer.count(); n != tc.wantCalls {
					t.Fatalf("RPC calls before MaxRetryInterval = %d, want %d", n, tc.wantCalls)
				}
			}
		})
	}
}

func TestCredentialConcurrentCallersShareOneFetch(t *testing.T) {
	t.Parallel()
	const slowOrigin = "https://github.com/owner/slow"
	const fastOrigin = "https://github.com/owner/fast"
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newHarness(t, Config{}, func(ctx context.Context, _ int, call issuedCall) (MintedToken, error) {
		if call.origin == slowOrigin {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return MintedToken{}, ctx.Err()
			}
		}
		return minted(epoch.Add(time.Hour)), nil
	})
	ctx := context.Background()

	const callers = 8
	results := make(chan Credential, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() { results <- h.src.Credential(ctx, slowOrigin) })
	}
	<-entered

	// The slow repository's fetch is in flight; another repository answers.
	if got := h.src.Credential(ctx, fastOrigin); got.Token() != installToken {
		t.Fatalf("fast repo Credential = %v, want its token while the slow fetch blocks", got)
	}
	close(release)
	wg.Wait()
	close(results)
	for got := range results {
		if got.Token() != installToken {
			t.Fatalf("slow repo Credential = %v, want the shared fetch's token", got)
		}
	}
	if n := h.issuer.countFor(slowOrigin); n != 1 {
		t.Fatalf("RPC calls for the slow repo = %d, want 1 shared fetch", n)
	}
}

func TestReportListsEveryRepoWithoutCalling(t *testing.T) {
	t.Parallel()
	repos := fakeGHTokenRepos{repos: []*models.Repo{
		{ID: "minted", OriginURL: sshOrigin},
		{ID: "refused", OriginURL: "https://github.com/owner/refused"},
		{ID: "untouched", OriginURL: "https://github.com/owner/untouched"},
		{ID: "gitlab", OriginURL: "https://gitlab.com/owner/repo"},
	}}
	h := newHarness(t, Config{Repos: repos}, func(_ context.Context, _ int, call issuedCall) (MintedToken, error) {
		if call.origin == "https://github.com/owner/refused" {
			return MintedToken{}, refusal(connect.CodePermissionDenied)
		}
		return minted(epoch.Add(time.Hour)), nil
	})
	ctx := context.Background()
	h.src.Credential(ctx, sshOrigin)
	h.src.Credential(ctx, "https://github.com/owner/refused")
	before := h.issuer.count()

	report, err := h.src.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := h.issuer.count(); n != before {
		t.Fatalf("Report made %d RPCs, want none", n-before)
	}
	want := map[string]RepoCredentialState{
		"minted": {
			RepoID: "minted", Origin: canonicalOrigin, Identity: IdentityInstallation, InstallationID: 4242,
			AccountLogin: "owner", Reason: ReasonInstallation, ExpiresAt: epoch.Add(time.Hour),
			NextAttemptAt: epoch.Add(time.Hour - refreshMargin),
		},
		"refused": {
			RepoID: "refused", Origin: "https://github.com/owner/refused", Identity: IdentityUser,
			Reason: ReasonNoInstallation, NextAttemptAt: epoch.Add(MaxRetryInterval),
		},
		"untouched": {RepoID: "untouched", Origin: "https://github.com/owner/untouched", Identity: IdentityUser, Reason: ReasonNotRequested},
		"gitlab":    {RepoID: "gitlab", Origin: "https://gitlab.com/owner/repo", Identity: IdentityUser, Reason: ReasonNotGitHub},
	}
	if len(report) != len(want) {
		t.Fatalf("Report has %d entries, want %d", len(report), len(want))
	}
	for _, got := range report {
		if w := want[got.RepoID]; got != w {
			t.Errorf("Report[%s] = %+v, want %+v", got.RepoID, got, w)
		}
	}

	local, err := New(Config{Repos: repos}).Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range local {
		wantReason := ReasonNotSignedIn
		if got.RepoID == "gitlab" {
			wantReason = ReasonNotGitHub
		}
		if got.Identity != IdentityUser || got.Reason != wantReason {
			t.Errorf("local-only Report[%s] = %+v, want user/%s", got.RepoID, got, wantReason)
		}
	}
}

func TestTokenNeverRendered(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := newHarness(t, Config{
		Logger: zerolog.New(&logs).Level(zerolog.DebugLevel),
		Repos:  fakeGHTokenRepos{repos: []*models.Repo{{ID: "r1", OriginURL: sshOrigin}}},
	}, func(_ context.Context, n int, _ issuedCall) (MintedToken, error) {
		if n == 1 {
			return minted(epoch.Add(time.Hour)), nil
		}
		return MintedToken{}, refusal(connect.CodeUnavailable)
	})
	ctx := context.Background()
	cred := h.src.Credential(ctx, sshOrigin)
	if cred.Token() != installToken {
		t.Fatalf("setup: Credential = %v, want the minted token", cred)
	}
	h.clock.set(epoch.Add(time.Hour - time.Minute))
	h.src.Credential(ctx, sshOrigin) // failed refresh, logged at Warn
	report, err := h.src.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rendered := []string{
		fmt.Sprintf("%v", cred), fmt.Sprintf("%+v", cred), fmt.Sprintf("%#v", cred), fmt.Sprint(cred),
		fmt.Sprintf("%+v", &cred), fmt.Sprintf("%+v", struct{ C Credential }{cred}),
		fmt.Sprintf("%v", report), fmt.Sprintf("%+v", report), fmt.Sprintf("%#v", report),
		fmt.Sprintf("%v", minted(epoch)), fmt.Sprintf("%+v", minted(epoch)), fmt.Sprintf("%#v", minted(epoch)),
	}
	for _, v := range []any{cred, report} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		rendered = append(rendered, string(b))
	}
	rendered = append(rendered, logs.String())
	if !strings.Contains(logs.String(), "IssueGitHubInstallationToken failed") {
		t.Fatalf("captured logs lack the failure warning, so the scan proves nothing:\n%s", logs.String())
	}
	for _, out := range rendered {
		for _, secret := range []string{installToken, sessionToken} {
			if strings.Contains(out, secret) {
				t.Fatalf("a rendering leaked a token:\n%s", out)
			}
		}
	}
}

func TestCredentialRetriesWithTokenAnotherCallerRotated(t *testing.T) {
	t.Parallel()
	var reRegisters atomic.Int32
	var h harness
	h = newHarness(t, Config{ReRegister: func(context.Context) (string, error) {
		reRegisters.Add(1)
		return "daemon-session-3", nil
	}}, func(_ context.Context, _ int, call issuedCall) (MintedToken, error) {
		if call.bearer == sessionToken {
			// Another repository's caller re-registered while this request
			// was in flight.
			h.tokens.Set("daemon-session-2")
			return MintedToken{}, refusal(connect.CodeUnauthenticated)
		}
		if call.bearer != "daemon-session-2" {
			return MintedToken{}, fmt.Errorf("retry bearer %q, want the rotated token", call.bearer)
		}
		return minted(epoch.Add(time.Hour)), nil
	})
	got := h.src.Credential(context.Background(), sshOrigin)
	if got.Token() != installToken {
		t.Fatalf("Credential = %v, want the token minted with the rotated session token", got)
	}
	if n := reRegisters.Load(); n != 0 {
		t.Fatalf("re-registers = %d, want 0: re-registering would revoke the rotated token", n)
	}
	if tok := h.tokens.Get(); tok != "daemon-session-2" {
		t.Fatalf("session token = %q, want the rotation kept", tok)
	}
}

func TestCredentialConcurrentRejectionsReRegisterOnce(t *testing.T) {
	t.Parallel()
	const repos = 6
	var rejected sync.WaitGroup
	rejected.Add(repos)
	allRejected := make(chan struct{})
	go func() { rejected.Wait(); close(allRejected) }()
	var reRegisters atomic.Int32
	h := newHarness(t, Config{ReRegister: func(context.Context) (string, error) {
		return fmt.Sprintf("daemon-session-%d", 1+reRegisters.Add(1)), nil
	}}, func(ctx context.Context, _ int, call issuedCall) (MintedToken, error) {
		if call.bearer == sessionToken {
			// Hold every first attempt until all repositories have sent the
			// stale token, so each one sees the rejection concurrently.
			rejected.Done()
			select {
			case <-allRejected:
			case <-ctx.Done():
				return MintedToken{}, ctx.Err()
			}
			return MintedToken{}, refusal(connect.CodeUnauthenticated)
		}
		if call.bearer != "daemon-session-2" {
			return MintedToken{}, fmt.Errorf("retry bearer %q, want the one re-registered token", call.bearer)
		}
		return minted(epoch.Add(time.Hour)), nil
	})
	var wg sync.WaitGroup
	results := make(chan Credential, repos)
	for i := range repos {
		wg.Go(func() {
			results <- h.src.Credential(context.Background(), fmt.Sprintf("https://github.com/owner/repo%d", i))
		})
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got.Token() != installToken {
			t.Errorf("Credential = %v, want every repository on the token minted after one re-register", got)
		}
	}
	if n := reRegisters.Load(); n != 1 {
		t.Fatalf("re-registers = %d, want 1: each extra one revokes the token the others adopted", n)
	}
}

func TestCredentialStopsServingAfterLogout(t *testing.T) {
	t.Parallel()
	var needsLogin atomic.Bool
	h := newHarness(t, Config{
		NeedsLogin: needsLogin.Load,
		Repos:      fakeGHTokenRepos{repos: []*models.Repo{{ID: "r1", OriginURL: sshOrigin}}},
	}, func(context.Context, int, issuedCall) (MintedToken, error) {
		return minted(epoch.Add(time.Hour)), nil
	})
	ctx := context.Background()
	if got := h.src.Credential(ctx, sshOrigin); got.Token() != installToken {
		t.Fatalf("setup: Credential = %v, want the minted token", got)
	}

	needsLogin.Store(true)
	h.src.Reset()
	if got := h.src.Credential(ctx, sshOrigin); got.Identity != IdentityUser || got.Reason != ReasonNotSignedIn || got.Token() != "" {
		t.Fatalf("after logout Credential = %v, want user/not_signed_in", got)
	}
	report, err := h.src.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report[0].Identity != IdentityUser || report[0].Reason != ReasonNotSignedIn {
		t.Fatalf("after logout Report = %+v, want user/not_signed_in", report[0])
	}
	if n := h.issuer.count(); n != 1 {
		t.Fatalf("RPC calls after logout = %d, want no new mint", n)
	}

	// Signing back in mints afresh: the logged-out token was not kept.
	needsLogin.Store(false)
	if got := h.src.Credential(ctx, sshOrigin); got.Token() != installToken {
		t.Fatalf("after login Credential = %v, want a token", got)
	}
	if n := h.issuer.count(); n != 2 {
		t.Fatalf("RPC calls after login = %d, want a fresh mint", n)
	}
}

func TestResetDropsCachedTokensForAnAccountChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{}, func(_ context.Context, n int, _ issuedCall) (MintedToken, error) {
		m := minted(epoch.Add(time.Hour))
		m.Token = fmt.Sprintf("ghs_account-%d", n)
		return m, nil
	})
	ctx := context.Background()
	if got := h.src.Credential(ctx, sshOrigin); got.Token() != "ghs_account-1" {
		t.Fatalf("setup: Credential = %v, want the first account's token", got)
	}
	h.src.Reset()
	if got := h.src.Credential(ctx, sshOrigin); got.Token() != "ghs_account-2" {
		t.Fatalf("after Reset Credential token = %q, want a token minted for the new sign-in", got.Token())
	}
}

func TestCredentialMintedAcrossLogoutIsNotServed(t *testing.T) {
	t.Parallel()
	for _, change := range []struct {
		name  string
		apply func(h harness, needsLogin *atomic.Bool)
	}{
		{"logout", func(_ harness, needsLogin *atomic.Bool) { needsLogin.Store(true) }},
		{"reset", func(h harness, _ *atomic.Bool) { h.src.Reset() }},
	} {
		t.Run(change.name, func(t *testing.T) {
			t.Parallel()
			var needsLogin atomic.Bool
			entered := make(chan struct{})
			release := make(chan struct{})
			h := newHarness(t, Config{NeedsLogin: needsLogin.Load}, func(context.Context, int, issuedCall) (MintedToken, error) {
				close(entered)
				<-release
				return minted(epoch.Add(time.Hour)), nil
			})
			done := make(chan Credential, 1)
			go func() { done <- h.src.Credential(context.Background(), sshOrigin) }()
			<-entered
			change.apply(h, &needsLogin)
			close(release)
			if got := <-done; got.Identity != IdentityUser || got.Token() != "" {
				t.Fatalf("Credential = %v, want the user login: the token was minted for the previous sign-in", got)
			}
		})
	}
}

func TestInvalidateForcesAReMint(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{}, func(_ context.Context, n int, _ issuedCall) (MintedToken, error) {
		m := minted(epoch.Add(time.Hour))
		if n == 3 {
			m.Token = "ghs_reminted"
		}
		return m, nil
	})
	ctx := context.Background()
	h.src.Credential(ctx, sshOrigin)

	h.src.Invalidate(sshOrigin, "ghs_some-older-token")
	if got := h.src.Credential(ctx, sshOrigin); got.Token() != installToken || h.issuer.count() != 1 {
		t.Fatalf("a stale Invalidate changed the cache: Credential = %v after %d RPCs", got, h.issuer.count())
	}

	h.src.Invalidate(canonicalOrigin, installToken)
	// bosso answers from its own cache with the token GitHub just rejected.
	got := h.src.Credential(ctx, sshOrigin)
	if h.issuer.count() != 2 {
		t.Fatalf("RPC calls after Invalidate = %d, want a re-mint", h.issuer.count())
	}
	if got.Identity != IdentityUser || got.Reason != ReasonUpstreamError || got.Token() != "" {
		t.Fatalf("Credential with the rejected token re-issued = %v, want user/upstream_error", got)
	}
	h.src.Credential(ctx, sshOrigin)
	if n := h.issuer.count(); n != 2 {
		t.Fatalf("RPC calls inside the backoff = %d, want 2: a re-issued rejected token must not hot-loop", n)
	}

	h.clock.set(epoch.Add(errorBackoffBase))
	if got := h.src.Credential(ctx, sshOrigin); got.Token() != "ghs_reminted" {
		t.Fatalf("Credential after the backoff = %v, want the re-minted token", got)
	}
}

func TestCredentialCancelledWaiterDoesNotWaitForAnotherFetch(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	h := newHarness(t, Config{}, func(context.Context, int, issuedCall) (MintedToken, error) {
		close(entered)
		<-release
		return minted(epoch.Add(time.Hour)), nil
	})
	first := make(chan Credential, 1)
	go func() { first <- h.src.Credential(context.Background(), sshOrigin) }()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := h.src.Credential(ctx, sshOrigin); got.Identity != IdentityUser || got.Token() != "" {
		t.Fatalf("cancelled Credential = %v, want the cached user answer", got)
	}
	close(release)
	if got := <-first; got.Token() != installToken {
		t.Fatalf("fetching Credential = %v, want the minted token", got)
	}
	if n := h.issuer.count(); n != 1 {
		t.Fatalf("RPC calls = %d, want only the first caller's", n)
	}
}
