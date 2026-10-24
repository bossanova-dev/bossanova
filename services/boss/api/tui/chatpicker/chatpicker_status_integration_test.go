package chatpicker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRotationTimeline_KeepsEndedWedge(t *testing.T) {
	wedgeAt := time.Now().Add(-2 * time.Hour)
	sess := rotationSession(&pb.RotationEvent{
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED,
		CreatedAt: timestamppb.New(wedgeAt),
	})
	sess.LastAgentActivityAt = timestamppb.New(wedgeAt.Add(10 * time.Minute))

	if hints := ui.SessionWarningHintTexts(sess); len(hints) != 0 {
		t.Fatalf("wedge should no longer be current, got %q", hints)
	}
	m := newTestModel(nil, context.Background(), "s", "")
	m.session = sess
	if got := stripANSI(m.timelineBlock()); !strings.Contains(got, "✗ "+rotationStepTitle+"  stopped restarting the agent: it stayed signed out") {
		t.Fatalf("timeline lost the ended wedge event: %q", got)
	}
}
