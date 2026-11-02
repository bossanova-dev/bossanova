package chatpicker

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestNeededFromYouListsWhatWouldRaiseTheLevel_ChatPicker_AC208901: a leveled
// park lists every lowered-by reason, in served order, under the raise heading
// below its question; a park without reasons shows no heading.
func TestNeededFromYouListsWhatWouldRaiseTheLevel_ChatPicker_AC208901(t *testing.T) {
	reasons := []string{"large diff", "touches the auth path", "no end-to-end test"}
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, 40)
	question := m.neededQuestion()
	m.session.Attention.ConfidenceLevel = pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW
	m.session.Attention.LoweredBy = reasons
	got := m.neededQuestion()
	if !strings.HasPrefix(got, question) {
		t.Fatalf("raise list replaced the question:\n%s", got)
	}
	heading := strings.Index(got, "What would raise this")
	if heading < 0 {
		t.Fatalf("no raise heading:\n%s", got)
	}
	at := heading
	for _, reason := range reasons {
		next := strings.Index(got[at:], "• "+reason)
		if next < 0 {
			t.Fatalf("reason %q missing or out of order:\n%s", reason, got)
		}
		at += next
	}
	m.buildTableRows()
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "What would raise this") || !strings.Contains(view, reasons[2]) {
		t.Errorf("details view lacks the raise list:\n%s", view)
	}

	m.session.Attention.LoweredBy = nil
	if got := m.neededQuestion(); strings.Contains(got, "What would raise this") {
		t.Errorf("a park without reasons shows the heading:\n%s", got)
	}
	m.session.Attention.LoweredBy = reasons
	m.session.Attention.ConfidenceLevel = pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
	if got := m.neededQuestion(); strings.Contains(got, "What would raise this") {
		t.Errorf("a park without a level shows the heading:\n%s", got)
	}
}
