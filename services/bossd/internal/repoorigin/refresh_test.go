package repoorigin

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/recurser/bossd/internal/domain"
)

type remotes map[string]string

func (r remotes) DetectOriginURL(_ context.Context, path string) (string, error) {
	if path == "/broken" {
		return "", errors.New("git exploded")
	}
	return r[path], nil
}

type repoRows struct {
	rows      map[string]*domain.Repo
	updateErr error
	updates   int
}

func newRepoRows(repos ...*domain.Repo) *repoRows {
	s := &repoRows{rows: map[string]*domain.Repo{}}
	for _, r := range repos {
		c := *r
		s.rows[r.ID] = &c
	}
	return s
}

func (s *repoRows) List(context.Context) ([]*domain.Repo, error) {
	out := make([]*domain.Repo, 0, len(s.rows))
	for _, id := range []string{"a", "b", "c", "d"} {
		if r, ok := s.rows[id]; ok {
			c := *r
			out = append(out, &c)
		}
	}
	return out, nil
}

func (s *repoRows) Update(_ context.Context, id string, p domain.UpdateRepoParams) (*domain.Repo, error) {
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	s.updates++
	if p.OriginURL != nil {
		s.rows[id].OriginURL = *p.OriginURL
	}
	c := *s.rows[id]
	return &c, nil
}

const (
	oldOrigin = "https://github.com/old-owner/repo.git"
	newOrigin = "https://github.com/new-owner/repo.git"
)

func TestRefreshPersistsRenamedRemote(t *testing.T) {
	store := newRepoRows(&domain.Repo{ID: "a", LocalPath: "/a", OriginURL: oldOrigin})
	repo := *store.rows["a"]
	var logs bytes.Buffer

	previous, err := Refresh(context.Background(), remotes{"/a": newOrigin + "\n"}, store, &repo, zerolog.New(&logs))
	if err != nil {
		t.Fatal(err)
	}
	if previous != oldOrigin || repo.OriginURL != newOrigin || store.rows["a"].OriginURL != newOrigin {
		t.Fatalf("previous=%q repo=%q stored=%q", previous, repo.OriginURL, store.rows["a"].OriginURL)
	}
	if got := strings.Count(logs.String(), `"level":"info"`); got != 1 || !strings.Contains(logs.String(), oldOrigin) || !strings.Contains(logs.String(), newOrigin) {
		t.Fatalf("want one info line naming both origins, got %s", logs.String())
	}

	logs.Reset()
	if _, err := Refresh(context.Background(), remotes{"/a": newOrigin}, store, &repo, zerolog.New(&logs)); err != nil {
		t.Fatal(err)
	}
	if store.updates != 1 || logs.Len() != 0 {
		t.Fatalf("second refresh wrote %d times and logged %q; want no write and no log", store.updates, logs.String())
	}
}

func TestRefreshStripsRemoteCredentials(t *testing.T) {
	secret := "s3cret-" + "pass"
	store := newRepoRows(&domain.Repo{ID: "a", LocalPath: "/a", OriginURL: oldOrigin})
	repo := *store.rows["a"]
	var logs bytes.Buffer

	if _, err := Refresh(context.Background(), remotes{"/a": "https://alice:" + secret + "@github.com/new-owner/repo.git"}, store, &repo, zerolog.New(&logs)); err != nil {
		t.Fatal(err)
	}
	if store.rows["a"].OriginURL != newOrigin {
		t.Fatalf("stored = %q, want %q", store.rows["a"].OriginURL, newOrigin)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("log carries the remote's password")
	}

	store.updates = 0
	if _, err := Refresh(context.Background(), remotes{"/a": "https://alice:" + secret + "@github.com/new-owner/repo.git"}, store, &repo, zerolog.Nop()); err != nil || store.updates != 0 {
		t.Fatalf("a credentialed remote equal to the stored origin wrote %d times (err %v); want none", store.updates, err)
	}
}

func TestRefreshKeepsStoredOriginWithoutRemote(t *testing.T) {
	for name, remote := range map[string]string{"missing": "", "blank": "  \n"} {
		t.Run(name, func(t *testing.T) {
			store := newRepoRows(&domain.Repo{ID: "a", LocalPath: "/a", OriginURL: oldOrigin})
			repo := *store.rows["a"]
			previous, err := Refresh(context.Background(), remotes{"/a": remote}, store, &repo, zerolog.Nop())
			if !errors.Is(err, ErrNoRemote) {
				t.Fatalf("err = %v, want ErrNoRemote", err)
			}
			if previous != oldOrigin || repo.OriginURL != oldOrigin || store.updates != 0 {
				t.Fatalf("previous=%q repo=%q writes=%d; want the stored origin untouched", previous, repo.OriginURL, store.updates)
			}
		})
	}
}

func TestRefreshKeepsHostedOriginOverUnhostedRemote(t *testing.T) {
	store := newRepoRows(&domain.Repo{ID: "a", LocalPath: "/a", OriginURL: oldOrigin})
	repo := *store.rows["a"]
	_, err := Refresh(context.Background(), remotes{"/a": "/tmp/fixture/origin.git"}, store, &repo, zerolog.Nop())
	if !errors.Is(err, ErrUnhostedRemote) || store.updates != 0 || repo.OriginURL != oldOrigin {
		t.Fatalf("err=%v writes=%d repo=%q; want ErrUnhostedRemote and the hosted origin kept", err, store.updates, repo.OriginURL)
	}
}

