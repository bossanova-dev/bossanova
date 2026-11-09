package config

import (
	"fmt"
	"strings"
)

// FullCloudControlWarning explains the machine authority a full grant conveys.
const FullCloudControlWarning = "Anyone who can act as you in Boss Cloud can then drive agents on this machine.\nA full repo can run shell commands as you, so it can reach every other repo on this machine. Use a separate OS user per organization when they need different trust."

// SetRepoCloudControl replaces a registered repo's local grants. Callers obtain
// identity from the local daemon, never from a cloud-provided repository label.
func SetRepoCloudControl(s *Settings, id RepoIdentity, mode string) error {
	if mode != "default" && mode != "private" && mode != "read-only" && mode != "full" {
		return fmt.Errorf("cloud control must be default, private, read-only or full")
	}
	if mode == "full" && id.Host == "github.com" && !id.IsIDVerified {
		return fmt.Errorf("can't verify this repo's identity with GitHub; try again")
	}
	grants := make([]CloudControlRepoGrant, 0, len(s.CloudControlRepos)+1)
	for _, g := range s.CloudControlRepos {
		// A bound grant belongs to its checkout; provider identity may be shared by clones.
		if sameCheckout(g.Checkout, id.LocalPath) || (g.Checkout == "" && grantMatchesRepo(g, id)) {
			continue
		}
		grants = append(grants, g)
	}
	if mode != "default" {
		repoID := int64(0)
		if id.IsIDVerified {
			repoID = id.ProviderRepoID
		}
		grants = append(grants, CloudControlRepoGrant{Host: id.Host, RepoID: repoID, Name: id.Name, Checkout: id.LocalPath, Mode: mode})
	}
	s.CloudControlRepos = grants
	return nil
}

func grantMatchesRepo(g CloudControlRepoGrant, id RepoIdentity) bool {
	return sameCheckout(g.Checkout, id.LocalPath) || (g.Host == id.Host && ((g.Name != "" && strings.EqualFold(g.Name, id.Name)) || (g.RepoID > 0 && id.IsIDVerified && g.RepoID == id.ProviderRepoID)))
}

// HasUnverifiedRepoGrant flags loose grants even when they do not lower the
// effective mode, so the operator can repair stale identity bindings.
func HasUnverifiedRepoGrant(s Settings, id RepoIdentity) bool {
	for _, g := range s.CloudControlRepos {
		if !grantMatchesRepo(g, id) {
			continue
		}
		providerMatches := g.RepoID > 0 && id.IsIDVerified && g.RepoID == id.ProviderRepoID
		noProvider := g.RepoID == 0 && id.ProviderRepoID == 0 && id.Host != "github.com"
		if g.Host != id.Host || !sameCheckout(g.Checkout, id.LocalPath) || !providerMatches && !noProvider || g.Mode != string(grantCloudControl(g.Mode)) {
			return true
		}
	}
	return false
}
