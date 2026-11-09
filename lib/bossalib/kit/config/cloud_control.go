package config

import (
	"path/filepath"
	"strings"
)

// CloudControlMode is the local operator's authority granted to cloud commands.
type CloudControlMode string

const (
	// CloudControlReadOnly allows observation and locally verified housekeeping.
	CloudControlReadOnly CloudControlMode = "read-only"
	// CloudControlPrivate allows metadata observation only.
	CloudControlPrivate CloudControlMode = "private"
	// CloudControlFull also allows commands that change local state.
	CloudControlFull CloudControlMode = "full"
)

// ResolveCloudControl maps the local setting to a mode. An absent or blank value
// is full, the default for every install. Any other value that is not exactly
// "private", "read-only" or "full" after trimming whitespace fails closed to
// read-only, so a typo never widens authority; unknown raw values survive
// settings round trips.
func ResolveCloudControl(raw string) CloudControlMode {
	switch CloudControlMode(strings.TrimSpace(raw)) {
	case "", CloudControlFull:
		return CloudControlFull
	case CloudControlPrivate:
		return CloudControlPrivate
	case CloudControlReadOnly:
		return CloudControlReadOnly
	}
	return CloudControlReadOnly
}

// CloudControlRepoGrant binds locally granted authority to a checkout and provider identity.
// Name is a display label, and can only contribute a restrictive loose match.
type CloudControlRepoGrant struct {
	Host     string `json:"host"`
	RepoID   int64  `json:"repo_id,omitempty"`
	Name     string `json:"name"`
	Checkout string `json:"checkout"`
	Mode     string `json:"mode"`
}

// RepoIdentity contains the registered checkout and locally verified provider identity.
type RepoIdentity struct {
	Host           string
	ProviderRepoID int64
	IsIDVerified   bool
	LocalPath      string
	Name           string
}

// CloudControlSource explains which local setting determined a mode.
type CloudControlSource string

const (
	CloudControlSourceGrant         CloudControlSource = "grant"
	CloudControlSourceGrantNarrowed CloudControlSource = "grant-narrowed"
	CloudControlSourceMachine       CloudControlSource = "machine"
	CloudControlSourceDefault       CloudControlSource = "default"
)

// MachineCloudControl resolves machine-wide authority for repo-less and account commands.
func MachineCloudControl(s Settings) CloudControlMode { return ResolveCloudControl(s.CloudControl) }

// ResolveRepoCloudControl widens authority only for proven, checkout-bound grants.
func ResolveRepoCloudControl(s Settings, id RepoIdentity) (CloudControlMode, CloudControlSource) {
	mode, source := MachineCloudControl(s), CloudControlSourceDefault
	if s.CloudControl != "" {
		source = CloudControlSourceMachine
	}
	strict := make([]bool, len(s.CloudControlRepos))
	for i, g := range s.CloudControlRepos {
		pathMatch := sameCheckout(g.Checkout, id.LocalPath)
		idMatch := g.RepoID > 0 && id.IsIDVerified && g.RepoID == id.ProviderRepoID
		noProvider := g.RepoID == 0 && id.ProviderRepoID == 0 && id.Host != "github.com"
		resolved := grantCloudControl(g.Mode)
		// Unknown modes can only narrow, even when the identity strictly matches.
		strict[i] = g.Host == id.Host && pathMatch && (idMatch || noProvider) && g.Mode == string(resolved)
		if strict[i] {
			if source != CloudControlSourceGrant {
				mode = resolved
			} else {
				mode = restrictCloudControl(mode, resolved)
			}
			source = CloudControlSourceGrant
		}
	}
	for i, g := range s.CloudControlRepos {
		if strict[i] {
			continue
		}
		nameMatch := g.Name != "" && id.Name != "" && strings.EqualFold(g.Name, id.Name)
		if sameCheckout(g.Checkout, id.LocalPath) || (g.RepoID > 0 && id.IsIDVerified && g.RepoID == id.ProviderRepoID) || nameMatch {
			narrowed := restrictCloudControl(mode, grantCloudControl(g.Mode))
			if narrowed != mode {
				mode = narrowed
				source = CloudControlSourceGrantNarrowed
			}
		}
	}
	return mode, source
}

func sameCheckout(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	left, err := filepath.EvalSymlinks(filepath.Clean(a))
	if err != nil {
		return false
	}
	right, err := filepath.EvalSymlinks(filepath.Clean(b))
	return err == nil && left == right
}

func grantCloudControl(raw string) CloudControlMode {
	switch CloudControlMode(raw) {
	case CloudControlFull, CloudControlPrivate, CloudControlReadOnly:
		return CloudControlMode(raw)
	}
	return CloudControlReadOnly
}

func restrictCloudControl(a, b CloudControlMode) CloudControlMode {
	if a == CloudControlPrivate || b == CloudControlPrivate {
		return CloudControlPrivate
	}
	if a == CloudControlReadOnly || b == CloudControlReadOnly {
		return CloudControlReadOnly
	}
	return CloudControlFull
}
