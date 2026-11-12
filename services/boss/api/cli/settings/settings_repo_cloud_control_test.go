package settings

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

func Test_Settings_AC237901_MachineDefaultReset(t *testing.T) {
	seedDaemonNameSettings(t)
	s, _ := config.Load()
	s.CloudControl = "full"
	if err := config.Save(s); err != nil {
		t.Fatal(err)
	}
	cmd := New(Options{}).SettingsCmd()
	cmd.SetArgs([]string{"--cloud-control", "default"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	s, _ = config.Load()
	if s.CloudControl != "" {
		t.Fatalf("machine grant remains: %q", s.CloudControl)
	}
	out, err := executeRepoSettings(t, repoControlGroup())
	if err != nil || !strings.Contains(out, "Cloud control:    full (default)") {
		t.Fatalf("default summary: %s / %v", out, err)
	}
}

type settingsRepoControlReader struct {
	repos []*pb.RepoCloudControl
	err   error
}

func (f settingsRepoControlReader) ListRepoCloudControls(context.Context) ([]*pb.RepoCloudControl, error) {
	return f.repos, f.err
}
func repoControlGroup(repos ...*pb.RepoCloudControl) *Group {
	return New(Options{NewRepoCloudControlReader: func(string) RepoCloudControlReader { return settingsRepoControlReader{repos: repos} }})
}
func registeredControlRepo(t *testing.T, name string, id int64) *pb.RepoCloudControl {
	t.Helper()
	return &pb.RepoCloudControl{Host: "github.com", Name: name, DisplayName: name, Checkout: t.TempDir(), ProviderRepoId: id, IsIdVerified: id > 0, OriginUrl: "git@github.com:" + name + ".git", EffectiveMode: "full", Source: "default"}
}
func executeRepoSettings(t *testing.T, g *Group, args ...string) (string, error) {
	t.Helper()
	cmd := g.SettingsCmd()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	return out, err
}
func Test_Settings_AC237901_RepoGrantSelectorsAndReset(t *testing.T) {
	for _, selector := range []string{"name", "display", "origin", "path"} {
		t.Run(selector, func(t *testing.T) {
			seedDaemonNameSettings(t)
			repo := registeredControlRepo(t, "acme/oss", 42)
			repo.DisplayName = "Friendly OSS"
			values := map[string]string{"name": repo.Name, "display": repo.DisplayName, "origin": repo.OriginUrl, "path": repo.Checkout}
			g := repoControlGroup(repo)
			if _, err := executeRepoSettings(t, g, "--cloud-control-repo", values[selector]+"=read-only"); err != nil {
				t.Fatal(err)
			}
			s, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			want := config.CloudControlRepoGrant{Host: "github.com", RepoID: 42, Name: repo.Name, Checkout: repo.Checkout, Mode: "read-only"}
			if len(s.CloudControlRepos) != 1 || s.CloudControlRepos[0] != want {
				t.Fatalf("grants = %#v", s.CloudControlRepos)
			}
			if _, err := executeRepoSettings(t, g, "--cloud-control-repo", values[selector]+"=default"); err != nil {
				t.Fatal(err)
			}
			s, _ = config.Load()
			if len(s.CloudControlRepos) != 0 {
				t.Fatalf("grants remain: %#v", s.CloudControlRepos)
			}
		})
	}
}
func Test_Settings_AC237901_RepoIdentityRefusalAndAtomicWrites(t *testing.T) {
	seedDaemonNameSettings(t)
	verified := registeredControlRepo(t, "acme/oss", 42)
	unknown := registeredControlRepo(t, "acme/new", 0)
	g := repoControlGroup(verified, unknown)
	for _, args := range [][]string{
		{"--cloud-control-repo", "missing/repo=full"},
		{"--cloud-control-repo", "acme/oss=private", "--cloud-control-repo", "acme/new=full"},
		{"--cloud-control-owner", "github.com/acme=full"},
		{"--cloud-control-repo", "acme/oss=invalid"},
		{"--cloud-control-repo", "acme/oss"},
	} {
		if _, err := executeRepoSettings(t, g, args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
		s, _ := config.Load()
		if len(s.CloudControlRepos) != 0 {
			t.Fatalf("partially saved %v: %#v", args, s.CloudControlRepos)
		}
	}
	if _, err := executeRepoSettings(t, g, "--cloud-control-repo", "acme/new=full"); err == nil || !strings.Contains(err.Error(), "can't verify") {
		t.Fatalf("identity error: %v", err)
	}
	if _, err := executeRepoSettings(t, g, "--cloud-control-repo", "acme/new=read-only"); err != nil {
		t.Fatal(err)
	}
	s, _ := config.Load()
	if len(s.CloudControlRepos) != 1 || s.CloudControlRepos[0].RepoID != 0 || s.CloudControlRepos[0].Mode != "read-only" {
		t.Fatalf("narrow grant = %#v", s.CloudControlRepos)
	}
}
func Test_Settings_AC237901_OwnerGrantsAndWarnings(t *testing.T) {
	seedDaemonNameSettings(t)
	first, second, other := registeredControlRepo(t, "acme/a", 1), registeredControlRepo(t, "acme/b", 2), registeredControlRepo(t, "other/c", 3)
	g := repoControlGroup(first, second, other)
	out, err := executeRepoSettings(t, g, "--cloud-control-owner", "github.com/acme=full")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, config.FullCloudControlWarning) {
		t.Fatalf("warnings missing: %s", out)
	}
	s, _ := config.Load()
	if len(s.CloudControlRepos) != 2 {
		t.Fatalf("grants = %#v", s.CloudControlRepos)
	}
	for _, grant := range s.CloudControlRepos {
		if grant.Mode != "full" || !strings.HasPrefix(grant.Name, "acme/") || grant.RepoID == 0 {
			t.Fatalf("grant = %#v", grant)
		}
	}
	if _, err := executeRepoSettings(t, g, "--cloud-control-owner", "github.com/acme=default"); err != nil {
		t.Fatal(err)
	}
	s, _ = config.Load()
	if len(s.CloudControlRepos) != 0 {
		t.Fatalf("grants remain: %#v", s.CloudControlRepos)
	}
	for _, args := range [][]string{{"--cloud-control", "full"}, {"--cloud-control-repo", "acme/a=full"}} {
		out, err := executeRepoSettings(t, g, args...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, config.FullCloudControlWarning) {
			t.Fatalf("warnings missing: %s", out)
		}
	}
}
func Test_Settings_AC237901_GroupedRepoSummary(t *testing.T) {
	seedDaemonNameSettings(t)
	first, other := registeredControlRepo(t, "acme/a", 1), registeredControlRepo(t, "other/b", 2)
	first.EffectiveMode = "private"
	first.Source = "grant-narrowed"
	first.HasUnverifiedGrant = true
	out, err := executeRepoSettings(t, repoControlGroup(other, first))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Cloud control:    full (default)", "Cloud control per repo:", "github.com/acme", "acme/a: private (grant-narrowed)", "grant not verified: narrowing only", "github.com/other", "other/b: full (default)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary lacks %q: %s", want, out)
		}
	}
	if strings.Index(out, "github.com/acme") > strings.Index(out, "github.com/other") {
		t.Fatalf("groups not sorted: %s", out)
	}
}
func TestSettingsRepoAmbiguityAndDaemonFailure(t *testing.T) {
	seedDaemonNameSettings(t)
	a, b := registeredControlRepo(t, "acme/a", 1), registeredControlRepo(t, "other/b", 2)
	a.DisplayName = "Shared"
	b.DisplayName = "Shared"
	if _, err := executeRepoSettings(t, repoControlGroup(a, b), "--cloud-control-repo", "Shared=full"); err == nil {
		t.Fatal("ambiguous selector accepted")
	}
	if _, err := executeRepoSettings(t, repoControlGroup(a, b), "--cloud-control-repo", a.Checkout+"=full"); err != nil {
		t.Fatal(err)
	}
	g := New(Options{NewRepoCloudControlReader: func(string) RepoCloudControlReader { return settingsRepoControlReader{err: errors.New("unavailable")} }})
	if _, err := executeRepoSettings(t, g, "--cloud-control-repo", "acme/a=full"); err == nil {
		t.Fatal("daemon failure accepted")
	}
	if _, err := executeRepoSettings(t, g); err != nil {
		t.Fatalf("machine settings must remain readable: %v", err)
	}
}

