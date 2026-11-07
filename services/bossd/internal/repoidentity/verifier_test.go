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
)

type identityReaderFunc func(context.Context, string) (int64, string, error)

func (f identityReaderFunc) GetRepositoryIdentity(ctx context.Context, repo string) (int64, string, error) {
	return f(ctx, repo)
}

func TestIdentityExpiryAndFailures(t *testing.T) {
	now := time.Now()
	var calls int
	var failure error
	v := New(identityReaderFunc(func(context.Context, string) (int64, string, error) { calls++; return 42, "private", failure }))
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

func TestIdentityTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v := New(identityReaderFunc(func(ctx context.Context, _ string) (int64, string, error) {
			<-ctx.Done()
			return 42, "private", ctx.Err()
		}))
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
		}))
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
