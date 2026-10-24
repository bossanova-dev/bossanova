package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const cacheTestProfile = bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1

func cacheTestSurface() runtimeOperationSurface {
	return runtimeOperationSurface{Source: codexOperationRegistrySource, Servers: []runtimeMCPServer{{Name: "linear", AuthStatus: "oAuth", Operations: realLinearConnectorOperations()}}}
}

func cacheTestWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityCacheHitAcrossSiblingWorktrees(t *testing.T) {
	srv := newTestServer(t)
	calls := 0
	srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		calls++
		return cacheTestSurface(), nil
	})
	target := codexRuntimeTarget{Home: t.TempDir(), WorkDir: t.TempDir(), ExtraEnv: map[string]string{"B": "2", "A": "1"}}
	first, err := srv.preflightHeadlessCapabilityProfile(context.Background(), cacheTestProfile, target)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating a caller-owned response must not poison later cache hits.
	first.Provided[0] = "poisoned"
	target.ExtraEnv = map[string]string{"A": "1", "B": "2"}
	for _, workdir := range []string{target.WorkDir, t.TempDir()} {
		target.WorkDir = workdir
		got, err := srv.preflightHeadlessCapabilityProfile(context.Background(), cacheTestProfile, target)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range got.Provided {
			if op == "poisoned" {
				t.Fatal("cached response aliased caller-owned slice")
			}
		}
	}
	if calls != 1 {
		t.Fatalf("registry calls = %d, want 1", calls)
	}
}

func TestCapabilityCacheFingerprintInputs(t *testing.T) {
	for _, input := range []string{"home", "model", "effort", "trust", "env-value", "env-key", "home-bytes", "worktree-bytes", "home-absent-empty", "worktree-absent-empty"} {
		t.Run(input, func(t *testing.T) {
			srv := newTestServer(t)
			calls := 0
			srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
				calls++
				return cacheTestSurface(), nil
			})
			target := codexRuntimeTarget{Home: t.TempDir(), WorkDir: t.TempDir(), Model: "initial", Effort: "low", ExtraEnv: map[string]string{"TOKEN": "initial"}}
			homeConfig := filepath.Join(target.Home, "config.toml")
			workConfig := filepath.Join(target.WorkDir, ".codex", "config.toml")
			if input == "home-bytes" {
				cacheTestWrite(t, homeConfig, "# initial")
			}
			if input == "worktree-bytes" {
				cacheTestWrite(t, workConfig, "# initial")
			}
			if _, err := srv.preflightHeadlessCapabilityProfile(context.Background(), cacheTestProfile, target); err != nil {
				t.Fatal(err)
			}
			switch input {
			case "home":
				target.Home = t.TempDir()
			case "model":
				target.Model = "changed"
			case "effort":
				target.Effort = "high"
			case "trust":
				target.ProjectTrustOverride = `projects."repo".trust_level="untrusted"`
			case "env-value":
				target.ExtraEnv["TOKEN"] = "changed"
			case "env-key":
				target.ExtraEnv["OTHER"] = "initial"
			case "home-bytes":
				cacheTestWrite(t, homeConfig, "# changed")
			case "worktree-bytes":
				cacheTestWrite(t, workConfig, "# changed")
			case "home-absent-empty":
				cacheTestWrite(t, homeConfig, "")
			case "worktree-absent-empty":
				cacheTestWrite(t, workConfig, "")
			}
			if _, err := srv.preflightHeadlessCapabilityProfile(context.Background(), cacheTestProfile, target); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("registry calls = %d, want 2 after changing %s", calls, input)
			}
		})
	}
}

func TestCapabilityCacheFailuresAreRetried(t *testing.T) {
	for _, failure := range []string{"registry", "surface"} {
		t.Run(failure, func(t *testing.T) {
			srv := newTestServer(t)
			calls := 0
			srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
				calls++
				if calls == 1 {
					if failure == "registry" {
						return runtimeOperationSurface{}, errors.New("temporarily unavailable")
					}
					return runtimeOperationSurface{Source: codexOperationRegistrySource}, nil
				}
				return cacheTestSurface(), nil
			})
			target := codexRuntimeTarget{Home: t.TempDir()}
			if _, err := srv.preflightHeadlessCapabilityProfile(context.Background(), cacheTestProfile, target); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("first error = %v, want FailedPrecondition", err)
			}
			for range 2 {
				if _, err := srv.preflightHeadlessCapabilityProfile(context.Background(), cacheTestProfile, target); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 2 {
				t.Fatalf("registry calls = %d, want failed probe plus successful probe", calls)
			}
		})
	}
}