func Test_Settings_AC243002_RepoSelectorPathWithEquals(t *testing.T) {
	seedDaemonNameSettings(t)
	repo := registeredControlRepo(t, "acme/oss", 42)
	repo.Checkout = filepath.Join(t.TempDir(), "a=b")
	if err := os.Mkdir(repo.Checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	g := repoControlGroup(repo)
	if _, err := executeRepoSettings(t, g, "--cloud-control-repo", repo.Checkout+"=read-only"); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := config.CloudControlRepoGrant{Host: repo.Host, RepoID: 42, Name: repo.Name, Checkout: repo.Checkout, Mode: "read-only"}
	if len(s.CloudControlRepos) != 1 || s.CloudControlRepos[0] != want {
		t.Fatalf("grants = %#v, want %#v", s.CloudControlRepos, want)
	}
	if _, err := executeRepoSettings(t, g, "--cloud-control-repo", repo.Checkout+"=default"); err != nil {
		t.Fatal(err)
	}
	s, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.CloudControlRepos) != 0 {
		t.Fatalf("grants remain: %#v", s.CloudControlRepos)
	}
}

func Test_Settings_AC243001_OwnerIdentityErrorNamesRepo(t *testing.T) {
	for _, displayName := range []string{"Friendly New", ""} {
		t.Run(displayName, func(t *testing.T) {
			seedDaemonNameSettings(t)
			verified := registeredControlRepo(t, "acme/oss", 42)
			unknown := registeredControlRepo(t, "acme/new", 0)
			unknown.DisplayName = displayName
			_, err := executeRepoSettings(t, repoControlGroup(verified, unknown), "--cloud-control-owner", "github.com/acme=full")
			if err == nil {
				t.Fatal("unverified full grant accepted")
			}
			label := displayName
			if label == "" {
				label = unknown.Name
			}
			for _, want := range []string{label, unknown.Checkout, "can't verify this repo's identity with GitHub; try again"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("identity error %q lacks %q", err, want)
				}
			}
			s, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(s.CloudControlRepos) != 0 {
				t.Fatalf("partially saved grants: %#v", s.CloudControlRepos)
			}
		})
	}
}

