package displaystatus

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/recurser/bossalib/models"
)

// CallbackWaitingReason renders the canonical human-readable reason for a chat
// parked on an armed GitHub callback:
//
//	awaiting checks_passed_ready on acme/widget#123
//
// This is THE definition of that wording (BOS-668) — the daemon derives it, the
// TUI and the web UI render it, and none of them re-spell it. Returns "" when
// any component is missing, so callers can treat the empty string as "no
// waiting reason known" rather than emitting a half-formed sentence.
func CallbackWaitingReason(trigger, repoOwner, repoName string, prNumber int) string {
	if trigger == "" || repoOwner == "" || repoName == "" || prNumber <= 0 {
		return ""
	}
	return fmt.Sprintf("awaiting %s on %s/%s#%d", trigger, repoOwner, repoName, prNumber)
}

// StandbyLabel is the display label for a chat parked only on a standby
// (contingency) callback — see IsStandbyWaitingReason. It carries no spinner:
// the chat is not what the PR is waiting on.
const StandbyLabel = "standby"

// CallbackWaitingTrigger is the exact inverse of CallbackWaitingReason: it
// returns the trigger named by a canonical waiting reason, or "" for anything
// that is not that wording. Parsing lives beside the producer so no caller
// re-spells the sentence.
func CallbackWaitingTrigger(reason string) string {
	if _, _, _, ok := CallbackWaitingTarget(reason); !ok {
		return ""
	}
	rest, _ := strings.CutPrefix(reason, "awaiting ")
	trigger, _, _ := strings.Cut(rest, " on ")
	return trigger
}

// CallbackWaitingTarget parses the target of a canonical callback reason.
func CallbackWaitingTarget(reason string) (owner, repo string, pr int, ok bool) {
	rest, valid := strings.CutPrefix(reason, "awaiting ")
	if !valid {
		return "", "", 0, false
	}
	trigger, target, valid := strings.Cut(rest, " on ")
	if !valid {
		return "", "", 0, false
	}
	owner, nameAndPR, valid := strings.Cut(target, "/")
	if !valid {
		return "", "", 0, false
	}
	hash := strings.LastIndex(nameAndPR, "#")
	if hash < 0 {
		return "", "", 0, false
	}
	pr, err := strconv.Atoi(nameAndPR[hash+1:])
	if err != nil {
		return "", "", 0, false
	}
	repo = nameAndPR[:hash]
	return owner, repo, pr, CallbackWaitingReason(trigger, owner, repo, pr) == reason
}

// IsOwnPRWaitingReason matches GitHub repository names case-insensitively;
// sessions without a repository match by PR number alone.
func IsOwnPRWaitingReason(reason, sessionNWO string, sessionPR int) bool {
	owner, repo, pr, ok := CallbackWaitingTarget(reason)
	return ok && sessionPR > 0 && pr == sessionPR && (sessionNWO == "" || strings.EqualFold(owner+"/"+repo, sessionNWO))
}

// IsStandbyWaitingReason reports whether reason is a canonical callback waiting
// reason whose trigger is a standby (contingency) trigger — checks_failed or
// closed (models.GitHubCallbackTrigger.IsStandby). The daemon and the TUI both
// decide "standby" here, so neither re-spells the wording or the trigger set.
func IsStandbyWaitingReason(reason string) bool {
	return models.GitHubCallbackTrigger(CallbackWaitingTrigger(reason)).IsStandby()
}
