package chatpicker

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNeededReviewMatchesSharedAsks_ChatPicker_AC216001(t *testing.T) {
	data, err := os.ReadFile("../../../../../lib/bossalib/attention/testdata/park_asks.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Codes       []string `json:"codes"`
		Label       string   `json:"label"`
		EvidenceURL string   `json:"evidenceUrl"`
		Head        string   `json:"head"`
		Question    string   `json:"question"`
		Items       []string `json:"items"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		m := newTestModel(nil, context.Background(), "s", "")
		m.session = &pb.Session{PrDisplayHeadSha: r.Head, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: r.Codes, ParkLabel: r.Label, EvidenceUrl: r.EvidenceURL}}
		lines := []string{r.Question}
		for _, item := range r.Items {
			lines = append(lines, "  • "+item)
		}
		if got, want := m.neededQuestion(), strings.Join(lines, "\n"); got != want {
			t.Errorf("codes %v: got %q, want %q", r.Codes, got, want)
		}
	}
}

func TestFailureParkHeaderSaysBlocked(t *testing.T) {
	sess := &pb.Session{Title: "Failed park", DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, DisplayLabel: "blocked", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, ParkCodes: []string{"repair-exhausted"}}}
	banner := stripANSI(ui.RenderBanner(ui.ViewChatPicker, ui.BannerOpts{Session: sess, Width: 160}))
	if !strings.Contains(banner, "Failed park (blocked)") {
		t.Fatalf("failure header: %s", banner)
	}
}

// TestChatPickerDetailsBannerSaysWaitingForYou: the session-details
// title never shows the API's "needs sign-off" label, with or without
// attention. Its status suffix carries the TUI's "waiting for you" mapping.
func TestChatPickerDetailsBannerSaysWaitingForYou(t *testing.T) {
	sess := &pb.Session{Id: "s", Title: "Hide the line", PrNumber: new(int32(3154)), DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, DisplayLabel: displaystatus.NeedsSignOffLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
	for _, attention := range []*pb.SessionAttention{nil, {Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:ui"}, Since: timestamppb.Now()}} {
		sess.Attention = attention
		banner := stripANSI(ui.RenderBanner(ui.ViewChatPicker, ui.BannerOpts{Session: sess, Width: 160}))
		if !strings.Contains(banner, "Hide the line (waiting for you)") || strings.Contains(banner, displaystatus.NeedsSignOffLabel) {
			t.Fatalf("banner with attention %v:\n%s", attention, banner)
		}
	}
}
