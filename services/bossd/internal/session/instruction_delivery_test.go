package session

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

// unattendedCronSession is the fixture whose prompt carries both instruction
// classes — the shape the whole feature exists to report on.
func unattendedCronSession(id string) *models.Session {
	job := "cron-643"
	return &models.Session{ID: id, Title: "Nightly", CronJobID: &job}
}

func TestReportUndeliveredInstructions(t *testing.T) {
	const (
		inArgv      = bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV
		none        = bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE
		unspecified = bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_UNSPECIFIED
	)

	cases := []struct {
		name        string
		classes     []string
		support     bossanovav1.AppendSystemPromptSupport
		wantReport  bool
		wantClasses []string
		wantCrit    bool
	}{
		{
			name:    "nothing built reports nothing even when the runner drops it",
			classes: nil,
			support: none,
		},
		{
			name:    "empty class slice reports nothing",
			classes: []string{},
			support: unspecified,
		},
		{
			name:    "runner carried the suffix into argv",
			classes: []string{InstructionClassSessionContext, InstructionClassAutonomyDirective},
			support: inArgv,
		},
		{
			name:        "declared NONE reports every class bossd built",
			classes:     []string{InstructionClassSessionContext},
			support:     none,
			wantReport:  true,
			wantClasses: []string{InstructionClassSessionContext},
		},
		{
			// The whole point of the presence-bearing enum: an old plugin
			// binary sends the zero value, and silence must be as loud as a
			// refusal — never mistaken for delivery.
			name:        "UNSPECIFIED is treated exactly like NONE",
			classes:     []string{InstructionClassSessionContext},
			support:     unspecified,
			wantReport:  true,
			wantClasses: []string{InstructionClassSessionContext},
		},
		{
			name:        "dropping the autonomy directive is critical",
			classes:     []string{InstructionClassSessionContext, InstructionClassAutonomyDirective},
			support:     none,
			wantReport:  true,
			wantClasses: []string{InstructionClassSessionContext, InstructionClassAutonomyDirective},
			wantCrit:    true,
		},
		{
			name:        "dropping only session context is not critical",
			classes:     []string{InstructionClassSessionContext},
			support:     unspecified,
			wantReport:  true,
			wantClasses: []string{InstructionClassSessionContext},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, ok := ReportUndeliveredInstructions(tc.classes, tc.support)
			if ok != tc.wantReport {
				t.Fatalf("reported = %v, want %v (report=%+v)", ok, tc.wantReport, report)
			}
			if !tc.wantReport {
				if len(report.Undelivered) != 0 || report.Critical {
					t.Fatalf("no-report case must yield a zero report, got %+v", report)
				}
				return
			}
			if strings.Join(report.Undelivered, ",") != strings.Join(tc.wantClasses, ",") {
				t.Fatalf("Undelivered = %v, want %v", report.Undelivered, tc.wantClasses)
			}
			if report.Critical != tc.wantCrit {
				t.Fatalf("Critical = %v, want %v", report.Critical, tc.wantCrit)
			}
		})
	}
}

// logRecord captures the one structured record so tests can assert its shape
// rather than a substring of prose.
func logRecord(t *testing.T, classes []string, support bossanovav1.AppendSystemPromptSupport) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	LogUndeliveredInstructions(logger, "sess-1", "agent-session-1", "codex", classes, support)
	line := strings.TrimSpace(buf.String())
	if line == "" {
		return nil
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("log line is not JSON (%v): %q", err, line)
	}
	return got
}

func TestLogUndeliveredInstructionsRecord(t *testing.T) {
	got := logRecord(t, []string{InstructionClassSessionContext}, bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE)
	if got == nil {
		t.Fatal("a dropped instruction class must emit exactly one record")
	}
	for field, want := range map[string]string{
		"session":        "sess-1",
		"agentSessionID": "agent-session-1",
		"agent":          "codex",
		"declaration":    "APPEND_SYSTEM_PROMPT_SUPPORT_NONE",
		"level":          "warn",
	} {
		if got[field] != want {
			t.Errorf("record[%q] = %v, want %q", field, got[field], want)
		}
	}
	classes, _ := got["undeliveredClasses"].([]any)
	if len(classes) != 1 || classes[0] != InstructionClassSessionContext {
		t.Errorf("undeliveredClasses = %v, want [%s]", got["undeliveredClasses"], InstructionClassSessionContext)
	}
}

