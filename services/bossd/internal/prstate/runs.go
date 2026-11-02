// Package prstate reads one pull request's authoritative state through the
// daemon's cached GitHub provider and derives the verdicts a session acts on:
// the check verdict, the verify phase and, for a green head, whether every
// workflow run has settled.
package prstate

import (
	"sort"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
)

// Held workflow run classes.
const (
	// HeldClassInFlight is a run that is queued, running or in a state the
	// provider did not name.
	HeldClassInFlight = "in-flight"
	// HeldClassAwaitingApproval is a run waiting on a human approval
	// (GitHub's "waiting" or "action_required").
	HeldClassAwaitingApproval = "awaiting-approval"
)

// HeldWorkflowRun is one head workflow run that has not completed.
type HeldWorkflowRun struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Event  string `json:"event"`
	Class  string `json:"class"`
}

// HeldHeadRuns returns the runs for headSHA that have not completed, sorted by
// name, status and event. A run carrying another head SHA is ignored; one with
// no SHA is taken as the head's. An empty result means every head run has
// settled, so a check set read before the head's workflows attached their jobs
// is not mistaken for a complete one.
func HeldHeadRuns(headSHA string, runs []vcs.WorkflowRun) []HeldWorkflowRun {
	var held []HeldWorkflowRun
	for _, run := range runs {
		if run.HeadSHA != "" && run.HeadSHA != headSHA {
			continue
		}
		if strings.EqualFold(run.Status, "completed") {
			continue
		}
		class := HeldClassInFlight
		if strings.EqualFold(run.Status, "waiting") || strings.EqualFold(run.Status, "action_required") {
			class = HeldClassAwaitingApproval
		}
		held = append(held, HeldWorkflowRun{Name: run.Name, Status: run.Status, Event: run.Event, Class: class})
	}
	sort.Slice(held, func(i, j int) bool {
		if held[i].Name != held[j].Name {
			return held[i].Name < held[j].Name
		}
		if held[i].Status != held[j].Status {
			return held[i].Status < held[j].Status
		}
		return held[i].Event < held[j].Event
	})
	return held
}

// AwaitsApproval reports whether any held run is waiting on a human approval.
func AwaitsApproval(held []HeldWorkflowRun) bool {
	for _, run := range held {
		if run.Class == HeldClassAwaitingApproval {
			return true
		}
	}
	return false
}
