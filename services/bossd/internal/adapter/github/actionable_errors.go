package github

import (
	"fmt"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
)

func classifyMergeError(err error, repoPath string, prID int) error {
	if err == nil {
		return nil
	}

	errText := ghResponseText(err)
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

	// GitHub refuses a merge whose pinned head moved. Match the HEAD phrase only:
	// "Base branch was modified" is a different condition.
	if strings.Contains(strings.ToLower(errText), "head branch was modified") {
		return fmt.Errorf("%w: %w", vcs.ErrHeadMismatch, err)
	}

	return err
}
