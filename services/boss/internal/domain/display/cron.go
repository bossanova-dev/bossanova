package display

// RunNowSkipLabel phrases a scheduler skip reason for people. The TUI's run-now
// status line and `boss cron run-now` both print it. Unknown reasons pass
// through verbatim.
func RunNowSkipLabel(reason string) string {
	switch reason {
	case "":
		return "no reason given"
	case "gated":
		return "blocked by gate command"
	case "gate_failed":
		// Distinct from "gated" on purpose (BOS-881): the gate never produced a
		// verdict, so the run was blocked defensively rather than because the
		// gate decided there was no work. Saying "blocked by gate command"
		// here would repeat the conflation the ticket removed.
		return "the gate command could not run; check the daemon log"
	case "overlap_prev_active":
		return "previous run still active"
	case "disabled":
		return "job is disabled"
	default:
		return reason
	}
}