func TestRefreshFollowsUnhostedRemoteWhenNothingHostedIsStored(t *testing.T) {
	for name, stored := range map[string]string{"empty": "", "local path": "/old/origin.git"} {
		t.Run(name, func(t *testing.T) {
			store := newRepoRows(&domain.Repo{ID: "a", LocalPath: "/a", OriginURL: stored})
			repo := *store.rows["a"]
			if _, err := Refresh(context.Background(), remotes{"/a": "/new/origin.git"}, store, &repo, zerolog.Nop()); err != nil {
				t.Fatal(err)
			}
			if store.rows["a"].OriginURL != "/new/origin.git" {
				t.Fatalf("stored = %q, want the local remote", store.rows["a"].OriginURL)
			}
		})
	}
}

func TestRefreshFillsEmptyStoredOrigin(t *testing.T) {
	store := newRepoRows(&domain.Repo{ID: "a", LocalPath: "/a"})
	repo := *store.rows["a"]
	previous, err := Refresh(context.Background(), remotes{"/a": newOrigin}, store, &repo, zerolog.Nop())
	if err != nil || previous != "" || store.rows["a"].OriginURL != newOrigin {
		t.Fatalf("previous=%q stored=%q err=%v", previous, store.rows["a"].OriginURL, err)
	}
}

func TestRefreshLeavesRepoOnPersistFailure(t *testing.T) {
	store := newRepoRows(&domain.Repo{ID: "a", LocalPath: "/a", OriginURL: oldOrigin})
	store.updateErr = domain.ErrAmbiguousOrigin
	repo := *store.rows["a"]
	_, err := Refresh(context.Background(), remotes{"/a": newOrigin}, store, &repo, zerolog.Nop())
	if !errors.Is(err, domain.ErrAmbiguousOrigin) || repo.OriginURL != oldOrigin {
		t.Fatalf("err=%v repo=%q; want the store error and the stored origin kept", err, repo.OriginURL)
	}
}

func TestRefreshAllRefreshesEachRepoIndependently(t *testing.T) {
	store := newRepoRows(
		&domain.Repo{ID: "a", LocalPath: "/broken", OriginURL: oldOrigin},
		&domain.Repo{ID: "b", LocalPath: "/b", OriginURL: oldOrigin},
		&domain.Repo{ID: "c", LocalPath: "/c", OriginURL: "https://github.com/x/c.git"},
		&domain.Repo{ID: "d", LocalPath: "/gone", OriginURL: "https://github.com/x/d.git"},
	)
	RefreshAll(context.Background(), remotes{"/b": newOrigin, "/c": "https://github.com/x/c.git"}, store, zerolog.Nop())

	want := map[string]string{"a": oldOrigin, "b": newOrigin, "c": "https://github.com/x/c.git", "d": "https://github.com/x/d.git"}
	for id, origin := range want {
		if got := store.rows[id].OriginURL; got != origin {
			t.Errorf("repo %s origin = %q, want %q", id, got, origin)
		}
	}
	if store.updates != 1 {
		t.Errorf("writes = %d, want 1", store.updates)
	}
}

func TestRefreshFollowsARealRenamedRemote(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", oldOrigin},
		{"remote", "set-url", "origin", newOrigin},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	store := newRepoRows(&domain.Repo{ID: "a", LocalPath: dir, OriginURL: oldOrigin})

	RefreshAll(context.Background(), gitpkg.NewManager(zerolog.Nop()), store, zerolog.Nop())

	if got := store.rows["a"].OriginURL; got != newOrigin {
		t.Fatalf("stored origin = %q, want %q", got, newOrigin)
	}
}

// hangingRemotes blocks on /hang until the caller's context ends.
type hangingRemotes struct{ remotes }

func (h hangingRemotes) DetectOriginURL(ctx context.Context, path string) (string, error) {
	if path == "/hang" {
		<-ctx.Done()
		return "", nil
	}
	return h.remotes.DetectOriginURL(ctx, path)
}

func TestRefreshAllBoundsEachRepo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newRepoRows(
			&domain.Repo{ID: "a", LocalPath: "/hang", OriginURL: oldOrigin},
			&domain.Repo{ID: "b", LocalPath: "/b", OriginURL: oldOrigin},
		)
		var logs bytes.Buffer
		RefreshAll(context.Background(), hangingRemotes{remotes{"/b": newOrigin}}, store, zerolog.New(&logs))

		if store.rows["a"].OriginURL != oldOrigin || store.rows["b"].OriginURL != newOrigin {
			t.Fatalf("origins a=%q b=%q; want a kept and b refreshed after a's timeout", store.rows["a"].OriginURL, store.rows["b"].OriginURL)
		}
		if !strings.Contains(logs.String(), "timed out") {
			t.Fatalf("a hung repo was not reported as timed out:\n%s", logs.String())
		}
	})
}

func TestRefreshAllReportsReposLeftUnchecked(t *testing.T) {
	store := newRepoRows(&domain.Repo{ID: "a", LocalPath: "/a", OriginURL: oldOrigin}, &domain.Repo{ID: "b", LocalPath: "/b", OriginURL: oldOrigin})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var logs bytes.Buffer
	RefreshAll(ctx, remotes{"/a": newOrigin, "/b": newOrigin}, store, zerolog.New(&logs))

	if store.updates != 0 || !strings.Contains(logs.String(), `"unchecked":2`) {
		t.Fatalf("writes=%d logs=%s; want no writes and a warning naming 2 unchecked repos", store.updates, logs.String())
	}
}
