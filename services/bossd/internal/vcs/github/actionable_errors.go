package github

import (
	"fmt"
	"strings"

	"github.com/recurser/bossalib/vcs"
)

func classifyMergeError(err error, repoPath string, prID int) error {
	if err == nil {
		return nil
	}

	errText := err.Error()
	if strings.Contains(errText, "refusing to allow an OAuth App to create or update workflow") &&
		strings.Contains(errText, "without `workflow` scope") {
		return &vcs.ActionableError{
			Code:    vcs.ErrorCodeGitHubWorkflowScopeRequired,
			Summary: "Auto-merge blocked: GitHub token lacks workflow permission",
			Detail:  fmt.Sprintf("PR #%d in %s changes a file under .github/workflows. GitHub refuses OAuth/PAT tokens without workflow permission from merging workflow-file changes.", prID, repoFlag(repoPath)),
			Command: "gh auth refresh -h github.com -s workflow",
			Err:     err,
		}
	}

	return err
}
