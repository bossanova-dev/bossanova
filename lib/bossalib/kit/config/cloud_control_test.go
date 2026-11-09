package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCloudControl(t *testing.T) {
	for _, raw := range []string{"", "read-only", "full", " full ", "FULL", "yes", "private", " private ", "PRIVATE", "privat"} {
		t.Run(raw, func(t *testing.T) {
			want := CloudControlReadOnly
			if raw == "" || raw == "full" || raw == " full " {
				want = CloudControlFull
			}
			if raw == "private" || raw == " private " {
				want = CloudControlMode("private")
			}
			if got := ResolveCloudControl(raw); got != want {
				t.Fatalf("mode = %v, want %v", got, want)
			}
		})
	}
}

func TestResolveRepoCloudControl(t *testing.T) {
	checkout := t.TempDir()
	other := t.TempDir()
	id := RepoIdentity{Host: "github.com", ProviderRepoID: 42, IsIDVerified: true, LocalPath: checkout, Name: "acme/repo"}
	grant := CloudControlRepoGrant{Host: id.Host, RepoID: 42, Name: id.Name, Checkout: checkout, Mode: "full"}
	tests := []struct {
		name, machine string
		grants        []CloudControlRepoGrant
		id            RepoIdentity
		mode          CloudControlMode
		source        CloudControlSource
	}{
		{"default", "", nil, id, CloudControlFull, CloudControlSourceDefault},
		{"explicit read-only", "read-only", nil, id, CloudControlReadOnly, CloudControlSourceMachine},
		{"machine", "full", nil, id, CloudControlFull, CloudControlSourceMachine},
		{"invalid machine", "FULL", nil, id, CloudControlReadOnly, CloudControlSourceMachine},
		{"strict", "", []CloudControlRepoGrant{grant}, id, CloudControlFull, CloudControlSourceGrant},
	}
	for _, change := range []string{"unverified", "different id", "different checkout", "different host"} {
		altered := id
		switch change {
		case "unverified":
			altered.IsIDVerified = false
		case "different id":
			altered.ProviderRepoID = 99
		case "different checkout":
			altered.LocalPath = other
		case "different host":
			altered.Host = "enterprise.example"
		}
		tests = append(tests, struct {
			name, machine string
			grants        []CloudControlRepoGrant
			id            RepoIdentity
			mode          CloudControlMode
			source        CloudControlSource
		}{change, "read-only", []CloudControlRepoGrant{grant}, altered, CloudControlReadOnly, CloudControlSourceMachine})
	}
	for _, match := range []string{"name", "checkout", "id"} {
		for _, mode := range []string{"read-only", "private", "unknown", " full "} {
			g := grant
			g.Host = "other.example"
			g.Checkout = other
			g.Name = "different/repo"
			g.RepoID = 99
			g.Mode = mode
			switch match {
			case "name":
				g.Name = "ACME/Repo"
			case "checkout":
				g.Checkout = checkout
			case "id":
				g.RepoID = 42
			}
			want := CloudControlReadOnly
			if mode == "private" {
				want = CloudControlPrivate
			}
			tests = append(tests, struct {
				name, machine string
				grants        []CloudControlRepoGrant
				id            RepoIdentity
				mode          CloudControlMode
				source        CloudControlSource
			}{match + mode, "full", []CloudControlRepoGrant{g}, id, want, CloudControlSourceGrantNarrowed})
		}
	}
	narrow := grant
	narrow.Mode = "private"
	tests = append(tests, struct {
		name, machine string
		grants        []CloudControlRepoGrant
		id            RepoIdentity
		mode          CloudControlMode
		source        CloudControlSource
	}{"strict most restrictive", "full", []CloudControlRepoGrant{grant, narrow}, id, CloudControlPrivate, CloudControlSourceGrant})
	for _, host := range []string{"enterprise.example", "", "github.com"} {
		local := RepoIdentity{Host: host, LocalPath: checkout}
		g := CloudControlRepoGrant{Host: host, Checkout: checkout, Mode: "full"}
		want, source := CloudControlFull, CloudControlSourceGrant
		if host == "github.com" {
			want, source = CloudControlReadOnly, CloudControlSourceMachine
		}
		tests = append(tests, struct {
			name, machine string
			grants        []CloudControlRepoGrant
			id            RepoIdentity
			mode          CloudControlMode
			source        CloudControlSource
		}{"no provider " + host, "read-only", []CloudControlRepoGrant{g}, local, want, source})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, source := ResolveRepoCloudControl(Settings{CloudControl: tt.machine, CloudControlRepos: tt.grants}, tt.id)
			if mode != tt.mode || source != tt.source {
				t.Fatalf("got %s/%s want %s/%s", mode, source, tt.mode, tt.source)
			}
		})
	}
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	grant.Checkout = link
	if mode, _ := ResolveRepoCloudControl(Settings{CloudControl: "read-only", CloudControlRepos: []CloudControlRepoGrant{grant}}, id); mode != CloudControlFull {
		t.Fatalf("symlink: %s", mode)
	}
	grant.Checkout = filepath.Join(checkout, "missing")
	id.LocalPath = grant.Checkout
	if mode, _ := ResolveRepoCloudControl(Settings{CloudControl: "read-only", CloudControlRepos: []CloudControlRepoGrant{grant}}, id); mode != CloudControlReadOnly {
		t.Fatalf("unresolvable path granted: %s", mode)
	}
}

