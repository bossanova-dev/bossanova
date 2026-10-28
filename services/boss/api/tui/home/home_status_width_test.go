package home

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestHomePendingVerifyStatusFitsColumn(t *testing.T) {
	h := NewModel(nil, context.Background(), nil)
	for _, reason := range []string{
		"draft", "ambiguous-pr", "ticket-not-in-review", "attached", "claimed",
		"no-session", "session-has-ticket", "brand-new-reason", "",
	} {
		t.Run(reason, func(t *testing.T) {
			att := attention.Compute(attention.Input{
				HasPR: true, PRState: attention.PRStateOpen,
				ChecksTotal: 7, ChecksDone: 7, Mergeable: proto.Bool(true),
				IsFactoryRepo:   true,
				VerifyAdmission: attention.Admission{State: attention.AdmissionPending, Reason: reason},
			})
			if att.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || !attention.IsVerifyHoldLabel(att.GetProgressLabel()) {
				t.Fatalf("pending reason %q did not produce an in-progress hold: %v", reason, att)
			}
			cell := h.RenderSessionStatus(&pb.Session{Attention: att})
			assertHomeStatusFits(t, cell)
			if got := ansi.Strip(cell); got != att.GetProgressLabel() {
				t.Errorf("cell = %q, want full label %q without spinner", got, att.GetProgressLabel())
			}
		})
	}
}

func TestHomeProgressLabelSetsFitColumn(t *testing.T) {
	h := NewModel(nil, context.Background(), nil)
	labels := append(attention.VerifyHoldLabels(), attention.WaitLabels()...)
	labels = append(labels, attention.VerifyUnknownLabel)
	for _, label := range labels {
		t.Run(label, func(t *testing.T) {
			cell := h.renderProgressStatus(label)
			assertHomeStatusFits(t, cell)
			want := label
			if attention.IsWaitLabel(label) {
				want = ansi.Strip(h.Spinner.View()) + label
			}
			if got := ansi.Strip(cell); got != want {
				t.Errorf("cell = %q, want full label with its spinner %q", got, want)
			}
		})
	}
}

func assertHomeStatusFits(t *testing.T, cell string) {
	t.Helper()
	plain := ansi.Strip(cell)
	if strings.Contains(plain, "…") {
		t.Errorf("status was truncated: %q", plain)
	}
	if width := ansi.StringWidth(plain); width > homeStatusWidth {
		t.Errorf("status %q is %d columns, budget %d", plain, width, homeStatusWidth)
	}
}