func TestLogUndeliveredInstructionsSeverity(t *testing.T) {
	critical := logRecord(t,
		[]string{InstructionClassSessionContext, InstructionClassAutonomyDirective},
		bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_UNSPECIFIED)
	if critical == nil {
		t.Fatal("dropping the autonomy directive must emit a record")
	}
	if critical["level"] != "error" {
		t.Errorf("level = %v, want error when the autonomy directive is dropped", critical["level"])
	}
	if critical["declaration"] != "APPEND_SYSTEM_PROMPT_SUPPORT_UNSPECIFIED" {
		t.Errorf("declaration = %v, want the raw UNSPECIFIED declaration", critical["declaration"])
	}
}

func TestLogUndeliveredInstructionsSilentWhenDelivered(t *testing.T) {
	if got := logRecord(t,
		[]string{InstructionClassSessionContext, InstructionClassAutonomyDirective},
		bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV); got != nil {
		t.Fatalf("a runner that carried the suffix must emit nothing, got %v", got)
	}
	if got := logRecord(t, nil, bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE); got != nil {
		t.Fatalf("no instructions built must emit nothing, got %v", got)
	}
}

// The record names classes only. The prompt body carries session identifiers
// and the whole instruction text, so it must never reach the log.
//
// This asserts on the unmarshalled record, not on a substring of the raw line:
// the prompt is multiline and zerolog escapes newlines, so a `strings.Contains`
// against the marshalled line could never match the body even if the whole body
// were logged. The field-set check is the load-bearing half — it fails on any
// field added to the record, which is how a body leak would actually arrive.
func TestLogUndeliveredInstructionsOmitsPromptBody(t *testing.T) {
	sess := unattendedCronSession("s-secret")
	text, classes := BuildAppendSystemPrompt(sess, "agent-secret", "codex", config.SubagentDispatchGrantAlways)
	if text == "" || len(classes) == 0 {
		t.Fatalf("fixture must build a non-empty prompt, got text=%q classes=%v", text, classes)
	}

	got := logRecord(t, classes, bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE)
	if got == nil {
		t.Fatal("expected a record")
	}

	allowed := map[string]bool{
		"level": true, "session": true, "agentSessionID": true,
		"agent": true, "undeliveredClasses": true, "declaration": true, "message": true,
	}
	for field := range got {
		if !allowed[field] {
			t.Errorf("record carries unexpected field %q = %v; it must name classes only", field, got[field])
		}
	}

	if leaked, line := promptBodyLeak(recordStrings(got), text); leaked {
		t.Fatalf("record leaked prompt body line %q", line)
	}
}

// LogUndeliveredInstructions must survive the zero-value logger, which is what
// a caller that reports nothing (DescribeChatLaunch) leaves unset.
func TestLogUndeliveredInstructionsZeroLoggerIsSafe(t *testing.T) {
	var logger zerolog.Logger
	LogUndeliveredInstructions(logger, "s", "a", "codex",
		[]string{InstructionClassAutonomyDirective},
		bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE)
}

// BOS-882: an attended chat's subagent grant is reported like every other class
// when the runner does not declare it carried the suffix into argv. Its absence
// is deliberately NOT Critical — a human is in the chat and can re-authorise —
// so this pins both halves: it IS reported, and it does NOT escalate on its own.
func TestReportUndeliveredInstructionsSubagentGrant(t *testing.T) {
	classes := []string{InstructionClassSessionContext, InstructionClassSubagentGrant}

	for _, support := range []bossanovav1.AppendSystemPromptSupport{
		bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE,
		bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_UNSPECIFIED,
	} {
		report, ok := ReportUndeliveredInstructions(classes, support)
		if !ok {
			t.Fatalf("%v: a dropped subagent grant must be reported", support)
		}
		if !slices.Contains(report.Undelivered, InstructionClassSubagentGrant) {
			t.Errorf("%v: Undelivered = %v, want it to name %q", support, report.Undelivered, InstructionClassSubagentGrant)
		}
		if report.Critical {
			t.Errorf("%v: the subagent grant alone must not be Critical; that level is reserved for an unattended run losing its autonomy directive", support)
		}
	}

	// A runner that carried the suffix reports nothing, as for every class.
	if _, ok := ReportUndeliveredInstructions(classes, bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV); ok {
		t.Error("a runner that carried the suffix must report nothing")
	}

	// The grant travelling beside a genuinely critical class does not mask it.
	withDirective := []string{InstructionClassSessionContext, InstructionClassAutonomyDirective}
	report, ok := ReportUndeliveredInstructions(withDirective, bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE)
	if !ok || !report.Critical {
		t.Errorf("autonomy directive must stay Critical: ok=%v report=%+v", ok, report)
	}
}

