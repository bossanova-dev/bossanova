package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"

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
	text, classes := BuildAppendSystemPrompt(sess, "agent-secret", "codex")
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

	var values []string
	for _, v := range got {
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
	for rawLine := range strings.SplitSeq(text, "\n") {
		promptLine := strings.TrimSpace(rawLine)
		if len(promptLine) < 24 { // too short to identify the body
			continue
		}
		for _, value := range values {
			if strings.Contains(value, promptLine) {
				t.Fatalf("record leaked prompt body line %q in %q", promptLine, value)
			}
		}
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