func Test_Settings_AC238301_RestrictPublicRepos(t *testing.T) {
	for _, owner := range []string{"", "github.com/acme"} {
		t.Run(owner, func(t *testing.T) {
			seedDaemonNameSettings(t)
			a, b, c := registeredControlRepo(t, "acme/a", 1), registeredControlRepo(t, "other/b", 2), registeredControlRepo(t, "acme/private", 3)
			a.IsSuggested, b.IsSuggested = true, true
			g := repoControlGroup(a, b, c)
			args := []string{"--restrict-public-repos", "--yes"}
			if owner != "" {
				args = append(args, "--owner", owner)
			}
			if _, err := executeRepoSettings(t, g, args...); err != nil {
				t.Fatal(err)
			}
			settings, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if owner != "" {
				want = 1
			}
			if len(settings.CloudControlRepos) != want {
				t.Fatalf("grants: %+v", settings.CloudControlRepos)
			}
			for _, grant := range settings.CloudControlRepos {
				if grant.Mode != "read-only" || grant.RepoID == 0 || grant.Checkout == "" || grant.Host != "github.com" {
					t.Fatalf("unbound grant: %+v", grant)
				}
				if owner != "" && !strings.HasPrefix(grant.Name, "acme/") {
					t.Fatalf("wrong owner: %+v", grant)
				}
			}
		})
	}
}
func TestPublicRestrictionConfirmationAndEmpty(t *testing.T) {
	seedDaemonNameSettings(t)
	repo := registeredControlRepo(t, "acme/a", 1)
	repo.IsSuggested = true
	for _, answer := range []string{"n\n", "y\n"} {
		cmd := repoControlGroup(repo).SettingsCmd()
		cmd.SetArgs([]string{"--restrict-public-repos"})
		cmd.SetIn(strings.NewReader(answer))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		settings, _ := config.Load()
		if (len(settings.CloudControlRepos) == 1) != (answer == "y\n") {
			t.Fatalf("answer %q: %+v", answer, settings.CloudControlRepos)
		}
	}
	out, err := executeRepoSettings(t, repoControlGroup(), "--restrict-public-repos", "--yes")
	if err != nil || !strings.Contains(out, "No public repos to restrict") {
		t.Fatalf("empty: %s %v", out, err)
	}
}
