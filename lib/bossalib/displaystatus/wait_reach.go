package displaystatus

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

// WaitChecks is the check state relevant to whether an armed wake can fire.
type WaitChecks int

const (
	WaitChecksUnknown WaitChecks = iota
	WaitChecksPending
	WaitChecksGreen
	WaitChecksFailing
)

// WaitPR contains only the PR facts needed to judge an armed wake.
type WaitPR struct {
	Open        bool
	Draft       bool
	Conflicting bool
	Checks      WaitChecks
}

// WaitBlocker names the action preventing an armed wake from firing.
type WaitBlocker string

// WaitReach describes whether the current PR can satisfy a wake without intervention.
type WaitReach struct {
	Reachable bool
	Blocker   WaitBlocker
	Owner     string
}

// CallbackWaitReach only rejects waits whose blocker is known. Green ready PRs
// remain reachable because the verify cron also judges them without a callback.
// awaitingBaseline means a transition watch has not yet observed an unsatisfied PR.
func CallbackWaitReach(trigger string, awaitingBaseline bool, pr WaitPR) WaitReach {
	var blocker WaitBlocker
	if pr.Open {
		switch trigger {
		case "checks_passed_ready":
			switch {
			case pr.Conflicting:
				blocker = "conflict"
			case pr.Checks == WaitChecksFailing:
				blocker = "failing"
			case pr.Draft:
				blocker = "draft"
			}
		case "checks_passed":
			switch {
			case pr.Checks == WaitChecksFailing:
				blocker = "failing"
			case pr.Conflicting && pr.Checks != WaitChecksPending:
				blocker = "conflict"
			case awaitingBaseline && pr.Draft && pr.Checks == WaitChecksGreen:
				blocker = "draft"
			}
		}
	}
	reach := WaitReach{Reachable: blocker == "", Blocker: blocker}
	if blocker == "draft" {
		reach.Owner = "person"
	} else if blocker != "" {
		reach.Owner = "repair"
	}
	return reach
}

// WaitPRFromSession reads the display projection and freshness-gated check latch,
// preserving unknown mergeability rather than treating it as a conflict.
func WaitPRFromSession(p *pb.Session) WaitPR {
	if p == nil {
		return WaitPR{}
	}
	status := p.GetDisplayStatus()
	pr := WaitPR{
		Open:        status != pb.DisplayStatus_DISPLAY_STATUS_MERGED && status != pb.DisplayStatus_DISPLAY_STATUS_CLOSED,
		Draft:       status == pb.DisplayStatus_DISPLAY_STATUS_DRAFT,
		Conflicting: (p.PrMergeable != nil && !p.GetPrMergeable()) || status == pb.DisplayStatus_DISPLAY_STATUS_CONFLICT,
	}
	switch {
	case p.GetDisplayHasFailures() || status == pb.DisplayStatus_DISPLAY_STATUS_FAILING:
		pr.Checks = WaitChecksFailing
	case status == pb.DisplayStatus_DISPLAY_STATUS_CHECKING:
		pr.Checks = WaitChecksPending
	case status == pb.DisplayStatus_DISPLAY_STATUS_PASSING || status == pb.DisplayStatus_DISPLAY_STATUS_APPROVED:
		pr.Checks = WaitChecksGreen
	}
	// Draft and conflict display statuses take precedence over CI. Use only the
	// freshness-gated latch; the raw observed latch can describe an older head.
	if pr.Checks == WaitChecksUnknown {
		switch p.GetLastCheckState() {
		case pb.ChecksOverall_CHECKS_OVERALL_PASSED:
			pr.Checks = WaitChecksGreen
		case pb.ChecksOverall_CHECKS_OVERALL_PENDING:
			pr.Checks = WaitChecksPending
		case pb.ChecksOverall_CHECKS_OVERALL_FAILED:
			pr.Checks = WaitChecksFailing
		case pb.ChecksOverall_CHECKS_OVERALL_UNSPECIFIED:
		}
	}
	return pr
}