func TestCapabilityCacheExpiry(t *testing.T) {
	srv := newTestServer(t)
	now := time.Unix(1700000000, 0)
	srv.capabilityCache.now = func() time.Time { return now }
	calls := 0
	srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		calls++
		return cacheTestSurface(), nil
	})
	target := codexRuntimeTarget{Home: t.TempDir()}
	for _, advance := range []time.Duration{0, 10*time.Minute - time.Nanosecond, time.Nanosecond} {
		now = now.Add(advance)
		if _, err := srv.preflightHeadlessCapabilityProfile(context.Background(), cacheTestProfile, target); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("registry calls = %d, want 2 at TTL boundary", calls)
	}
}

// Reporting Done evaluation lets the test hold the leader until every follower
// has reached its wait, without scheduling delays or sleeps.
type cacheWaitContext struct {
	context.Context
	waiting chan<- struct{}
	once    sync.Once
}

func (c *cacheWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { c.waiting <- struct{}{} })
	return c.Context.Done()
}

func TestCapabilityCacheConcurrentMissesAndWaitingCancellation(t *testing.T) {
	srv := newTestServer(t)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	srv.operationRegistry = runtimeOperationRegistryFunc(func(ctx context.Context, _ codexRuntimeTarget) (runtimeOperationSurface, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return cacheTestSurface(), nil
		case <-ctx.Done():
			return runtimeOperationSurface{}, ctx.Err()
		}
	})
	target := codexRuntimeTarget{Home: t.TempDir()}
	leader := make(chan error, 1)
	go func() {
		_, err := srv.preflightHeadlessCapabilityProfile(context.Background(), cacheTestProfile, target)
		leader <- err
	}()
	<-started
	baseCtx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{}, 16)
	ctx := &cacheWaitContext{Context: baseCtx, waiting: waiting}
	cancelled := make(chan error, 1)
	go func() {
		_, err := srv.preflightHeadlessCapabilityProfile(ctx, cacheTestProfile, target)
		cancelled <- err
	}()
	<-waiting
	cancel()
	select {
	case err := <-cancelled:
		if err == nil {
			t.Fatal("cancelled waiting caller succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting caller ignored cancellation")
	}
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := srv.preflightHeadlessCapabilityProfile(&cacheWaitContext{Context: context.Background(), waiting: waiting}, cacheTestProfile, target)
			errs <- err
		}()
	}
	for range n {
		<-waiting
	}
	close(release)
	wg.Wait()
	close(errs)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("registry calls = %d, want 1", got)
	}
}

func TestCapabilityCacheStillRejectsUnsupportedProfiles(t *testing.T) {
	srv := newTestServer(t)
	calls := 0
	srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		calls++
		return cacheTestSurface(), nil
	})
	target := codexRuntimeTarget{Home: t.TempDir()}
	if _, err := srv.preflightHeadlessCapabilityProfile(context.Background(), cacheTestProfile, target); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.preflightHeadlessCapabilityProfile(context.Background(), bossanovav1.HeadlessCapabilityProfile(999), target); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unsupported profile error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("unsupported profile invoked registry; calls=%d", calls)
	}
}

func TestCapabilityCacheAppliesTrustOnHit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", home)
	srv := newTestServer(t)
	calls := 0
	srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		calls++
		return cacheTestSurface(), nil
	})
	req := fullProfileRequest(home)
	req.WorkDir = t.TempDir()
	req.ProjectTrust = bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT
	if _, err := srv.PreflightHeadlessRun(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.toml")
	trusted, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cacheTestWrite(t, configPath, "")
	if _, err := srv.PreflightHeadlessRun(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(trusted) {
		t.Fatalf("trust not restored on cached preflight: got %q want %q", restored, trusted)
	}
	if calls != 1 {
		t.Fatalf("registry calls = %d, want cache hit after trust restored identical configuration", calls)
	}
}
