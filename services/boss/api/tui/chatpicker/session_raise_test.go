package chatpicker

import (
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
