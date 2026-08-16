package session

import (
	"slices"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Instruction classes are the units bossd reports on when an agent runner does
// not carry the system-prompt suffix into its argv. They name what bossd BUILT,
// not a per-class handshake with the runner: append_system_prompt is a single
// string, so a runner's declaration is atomic for the whole suffix.
const (
	// InstructionClassSessionContext is the boss session context (session,
	// chat, repo identifiers and the boss CLI/MCP affordances) appended for
	// every chat.
	InstructionClassSessionContext = "boss-session-context"
	// InstructionClassAutonomyDirective is the directive telling an unattended
	// run that no human is watching. Dropping it is the incident shape this
	// report exists for, so it escalates the log level.
	InstructionClassAutonomyDirective = "autonomy-directive"
	// InstructionClassSubagentGrant is the bounded subagent-dispatch grant an
	// ATTENDED chat carries (BOS-882). An unattended run's grant travels inside
	// InstructionClassAutonomyDirective instead, so the two never both appear.
	//
	// Its absence is deliberately NOT Critical, and the asymmetry with the
	// autonomy directive is the whole point of the distinction. Critical means
	// "nobody will notice this in time": an unattended run that loses its
	// autonomy directive strands with no human to see it. This class is defined
	// by the opposite condition — a human is in the chat. Dropping it degrades
	// the session to the behaviour bossanova shipped before BOS-882, which the
	// operator can see and re-authorise by hand in the moment. Reporting it at
	// Warn is what fixes the silence; escalating it to Error would also make
	// every attended spawn against a not-yet-rebuilt agent-runner plugin read as
	// an incident, which is the noise ReportUndeliveredInstructions already
	// takes care to avoid. If that trade ever changes, change it here — adding
	// this class to the Critical predicate is a one-line edit.
	InstructionClassSubagentGrant = "subagent-grant"
)

// InstructionDeliveryReport describes instruction classes bossd built that the
// runner did not report as present in the argv it returned.
type InstructionDeliveryReport struct {
	// Undelivered is the set of classes bossd built, reported when the runner
	// did not declare it carried them.
	Undelivered []string
	// Critical is true when an autonomy directive was among them, so the caller
	// logs at Error rather than Warn.
	Critical bool
}

// ReportUndeliveredInstructions returns (report, true) when bossd built
// instruction classes the runner did not report as carried in its argv.
// Returns ok=false when nothing was built, or when the runner declared IN_ARGV.
//
// IN_ARGV is a statement about the argv the runner just returned — that the
// flag carrying the suffix is on the command line. It is NOT a receipt that the
// launched binary accepted, retained or applied the instructions, and nothing
// here should be worded as if it were.
//
// UNSPECIFIED (a plugin binary predating the field, or one that forgot to
// declare) is treated exactly like NONE, so silence is loud rather than
// mistaken for success. Callers surface the raw declaration alongside the
// classes so the two remain distinguishable at triage time.
func ReportUndeliveredInstructions(classes []string, support bossanovav1.AppendSystemPromptSupport) (InstructionDeliveryReport, bool) {
	if len(classes) == 0 {
		return InstructionDeliveryReport{}, false
	}
	if support == bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV {
		return InstructionDeliveryReport{}, false
	}
	return InstructionDeliveryReport{
		Undelivered: classes,
		Critical:    slices.Contains(classes, InstructionClassAutonomyDirective),
	}, true
}

// LogUndeliveredInstructions emits the one structured record naming the
// instruction classes bossd built that the runner did not declare it carried
// into argv. It is the only place this record is shaped, so every host spawn
// site reports identically.
//
// It never fails a spawn and never alters argv: callers log and continue. The
// record deliberately carries only class NAMES and the runner's raw
// declaration — never the prompt body, which contains session identifiers and
// the full instruction text.
//
// The message says "did not confirm", not "did not carry", because the two
// reported declarations do not mean the same thing: NONE is the runner saying
// it dropped the suffix, while UNSPECIFIED is only silence — a plugin binary
// predating the field may well have appended the flag. Asserting a drop would
// make every unattended spawn against a not-yet-rebuilt plugin read as an
// incident. The declaration field distinguishes them for whoever triages.
func LogUndeliveredInstructions(
	logger zerolog.Logger,
	sessionID, agentSessionID, agentName string,
	classes []string,
	support bossanovav1.AppendSystemPromptSupport,
) {
	report, ok := ReportUndeliveredInstructions(classes, support)
	if !ok {
		return
	}
	event := logger.Warn()
	if report.Critical {
		event = logger.Error()
	}
	event.
		Str("session", sessionID).
		Str("agentSessionID", agentSessionID).
		Str("agent", agentName).
		Strs("undeliveredClasses", report.Undelivered).
		Str("declaration", support.String()).
		Msg("agent runner did not confirm bossd instructions reached argv")
}
