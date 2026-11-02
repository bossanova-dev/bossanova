package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/sessionattention"
	"github.com/recurser/bossd/internal/status"
)

// A pending boss/verify status reads the same label in the session list
// (attention.progress_label) as in the session view (display_label), through
// the real read path: a verify-cron repo whose admission file admits the head,
// a tracker entry from the real display classification and a snapshot of the
// same head, with mergeability known and unknown. The timeline's attention
// answers it the same way.
func TestVerifyPendingLabelMatchesAcrossSurfaces_Sessions_AC225001(t *testing.T) {
	cases := []struct {
		description string
		want        string
	}{
		{description: "verifying… tok", want: "verifying"},
		{description: "waiting: merge-state-unknown", want: "waiting: merge"},
		{description: "waiting: ci-not-settled", want: "waiting: CI"},
		{description: "waiting: base-ci-red", want: "main red"},
		{description: "waiting: some-new-code", want: "waiting: retry"},
	}
	for _, mergeKnown := range []bool{true, false} {
		for _, tc := range cases {
			name := tc.description + map[bool]string{true: " mergeable", false: " mergeability unknown"}[mergeKnown]
			t.Run(name, func(t *testing.T) {
				var mergeable *bool
				if mergeKnown {
					v := true
					mergeable = &v
				}
				sess, facts := listVerifyPendingSession(t, greenCI(2, tc.description), mergeable)
				if sess.GetDisplayStatus() != pb.DisplayStatus_DISPLAY_STATUS_VERIFYING {
					t.Fatalf("display status = %v, want VERIFYING", sess.GetDisplayStatus())
				}
				list, view := sess.GetAttention().GetProgressLabel(), sess.GetDisplayLabel()
				if list != view || list != tc.want {
					t.Fatalf("list %q, view %q, want both %q", list, view, tc.want)
				}
				if attention.IsVerifyWaitLabel(list) && (list == "waiting" || list == tc.description) {
					t.Fatalf("label %q is a bare wait or the raw status", list)
				}
				latest := &facts.Snapshots[len(facts.Snapshots)-1]
				if timeline := attention.Compute(sessionattention.Derive(attentionFacts(facts, latest)).Input).GetProgressLabel(); timeline != tc.want {
					t.Fatalf("timeline attention label %q, want %q", timeline, tc.want)
				}
			})
		}
	}
}

// An admitted head with no pending boss/verify status still reads queued.
func TestVerifyAdmittedHeadWithoutPendingStatusReadsQueued(t *testing.T) {
	mergeable := true
	sess, _ := listVerifyPendingSession(t, greenCI(2, ""), &mergeable)
	if got := sess.GetAttention().GetProgressLabel(); got != attention.WaitingForVerifyLabel {
		t.Fatalf("list label = %q, want %q", got, attention.WaitingForVerifyLabel)
	}
}

// listVerifyPendingSession lists one open PR session in a verify-cron repo whose
// admission file admits timelineHead, with checks on that head, and returns the
// served session and its timeline facts.
func listVerifyPendingSession(t *testing.T, checks []vcs.CheckResult, mergeable *bool) (*pb.Session, timelineFacts) {
	t.Helper()
	repoPath := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "init", repoPath)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	dir := filepath.Join(repoPath, ".git", "boss-verify")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verify-admission.json"), admissionSnapshot(t, timelineHead, "admitted", ""), 0o600); err != nil {
		t.Fatal(err)
	}
	n := 1
	row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n, State: machine.ReadyForReview}
	info := vcs.ComputeDisplayStatus(&vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: timelineHead, Mergeable: mergeable}, checks, nil)
	info.HeadSHA, info.Mergeable = timelineHead, mergeable
	tr := status.NewDisplayTracker()
	tr.Set("s", info)
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
	s.repos = &listSessionsRepoStoreFake{repos: map[string]*models.Repo{"r": {ID: "r", LocalPath: repoPath}}}
	s.cronJobs = &attentionCronStore{jobs: []*domain.CronJob{{Prompt: "/boss-verify", IsEnabled: true}}}
	s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{SessionID: "s", HeadSHA: timelineHead, ComputedStatus: int(info.Status), RawJSON: checksJSON(t, checks)}}}
	resp, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	facts, err := s.timelineFacts(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	return onlySession(t, resp.Msg.Sessions), facts
}
