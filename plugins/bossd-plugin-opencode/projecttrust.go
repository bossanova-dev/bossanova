package main

import (
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// These settings were verified against anomalyco/opencode v1.18.4:
// https://github.com/anomalyco/opencode/tree/v1.18.4/packages/opencode/src
// config/config.ts skips project JSON; config/paths.ts excludes project .opencode
// directories, including their agents, commands and plugins. plugin/index.ts
// honors PURE for all external plugins; skill/index.ts honors DISABLE_EXTERNAL_SKILLS
// for the separate .claude/.agents scan. A custom CONFIG_DIR alone is additive.
var projectIsolationEnv = []string{
	"OPENCODE_DISABLE_PROJECT_CONFIG=1",
	"OPENCODE_PURE=1",
	"OPENCODE_DISABLE_EXTERNAL_SKILLS=1",
}

// projectIsolationSupported requires a complete stable version at the verified
// 1.18.4 floor. Missing patch versions and prereleases cannot prove support.
func projectIsolationSupported(version string) bool {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(version), "v"), ".")
	if len(parts) != 3 {
		return false
	}
	var numbers [3]int
	for i, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return false
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return false
		}
		numbers[i] = n
	}
	floor := [3]int{1, 18, 4}
	for i, n := range numbers {
		if n != floor[i] {
			return n > floor[i]
		}
	}
	return true
}

func (s *Server) projectConfigTrusted(decision bossanovav1.ProjectTrustDecision) (bool, error) {
	switch decision {
	case bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT:
		return true, nil
	case bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY:
		if s.runner == nil || !projectIsolationSupported(s.runner.cliVersion) {
			return false, status.Error(codes.FailedPrecondition, "cannot verify OpenCode project isolation; upgrade OpenCode to 1.18.4 or newer, or use boss repo update <id> --trusted")
		}
		return false, nil
	default:
		return false, status.Errorf(codes.InvalidArgument, "unknown project trust decision %d", decision)
	}
}
