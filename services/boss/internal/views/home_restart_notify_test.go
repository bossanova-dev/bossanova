package views

import (
	"context"
	"fmt"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var readySince = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

// readySessions are n sessions whose PRs are ready to merge, as the
// operator's board showed them before the daemon restarted.
func readySessions(n int) []*pb.Session {
	sessions := make([]*pb.Session, n)
	for i := range sessions {
		pr := int32(11700 + i)
		sessions[i] = &pb.Session{
			Id:               fmt.Sprintf("ready-%02d", i),
			Title:            fmt.Sprintf("ready %d", i),
			PrNumber:         &pr,
			PrDisplayHeadSha: fmt.Sprintf("head-%02d", i),
			Attention: &pb.SessionAttention{
				Kind:         pb.AttentionKind_ATTENTION_KIND_MERGE,
				Sentence:     "Ready to merge",
				Since:        timestamppb.New(readySince.Add(time.Duration(i) * time.Minute)),
				IsUnverified: true,
			},
		}
	}
	return sessions
}

// inProgress is sess as a daemon that has not yet read its PR serves it.
func inProgress(sess *pb.Session) *pb.Session {
	out := proto.CloneOf(sess)
	out.PrDisplayHeadSha = ""
	out.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "idle"}
	return out
}

func cloneSessions(sessions []*pb.Session) []*pb.Session {
	out := make([]*pb.Session, len(sessions))
	for i, s := range sessions {
		out[i] = proto.CloneOf(s)
	}
	return out
}

// pollNotifies applies one poll and reports the sessions it notified about.
func pollNotifies(h *HomeModel, sessions []*pb.Session) []string {
	var told []string
	prev := runNotify
	runNotify = func(req notifyRequest) error {
		told = append(told, req.Body)
		return nil
	}
	defer func() { runNotify = prev }()
	if cmd := h.notifyNewNeedsYou(sessions); cmd != nil {
		cmd()
	}
	h.sessions = sessions
	return told
}

// TestHomeDaemonRestartDoesNotRenotifyReadySessions reproduces the operator's
// report: 31 sessions already ready to merge, a daemon restart that serves
// them in progress, then each one turning ready again one poll at a time.
// None of that is news, so no notification may fire.
func TestHomeDaemonRestartDoesNotRenotifyReadySessions(t *testing.T) {
	ready := readySessions(31)
	h := NewHomeModel(nil, context.Background(), nil)
	h.focused = false
	pollNotifies(&h, cloneSessions(ready))

	restarted := make([]*pb.Session, len(ready))
	for i, s := range ready {
		restarted[i] = inProgress(s)
	}
	var told []string
	told = append(told, pollNotifies(&h, cloneSessions(restarted))...)
	for i := range ready {
		restarted[i] = proto.CloneOf(ready[i])
		told = append(told, pollNotifies(&h, cloneSessions(restarted))...)
	}
	if len(told) != 0 {
		t.Fatalf("a daemon restart re-announced %d sessions that were already ready, first %q", len(told), told[0])
	}
}

// TestHomeReloadDoesNotRenotifyReadySessions covers a boss relaunch or a
// rebuilt Home: the board's first poll shows what is already waiting and
// announces none of it.
func TestHomeReloadDoesNotRenotifyReadySessions(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.focused = false
	if told := pollNotifies(&h, readySessions(31)); len(told) != 0 {
		t.Fatalf("the first poll announced %d sessions already waiting, first %q", len(told), told[0])
	}
}

// TestHomeAttentionAfterDaemonRestartIsAnnouncedOnce covers what a restart
// must still announce: a session that becomes ready after it, once, and an
// attention that changed while the daemon was down.
func TestHomeAttentionAfterDaemonRestartIsAnnouncedOnce(t *testing.T) {
	ready := readySessions(3)
	working := inProgress(ready[2])
	before := []*pb.Session{ready[0], ready[1], working}

	h := NewHomeModel(nil, context.Background(), nil)
	h.focused = false
	pollNotifies(&h, cloneSessions(before))
	blank := []*pb.Session{inProgress(ready[0]), inProgress(ready[1]), working}
	pollNotifies(&h, cloneSessions(blank))

	newHead := proto.CloneOf(ready[0])
	newHead.PrDisplayHeadSha = "pushed-while-down"
	newHead.Attention.Since = timestamppb.New(readySince.Add(time.Hour))
	fix := proto.CloneOf(ready[1])
	fix.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Merge conflict", AlertKey: "merge-conflict", Since: ready[1].GetAttention().GetSince()}
	after := []*pb.Session{newHead, fix, ready[2]}

	told := pollNotifies(&h, cloneSessions(after))
	want := []string{"#11700 is ready to merge", "#11701 needs a fix · Merge conflict", "#11702 is ready to merge"}
	if fmt.Sprint(told) != fmt.Sprint(want) {
		t.Fatalf("after the restart announced %q, want %q", told, want)
	}
	for i := range 3 {
		if again := pollNotifies(&h, cloneSessions(after)); len(again) != 0 {
			t.Fatalf("poll %d re-announced %q", i, again)
		}
	}
	pollNotifies(&h, cloneSessions(blank))
	if again := pollNotifies(&h, cloneSessions(after)); len(again) != 0 {
		t.Fatalf("a second restart re-announced %q", again)
	}
}
