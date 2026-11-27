package main

import (
	"encoding/json"
	"path/filepath"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// launchConfig is the plugin-wide launch policy from the operator's settings.
// Every agy launch (headless, interactive, one-shot) renders its flags from it
// so the three surfaces cannot drift.
type launchConfig struct {
	model                      string
	mode                       string
	dangerouslySkipPermissions bool
	loginShell                 string
}

var agyModes = []string{"accept-edits", "auto", "plan"}

// agyEfforts is the set `agy --effort` accepts; any other value makes agy exit
// 1 before it starts, so an unknown effort is dropped instead of failing the run.
var agyEfforts = []string{"low", "medium", "high", "xhigh", "max"}

func validMode(mode string) bool { return slices.Contains(agyModes, mode) }

// sessionFlags renders the per-launch flags shared by every agy surface.
// requestModel wins over the plugin's fallback model. When hasUnattendedPolicy
// is set with bypass off and no configured mode, --mode auto is emitted so
// unattended runs execute tools without prompting.
func (c launchConfig) sessionFlags(requestModel, effort, workDir string, hasUnattendedPolicy bool) []string {
	var args []string
	model := requestModel
	if model == "" {
		model = c.model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if slices.Contains(agyEfforts, effort) {
		args = append(args, "--effort", effort)
	}
	if c.dangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	mode := c.mode
	if mode == "" && hasUnattendedPolicy && !c.dangerouslySkipPermissions {
		mode = "auto"
	}
	if mode != "" {
		args = append(args, "--mode", mode)
	}
	if settings := trustedWorkspacesSettings(workDir); settings != "" {
		args = append(args, "--settings", settings)
	}
	return args
}

// trustedWorkspacesSettings pre-trusts workDir for this agy process only
// (--settings is never saved). agy compares the literal path, and a symlinked
// path such as macOS /tmp counts as a different folder, so both spellings go in.
func trustedWorkspacesSettings(workDir string) string {
	if workDir == "" {
		return ""
	}
	dirs := []string{workDir}
	if real, err := filepath.EvalSymlinks(workDir); err == nil && real != workDir {
		dirs = append(dirs, real)
	}
	data, _ := json.Marshal(struct {
		TrustedWorkspaces []string `json:"trustedWorkspaces"`
	}{dirs})
	return string(data)
}

// errUntrustedProject is returned for launches the user has not trusted. agy
// loads a workspace's .agents/hooks.json (shell commands run at session start,
// before any approval), skills and rules from the working directory, and has
// no flag or setting that turns that off, so it cannot run a repository
// untrusted.
var errUntrustedProject = status.Error(codes.FailedPrecondition,
	"Antigravity cannot run an untrusted repository: agy always loads the workspace's .agents hooks and skills; use boss repo update <id> --trusted")

func requireTrustedProject(decision bossanovav1.ProjectTrustDecision) error {
	switch decision {
	case bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT:
		return nil
	case bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY:
		return errUntrustedProject
	default:
		return status.Errorf(codes.InvalidArgument, "unknown project trust decision %d", decision)
	}
}
