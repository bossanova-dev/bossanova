package repoidentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
)

type identityReaderFunc func(context.Context, string) (int64, string, error)

func (f identityReaderFunc) GetRepositoryIdentity(ctx context.Context, repo string) (int64, string, error) {
	return f(ctx, repo)
}

func TestIdentityExpiryAndFailures(t *testing.T) {
	now := time.Now()
	var calls int
	var failure error
	v := New(identityReaderFunc(func(context.Context, string) (int64, string, error) { calls++; return 42, "private", failure }), zerolog.Nop())
	v.now = func() time.Time { return now }
	repo := models.Repo{OriginURL: "git@github.com:Acme/Repo.git", LocalPath: t.TempDir()}
	first := v.Identity(context.Background(), repo)
	if !first.IsIDVerified || first.ProviderRepoID != 42 || first.Host != "github.com" || first.Name != "acme/repo" || first.LocalPath != repo.LocalPath {
		t.Fatalf("identity: %+v", first)
	}
	now = now.Add(9 * time.Minute)
	if !v.Identity(context.Background(), repo).IsIDVerified || calls != 1 {
		t.Fatal("cache did not retain absolute lifetime")
	}
	now = now.Add(time.Minute)
	for _, err := range []error{errors.New("404"), errors.New("unavailable"), context.DeadlineExceeded} {
		failure = err
		if got := v.Identity(context.Background(), repo); got.IsIDVerified || got.ProviderRepoID != 0 {
			t.Fatalf("stale on failure: %+v", got)
		}
	}
	failure = nil
	if !v.Identity(context.Background(), repo).IsIDVerified {
		t.Fatal("fresh recovery failed")
	}
	for _, origin := range []string{"https://enterprise.example/acme/repo", "", "https://github.com.evil/acme/repo"} {
		repo.OriginURL = origin
		if v.Identity(context.Background(), repo).IsIDVerified {
			t.Fatalf("verified unsupported host %s", origin)
		}
	}
}

func TestLastVerifiedIdentityRetainsIdleGrantMatching(t *testing.T) {
	now := time.Now()
	var reads int
	var failure error
	v := New(identityReaderFunc(func(context.Context, string) (int64, string, error) {
		reads++
		return 42, "private", failure
	}), zerolog.Nop())
	v.now = func() time.Time { return now }
	repo := models.Repo{OriginURL: "https://github.com/o/r", LocalPath: t.TempDir()}

	if got := v.LastVerifiedIdentity(repo); got.IsIDVerified || got.Name != "o/r" || got.LocalPath != repo.LocalPath || reads != 0 {
		t.Fatalf("cold cache returned %+v with %d provider reads", got, reads)
	}
	v.Identity(t.Context(), repo)
	if got := v.LastVerifiedIdentity(repo); !got.IsIDVerified || got.ProviderRepoID != 42 || reads != 1 {
		t.Fatalf("warm cache returned %+v with %d provider reads", got, reads)
	}
	now = now.Add(identityTTL)
	if got := v.LastVerifiedIdentity(repo); !got.IsIDVerified || got.ProviderRepoID != 42 || reads != 1 {
		t.Fatalf("expired cache returned %+v with %d provider reads", got, reads)
	}
	failure = errors.New("provider unavailable")
	if got := v.Identity(t.Context(), repo); got.IsIDVerified || reads != 2 {
		t.Fatalf("active lookup reused expired verification: %+v, %d reads", got, reads)
	}
	if got := v.LastVerifiedIdentity(repo); got.ProviderRepoID != 42 || reads != 2 {
		t.Fatalf("idle policy lost its matching identity: %+v, %d reads", got, reads)
	}
}

func TestIdentityTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v := New(identityReaderFunc(func(ctx context.Context, _ string) (int64, string, error) {
			<-ctx.Done()
			return 42, "private", ctx.Err()
		}), zerolog.Nop())
		if got := v.Identity(context.Background(), models.Repo{OriginURL: "https://github.com/o/r"}); got.IsIDVerified || got.ProviderRepoID != 0 {
			t.Fatalf("timeout: %+v", got)
		}
	})
}

