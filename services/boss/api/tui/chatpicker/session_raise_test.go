package chatpicker

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestNeededFromYouStatesTheConfidenceSentence_ChatPicker_AC208901: a leveled
// park states its level and lowered-by reasons as one sentence below its
// question, wrapped to the answer's width and counted in the details height; a
// leveled park without reasons still names its level; a park without a level
// shows no sentence.
func TestNeededFromYouStatesTheConfidenceSentence_ChatPicker_AC208901(t *testing.T) {
	const want = "Moderate: changes Infrastructure. Reviewing this area would raise this."
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, 40)
	question := m.neededQuestion()
	plainHeight := m.attentionDetailsHeight()
	m.session.Attention.ConfidenceLevel = pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE
	m.session.Attention.LoweredBy = []string{"Changes Infrastructure. Reviewing this area would raise this."}
	if got := m.neededQuestion(); got != question+"\n\n"+want {
		t.Fatalf("needed text = %q, want the question then %q", got, want)
	}
	m.buildTableRows()
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, want) {
		t.Errorf("details view lacks the confidence sentence:\n%s", view)
	}

	m.width = 40
	m.buildTableRows()
	lines := m.neededLines()
	at := len(strings.Split(question, "\n")) + 1
	if len(lines) <= at+1 {
		t.Fatalf("sentence did not wrap at a narrow width: %q", lines)
	}
	if got := strings.Join(strings.Fields(strings.Join(lines[at:], " ")), " "); got != want {
		t.Errorf("wrapped sentence = %q, want %q", got, want)
	}
	for _, line := range lines {
		if ansi.StringWidth(line) > m.neededTextWidth() {
			t.Errorf("line %q is wider than %d", line, m.neededTextWidth())
		}
	}
	m.width = 100
	m.buildTableRows()
	if got := m.attentionDetailsHeight(); got != plainHeight+2 {
		t.Errorf("details height = %d, want %d (blank row and sentence)", got, plainHeight+2)
	}

	m.session.Attention.LoweredBy = nil
	if got := m.neededQuestion(); got != question+"\n\nModerate." {
		t.Errorf("a leveled park without reasons = %q, want its level sentence", got)
	}
	m.session.Attention.LoweredBy = []string{"Changes Infrastructure. Reviewing this area would raise this."}
	m.session.Attention.ConfidenceLevel = pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
	if got := m.neededQuestion(); got != question {
		t.Errorf("a park without a level shows a sentence: %q", got)
	}
}

// TestNeededChecklistNamingItsLevelHasNoBareSentence_ChatPicker_AC208901: a
// sign-off checklist whose label item already reads "low: …" adds no bare
// "Low." under it, and still adds a sentence that names its reasons.
func TestNeededChecklistNamingItsLevelHasNoBareSentence_ChatPicker_AC208901(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, 40)
	m.session.Attention.ParkCodes = []string{"human-review:ui"}
	m.session.Attention.ParkLabel = "low: UI, Persistent write to shared referrer record"
	m.session.Attention.ConfidenceLevel = pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW
	question := m.neededAsk()
	if !strings.Contains(question, "• low: UI, Persistent write to shared referrer record") {
		t.Fatalf("precondition: the checklist lacks the label item:\n%s", question)
	}
	if got := m.neededQuestion(); got != question {
		t.Errorf("checklist naming its level = %q, want no sentence after %q", got, question)
	}
	m.session.Attention.LoweredBy = []string{"Changes UI."}
	if got := m.neededQuestion(); got != question+"\n\nLow: changes UI." {
		t.Errorf("checklist with a reason = %q, want the reason sentence", got)
	}
}

// TestNeededChecklistWrapsUnderItsText: a sign-off item too long for one row
// wraps onto rows that start under its text, not under its bullet, and keeps
// every word.
func TestNeededChecklistWrapsUnderItsText(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, 40)
	m.width = 50
	m.session.Attention.ParkCodes = []string{"human-review:ui"}
	m.session.Attention.ParkLabel = "low: UI, Persistent write to shared referrer record, Silent clearing of stored referrer defaults"
	m.buildTableRows()
	rows := neededRows(t, m)
	at := slices.IndexFunc(rows, func(r string) bool { return strings.HasPrefix(r, "  • low: UI") })
	if at < 0 || at+1 >= len(rows) {
		t.Fatalf("the label item did not wrap: %q", rows)
	}
	words := strings.Fields(strings.TrimPrefix(rows[at], "  • "))
	for _, row := range rows[at+1:] {
		text, ok := strings.CutPrefix(row, "    ")
		if !ok || strings.HasPrefix(text, " ") {
			t.Fatalf("wrapped row %q does not start under the item's text", row)
		}
		words = append(words, strings.Fields(text)...)
	}
	if got := strings.Join(words, " "); got != m.session.Attention.ParkLabel {
		t.Errorf("wrapped item reads %q, want %q", got, m.session.Attention.ParkLabel)
	}
}
