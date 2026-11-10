package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepoCloudControlChoice(t *testing.T) {
	checkout := t.TempDir()
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	id := RepoIdentity{Host: "github.com", Name: "acme/new", LocalPath: checkout, ProviderRepoID: 42, IsIDVerified: true}
	grant := CloudControlRepoGrant{Host: id.Host, Name: "acme/old", Checkout: checkout, RepoID: 42, Mode: "full"}
	symlinked, loose, other, narrow, raw, unbound := grant, grant, grant, grant, grant, grant
	symlinked.Checkout = link
	loose.RepoID, loose.Mode = 0, "read-only"
	other.Checkout = t.TempDir()
	narrow.Mode = "private"
	raw.Mode = "unknown"
	unbound.Checkout, unbound.Mode = "", "private"
	for _, tt := range []struct {
		name   string
		grants []CloudControlRepoGrant
		want   string
	}{
		{name: "renamed strict grant", grants: []CloudControlRepoGrant{grant}, want: "full"},
		{name: "symlinked strict grant", grants: []CloudControlRepoGrant{symlinked}, want: "full"},
		{name: "loose unverified grant", grants: []CloudControlRepoGrant{loose}, want: "read-only"},
		{name: "another checkout", grants: []CloudControlRepoGrant{other}, want: "default"},
		{name: "strict grants fold", grants: []CloudControlRepoGrant{grant, narrow}, want: "private"},
		{name: "strict takes priority over loose", grants: []CloudControlRepoGrant{loose, grant}, want: "full"},
		{name: "replacement retains raw mode", grants: []CloudControlRepoGrant{raw}, want: "unknown"},
		{name: "first replacement wins", grants: []CloudControlRepoGrant{raw, loose}, want: "unknown"},
		{name: "unbound identity replacement", grants: []CloudControlRepoGrant{unbound}, want: "private"},
		{name: "no grants", want: "default"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := Settings{CloudControlRepos: tt.grants}
			if got := RepoCloudControlChoice(s, id); got != tt.want {
				t.Fatalf("choice = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveRepoCloudControlRenamedAndSymlinked(t *testing.T) {
	checkout := t.TempDir()
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	id := RepoIdentity{Host: "github.com", Name: "acme/new", LocalPath: checkout, ProviderRepoID: 42, IsIDVerified: true}
	for _, path := range []string{checkout, link} {
		t.Run(path, func(t *testing.T) {
			s := Settings{CloudControl: "private", CloudControlRepos: []CloudControlRepoGrant{
				{Host: id.Host, Name: "acme/old", Checkout: path, RepoID: 42, Mode: "full"},
			}}
			if mode, source := ResolveRepoCloudControl(s, id); mode != CloudControlFull || source != CloudControlSourceGrant {
				t.Fatalf("resolved %s (%s), want full (grant)", mode, source)
			}
		})
	}
}

func TestSetRepoCloudControlIdentity(t *testing.T) {
	id := RepoIdentity{Host: "github.com", Name: "acme/repo", LocalPath: t.TempDir()}
	s := Settings{}
	if err := SetRepoCloudControl(&s, id, "full"); err == nil {
		t.Fatal("unverified widening accepted")
	}
	if err := SetRepoCloudControl(&s, id, "read-only"); err != nil {
		t.Fatal(err)
	}
	if !HasUnverifiedRepoGrant(s, id) {
		t.Fatal("unverified grant not reported")
	}
	id.IsIDVerified = true
	id.ProviderRepoID = 42
	if err := SetRepoCloudControl(&s, id, "full"); err != nil {
		t.Fatal(err)
	}
	if len(s.CloudControlRepos) != 1 || s.CloudControlRepos[0].RepoID != 42 || HasUnverifiedRepoGrant(s, id) {
		t.Fatalf("grant: %+v", s)
	}
	if mode, source := ResolveRepoCloudControl(s, id); mode != CloudControlFull || source != CloudControlSourceGrant {
		t.Fatalf("mode=%s source=%s", mode, source)
	}
	if err := SetRepoCloudControl(&s, id, "default"); err != nil || len(s.CloudControlRepos) != 0 {
		t.Fatalf("reset: %+v %v", s, err)
	}
}

func TestSetRepoCloudControlPreservesOtherCheckouts(t *testing.T) {
	for _, mode := range []string{"default", "private", "read-only", "full"} {
		t.Run(mode, func(t *testing.T) {
			other := RepoIdentity{Host: "github.com", Name: "acme/repo", ProviderRepoID: 42, IsIDVerified: true, LocalPath: t.TempDir()}
			current := other
			current.LocalPath = t.TempDir()
			s := Settings{CloudControl: "full"}
			if err := SetRepoCloudControl(&s, other, "private"); err != nil {
				t.Fatal(err)
			}
			otherGrant := s.CloudControlRepos[0]
			if err := SetRepoCloudControl(&s, current, mode); err != nil {
				t.Fatal(err)
			}
			if len(s.CloudControlRepos) == 0 || s.CloudControlRepos[0] != otherGrant {
				t.Fatalf("other checkout grant changed: %+v", s.CloudControlRepos)
			}
			if got, _ := ResolveRepoCloudControl(s, other); got != CloudControlPrivate {
				t.Fatalf("other checkout authority widened to %s", got)
			}
			if err := SetRepoCloudControl(&s, current, "default"); err != nil {
				t.Fatal(err)
			}
			if len(s.CloudControlRepos) != 1 || s.CloudControlRepos[0] != otherGrant {
				t.Fatalf("reset changed other checkout: %+v", s.CloudControlRepos)
			}
		})
	}
}

func TestSetRepoCloudControlReplacesUnboundIdentityGrants(t *testing.T) {
	id := RepoIdentity{Host: "github.com", Name: "acme/repo", ProviderRepoID: 42, IsIDVerified: true, LocalPath: t.TempDir()}
	for _, grant := range []CloudControlRepoGrant{
		{Host: id.Host, RepoID: id.ProviderRepoID, Name: "old/name", Mode: "private"},
		{Host: id.Host, Name: "Acme/Repo", Mode: "private"},
		{Host: "old.host", Name: "old/name", Checkout: id.LocalPath, Mode: "private"},
	} {
		s := Settings{CloudControlRepos: []CloudControlRepoGrant{grant}}
		if err := SetRepoCloudControl(&s, id, "full"); err != nil {
			t.Fatal(err)
		}
		if len(s.CloudControlRepos) != 1 || s.CloudControlRepos[0].Checkout != id.LocalPath {
			t.Fatalf("grant not replaced: %+v", s.CloudControlRepos)
		}
		if got, source := ResolveRepoCloudControl(s, id); got != CloudControlFull || source != CloudControlSourceGrant {
			t.Fatalf("replacement resolves to %s (%s)", got, source)
		}
	}
}