func TestResolveRepoCloudControlStrictGrantModes(t *testing.T) {
	checkout := t.TempDir()
	id := RepoIdentity{Host: "github.com", ProviderRepoID: 42, IsIDVerified: true, LocalPath: checkout}
	tests := []struct {
		name    string
		machine string
		modes   []string
		want    CloudControlMode
		source  CloudControlSource
	}{
		{"unknown preserves private", "private", []string{"unknown"}, CloudControlPrivate, CloudControlSourceMachine},
		{"whitespace is unknown", "private", []string{" private "}, CloudControlPrivate, CloudControlSourceMachine},
		{"unknown narrows full", "full", []string{"unknown"}, CloudControlReadOnly, CloudControlSourceGrantNarrowed},
		{"unknown narrows default full", "", []string{"unknown"}, CloudControlReadOnly, CloudControlSourceGrantNarrowed},
		{"valid full overrides private", "private", []string{"full"}, CloudControlFull, CloudControlSourceGrant},
		{"valid read-only overrides private", "private", []string{"read-only"}, CloudControlReadOnly, CloudControlSourceGrant},
		{"unknown narrows valid full", "private", []string{"full", "unknown"}, CloudControlReadOnly, CloudControlSourceGrantNarrowed},
		{"unknown before valid full", "private", []string{"unknown", "full"}, CloudControlReadOnly, CloudControlSourceGrantNarrowed},
		{"unknown preserves valid private", "full", []string{"private", "unknown"}, CloudControlPrivate, CloudControlSourceGrant},
		{"unknown before valid private", "full", []string{"unknown", "private"}, CloudControlPrivate, CloudControlSourceGrant},
		{"several unknown preserve private", "private", []string{"unknown", "FULL"}, CloudControlPrivate, CloudControlSourceMachine},
		{"valid strict grants restrict each other", "private", []string{"full", "read-only"}, CloudControlReadOnly, CloudControlSourceGrant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := Settings{CloudControl: tt.machine}
			for _, mode := range tt.modes {
				settings.CloudControlRepos = append(settings.CloudControlRepos, CloudControlRepoGrant{
					Host: id.Host, RepoID: id.ProviderRepoID, Checkout: checkout, Mode: mode,
				})
			}
			mode, source := ResolveRepoCloudControl(settings, id)
			if mode != tt.want || source != tt.source {
				t.Fatalf("got %s/%s want %s/%s", mode, source, tt.want, tt.source)
			}
		})
	}
}
