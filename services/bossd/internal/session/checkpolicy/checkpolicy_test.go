package checkpolicy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type policyRepoTable struct {
	repos []*domain.Repo
}

func (s policyRepoTable) GetByLocalPath(_ context.Context, localPath string) (*domain.Repo, error) {
	for _, r := range s.repos {
		if r.LocalPath == localPath {
			return r, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s policyRepoTable) GetByOriginURL(_ context.Context, originURL string) (*domain.Repo, error) {
	for _, r := range s.repos {
		if r.OriginURL == originURL {
			return r, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s policyRepoTable) List(context.Context) ([]*domain.Repo, error) { return s.repos, nil }

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const validConfig = `{"ci":{"required":{"checks":["go-test","scripts"],"exemptBases":["staging"]}}}`

func TestResolverReferencesResolveToSameRepo(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, validConfig)
	repos := policyRepoTable{repos: []*domain.Repo{
		{ID: "other", LocalPath: t.TempDir(), OriginURL: "git@github.com:acme/other.git"},
		{ID: "r1", LocalPath: dir, OriginURL: "git@github.com:Acme/Widgets.git"},
	}}
	r := New(repos, zerolog.Nop())
	want := []string{"go-test", "scripts"}
	for _, ref := range []string{dir, "git@github.com:Acme/Widgets.git", "https://github.com/acme/widgets", "acme/widgets"} {
		got := r.RequiredChecks(context.Background(), ref)
		if !reflect.DeepEqual(got.Names, want) || !reflect.DeepEqual(got.ExemptBases, []string{"staging"}) {
			t.Errorf("RequiredChecks(%q) = %+v, want names %v", ref, got, want)
		}
	}
	if got := r.RequiredChecks(context.Background(), "acme/unknown"); len(got.Names) != 0 {
		t.Errorf("unknown repo = %+v, want no requirement", got)
	}
}

func TestResolverMissingFileOrKeyIsNoRequirement(t *testing.T) {
	missing := t.TempDir()
	noKey := t.TempDir()
	writeConfig(t, noKey, `{"adapters":{"tracker":"linear"}}`)
	emptyChecks := t.TempDir()
	writeConfig(t, emptyChecks, `{"ci":{"required":{"checks":[]}}}`)
	r := New(policyRepoTable{}, zerolog.Nop())
	for name, dir := range map[string]string{"missing file": missing, "missing key": noKey, "empty checks": emptyChecks} {
		if got := r.RequiredChecks(context.Background(), dir); len(got.Names) != 0 || got.For("main") != nil {
			t.Errorf("%s: got %+v, want no requirement", name, got)
		}
	}
}

func TestResolverMalformedLogsOncePerVersion(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"ci":{"required":{"checks":"go-test"}}}`)
	var logs bytes.Buffer
	r := New(policyRepoTable{}, zerolog.New(&logs))
	for range 3 {
		if got := r.RequiredChecks(context.Background(), dir); len(got.Names) != 0 {
			t.Fatalf("malformed block = %+v, want no requirement", got)
		}
	}
	if n := strings.Count(logs.String(), "malformed ci.required"); n != 1 {
		t.Fatalf("warnings = %d, want 1: %s", n, logs.String())
	}

	writeConfig(t, dir, `{not json`)
	bumpModTime(t, dir)
	r.RequiredChecks(context.Background(), dir)
	r.RequiredChecks(context.Background(), dir)
	if n := strings.Count(logs.String(), "malformed ci.required"); n != 2 {
		t.Fatalf("warnings after edit = %d, want 2", n)
	}
}

func TestResolverReloadsOnModTimeChange(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"ci":{"required":{"checks":["go-test"]}}}`)
	r := New(policyRepoTable{}, zerolog.Nop())
	if got := r.RequiredChecks(context.Background(), dir).Names; !reflect.DeepEqual(got, []string{"go-test"}) {
		t.Fatalf("first read = %v", got)
	}
	// Same size, new content: only the modification time tells them apart.
	writeConfig(t, dir, `{"ci":{"required":{"checks":["scripts"]}}}`)
	bumpModTime(t, dir)
	if got := r.RequiredChecks(context.Background(), dir).Names; !reflect.DeepEqual(got, []string{"scripts"}) {
		t.Fatalf("after edit = %v, want [scripts]", got)
	}
}

func TestNilResolverRequiresNothing(t *testing.T) {
	var r *Resolver
	if got := r.RequiredChecks(context.Background(), "/repo"); len(got.Names) != 0 {
		t.Fatalf("nil resolver = %+v", got)
	}
}

func bumpModTime(t *testing.T, dir string) {
	t.Helper()
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, configFile), future, future); err != nil {
		t.Fatal(err)
	}
}
