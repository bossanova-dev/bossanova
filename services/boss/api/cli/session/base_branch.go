package session

import (
	"fmt"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"
)

func validateBaseBranchFlag(base string, present bool, pr *int32, quickChat, nonInteractive bool) error {
	if !present {
		return nil
	}
	var reason string
	switch {
	case strings.TrimSpace(base) == "":
		reason = "--base-branch must name a non-empty branch"
	case pr != nil:
		reason = "--base-branch and --pr are mutually exclusive: an existing PR already has a base"
	case quickChat:
		reason = "--base-branch and --quick-chat are mutually exclusive: a quick chat cuts no branch"
	case !nonInteractive:
		reason = "--base-branch is supported only on the non-interactive --repo + --prompt path"
	default:
		return nil
	}
	return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("%s", reason))
}
