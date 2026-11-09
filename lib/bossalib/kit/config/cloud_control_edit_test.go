package config

import "testing"

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