func TestIdentityCollapsesCanonicalOrigins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		v := New(identityReaderFunc(func(context.Context, string) (int64, string, error) {
			calls.Add(1)
			<-release
			return 42, "private", nil
		}), zerolog.Nop())
		var wg sync.WaitGroup
		for i := range 12 {
			wg.Go(func() {
				origin := "https://github.com/o/r"
				if i%2 == 0 {
					origin = "git@github.com:O/R.git"
				}
				if !v.Identity(context.Background(), models.Repo{OriginURL: origin}).IsIDVerified {
					t.Error("unverified")
				}
			})
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		if calls.Load() != 1 {
			t.Fatalf("reads: %d", calls.Load())
		}
	})
}

func TestIdentityWaitersAndBackgroundCache_Settings_AC242903(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		v := New(identityReaderFunc(func(ctx context.Context, _ string) (int64, string, error) {
			calls.Add(1)
			select {
			case <-time.After(2 * time.Second):
				return 42, "private", nil
			case <-ctx.Done():
				return 0, "", ctx.Err()
			}
		}), zerolog.Nop())
		repo := models.Repo{OriginURL: "https://github.com/o/r"}
		short, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		shortDone := make(chan bool, 1)
		go func() { shortDone <- v.Identity(short, repo).IsIDVerified }()
		synctest.Wait()
		longDone := make(chan bool, 1)
		go func() { longDone <- v.Identity(context.Background(), repo).IsIDVerified }()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case verified := <-shortDone:
			if verified {
				t.Fatal("expired caller verified")
			}
		default:
			t.Fatal("short caller inherited flight deadline")
		}
		if short.Err() != context.DeadlineExceeded {
			t.Fatal(short.Err())
		}
		if !<-longDone {
			t.Fatal("long caller failed to receive identity")
		}
		if !v.Identity(context.Background(), repo).IsIDVerified || calls.Load() != 1 {
			t.Fatalf("cache did not warm: reads=%d", calls.Load())
		}
	})
}

func TestIdentityAbandonedFlightWarmsCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		v := New(identityReaderFunc(func(ctx context.Context, _ string) (int64, string, error) {
			calls.Add(1)
			select {
			case <-time.After(2 * time.Second):
				return 42, "private", nil
			case <-ctx.Done():
				return 0, "", ctx.Err()
			}
		}), zerolog.Nop())
		repo := models.Repo{OriginURL: "https://github.com/o/r"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		started := time.Now()
		if v.Identity(ctx, repo).IsIDVerified {
			t.Fatal("expired caller verified")
		}
		if elapsed := time.Since(started); elapsed != time.Second {
			t.Fatalf("caller elapsed %v", elapsed)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if !v.Identity(context.Background(), repo).IsIDVerified || calls.Load() != 1 {
			t.Fatalf("abandoned flight did not warm cache: reads=%d", calls.Load())
		}
	})
}

func TestVisibilityIsFreshInformationalMetadata(t *testing.T) {
	now := time.Now()
	visibility := "public"
	var failure error
	v := New(identityReaderFunc(func(context.Context, string) (int64, string, error) { return 42, visibility, failure }), zerolog.Nop())
	v.now = func() time.Time { return now }
	repo := models.Repo{OriginURL: "https://github.com/acme/repo"}
	for _, tc := range []struct{ input, want string }{{"public", "public"}, {"internal", "public"}, {"private", "private"}, {"", "unknown"}, {"unexpected", "unknown"}} {
		visibility = tc.input
		now = now.Add(identityTTL)
		if got := v.Identity(t.Context(), repo); got.Visibility != tc.want || !got.IsIDVerified {
			t.Fatalf("%q: %+v", tc.input, got)
		}
	}
	visibility = "public"
	now = now.Add(identityTTL)
	v.Identity(t.Context(), repo)
	failure = errors.New("404")
	now = now.Add(identityTTL)
	if got := v.Identity(t.Context(), repo); got.Visibility != "unknown" || got.IsIDVerified {
		t.Fatalf("stale visibility: %+v", got)
	}
	for _, origin := range []string{"", "https://enterprise.example/acme/repo", "https://gitlab.com/acme/repo"} {
		repo.OriginURL = origin
		if got := v.Identity(t.Context(), repo); got.Visibility != "unknown" {
			t.Fatalf("unsupported: %+v", got)
		}
	}
}
