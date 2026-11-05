package attention

import (
	"slices"
	"strconv"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

// CallbackWait names the callback a waiting chat is parked on.
type CallbackWait struct {
	Trigger string
	OtherPR int
}

// ChecksState records whether the served attention has settled check facts.
type ChecksState int

const (
	ChecksUnknown ChecksState = iota
	ChecksPending
	ChecksSettled
)

var callbackWaitLabels = []string{"waiting", "waiting: CI", "waiting: retry", "waiting: ready", "waiting: merge", "waiting: close"}

// CallbackWaitLabels returns the fixed callback labels for display budgets.
func CallbackWaitLabels() []string { return slices.Clone(callbackWaitLabels) }

// IsCallbackWaitLabel distinguishes callback words from PR-derived progress.
func IsCallbackWaitLabel(label string) bool {
	if slices.Contains(callbackWaitLabels, label) {
		return true
	}
	digits, ok := strings.CutPrefix(label, "waiting: #")
	if !ok || digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// CallbackWaitLabel names a wait using its check facts. Pending check counts
// are rendered by the served attention before this fallback is needed.
func CallbackWaitLabel(w CallbackWait, checks ChecksState) string {
	if w.OtherPR > 0 {
		return "waiting: #" + strconv.Itoa(w.OtherPR)
	}
	switch models.GitHubCallbackTrigger(w.Trigger) {
	case "":
		return "waiting"
	case models.GitHubCallbackTriggerChecksPassed, models.GitHubCallbackTriggerChecksPassedReady, models.GitHubCallbackTriggerChecksFailed:
		if checks == ChecksSettled {
			return "waiting: retry"
		}
		return "waiting: CI"
	case models.GitHubCallbackTriggerReadyForReview:
		return "waiting: ready"
	case models.GitHubCallbackTriggerMerged:
		return "waiting: merge"
	case models.GitHubCallbackTriggerClosed:
		return "waiting: close"
	default:
		return "waiting: " + strings.ReplaceAll(w.Trigger, "_", " ")
	}
}

// ChatWaitLabel uses the daemon's PR facts for an own-PR callback wait: the
// served status phrase the session row shows ("checks 12/14"), or the progress
// label from a daemon that serves no phrase.
func ChatWaitLabel(trigger string, served *pb.SessionAttention) string {
	label := served.GetProgressLabel()
	callback := IsCallbackWaitLabel(label)
	chatDerived := callback || slices.Contains(knownPhases, label) || slices.Contains([]string{"working", "idle", "stopped"}, label)
	if served.GetKind() == pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS && label != "" && !chatDerived {
		if phrase := served.GetStatusPhrase(); phrase != "" {
			return phrase
		}
		return label
	}
	checks := ChecksUnknown
	if served.GetKind() == pb.AttentionKind_ATTENTION_KIND_MERGE || label == "waiting: retry" {
		checks = ChecksSettled
	}
	return CallbackWaitLabel(CallbackWait{Trigger: trigger}, checks)
}