// The attended grant's class must reach the log record the same way the others
// do — naming the class only, never the directive body.
func TestLogUndeliveredInstructionsNamesSubagentGrant(t *testing.T) {
	sess := &models.Session{ID: "s-attended", Title: "Manual"}
	text, classes := BuildAppendSystemPrompt(sess, "agent-attended", "claude", config.SubagentDispatchGrantAlways)
	if !slices.Contains(classes, InstructionClassSubagentGrant) {
		t.Fatalf("fixture must build the attended grant, got classes=%v", classes)
	}

	got := logRecord(t, classes, bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE)
	if got == nil {
		t.Fatal("expected a record")
	}
	if got["level"] != "warn" {
		t.Errorf("level = %v, want warn: a human is in the chat, so this is not the Error-level incident shape", got["level"])
	}
	names, _ := got["undeliveredClasses"].([]any)
	var found bool
	for _, n := range names {
		if s, ok := n.(string); ok && s == InstructionClassSubagentGrant {
			found = true
		}
	}
	if !found {
		t.Errorf("undeliveredClasses = %v, want it to name %q", names, InstructionClassSubagentGrant)
	}

	// …and the ATTENDED directive body never reaches the record. This is the
	// same scan TestLogUndeliveredInstructionsOmitsPromptBody runs, applied to
	// the prompt shape that test does not build: its fixture is an unattended
	// cron session, so without this the attended grant's prose — which names
	// the settings file and the operator's own configuration — is covered by
	// nothing. Comparing every record VALUE against every prompt LINE is what
	// makes the assertion able to fail: a check that only looked at the class
	// names could never see a leak, because the class names are short constants
	// that appear nowhere in the body.
	//
	// The `allowed`-field-set half of the sibling test is deliberately NOT
	// repeated here: the record's field set is built by LogUndeliveredInstructions
	// and is identical whatever classes it is handed, so asserting it twice
	// would add no coverage this class does not already have there.
	if leaked, line := promptBodyLeak(recordStrings(got), text); leaked {
		t.Fatalf("record leaked attended prompt body line %q", line)
	}

	// Positive control. The previous version of this assertion could not fail
	// under any regression, and read as if it could — so prove the detector
	// above actually detects: fed a value that DOES carry the body, it fires.
	if leaked, _ := promptBodyLeak([]string{text}, text); !leaked {
		t.Fatal("promptBodyLeak failed to flag a value carrying the whole prompt body; the check above proves nothing")
	}
}

// recordStrings flattens every string value in an unmarshalled log record,
// including the elements of its string arrays, so a leak scan sees whatever the
// record actually carries rather than a hand-listed subset of its fields.
func recordStrings(record map[string]any) []string {
	var values []string
	for _, v := range record {
		switch typed := v.(type) {
		case string:
			values = append(values, typed)
		case []any:
			for _, elem := range typed {
				if s, ok := elem.(string); ok {
					values = append(values, s)
				}
			}
		}
	}
	return values
}

// promptBodyLeak reports whether any of values contains an identifiable line of
// the prompt body, returning the offending line. Lines shorter than 24 bytes are
// skipped as too generic to identify the body.
func promptBodyLeak(values []string, text string) (bool, string) {
	for rawLine := range strings.SplitSeq(text, "\n") {
		promptLine := strings.TrimSpace(rawLine)
		if len(promptLine) < 24 {
			continue
		}
		for _, value := range values {
			if strings.Contains(value, promptLine) {
				return true, promptLine
			}
		}
	}
	return false, ""
}
