package server

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/sessionattention"
	"github.com/recurser/bossd/internal/status"
)

func TestTimelinePartialMatchesSessionAttention_ChatPicker_AC14(t *testing.T) {
	for _, tc := range []struct {
		name, displayHead, snapshotHead string
		partial                         bool
		status                          vcs.DisplayStatus
		want                            pb.AttentionKind
	}{
		{"current", timelineHead, timelineHead, true, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_FIX},
		{"case insensitive", strings.ToUpper(timelineHead), timelineHead, true, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_FIX},
		{"stale marker", "older-head", timelineHead, true, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"empty head", "", "", true, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{"nonpartial", timelineHead, timelineHead, false, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"required review", timelineHead, timelineHead, true, vcs.DisplayStatusReview, pb.AttentionKind_ATTENTION_KIND_FIX},
		{"stale required review", "older-head", timelineHead, true, vcs.DisplayStatusReview, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, mergeable := 1, true
			row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n}
			tr := status.NewDisplayTracker()
			tr.Set(row.ID, vcs.DisplayInfo{Status: tc.status, HeadSHA: tc.displayHead, IsPartial: tc.partial, Mergeable: &mergeable})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{SessionID: row.ID, HeadSHA: tc.snapshotHead, PolledAt: at(10), ComputedStatus: int(tc.status), RawJSON: checksJSON(t, greenCI(2, ""))}}}
			facts, err := s.timelineFacts(t.Context(), row)
			if err != nil {
				t.Fatal(err)
			}
			steps, kind := assembleTimeline(facts)
			if kind != tc.want {
				t.Fatalf("kind=%v want=%v", kind, tc.want)
			}
			if kind != pb.AttentionKind_ATTENTION_KIND_FIX {
				if facts.IsPartial || (tc.partial && facts.Mergeable != nil) {
					t.Fatal("live PR facts leaked across heads")
				}
				return
			}
			resp, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if got := onlySession(t, resp.Msg.Sessions).GetAttention().GetKind(); tc.displayHead == tc.snapshotHead && got != kind {
				t.Fatalf("board=%v timeline=%v", got, kind)
			}
			assertSteps(t, steps, []wantStep{{at(10), "CI passed, 2 checks", "", done}, {at(10), "Parked for you", "only part of the ticket was finished, so the PR is held", current}})
		})
	}
}

func TestTimelinePartialExistingVerifyParkIsNotDuplicated(t *testing.T) {
	f := timelineFacts{HasPR: true, IsPartial: true, Snapshots: []db.CheckSnapshot{snap(t, at(30), vcs.DisplayStatusNeedsHuman, greenCI(2, "needs human: no-receipt,partial-build"))}}
	steps, kind := assembleTimeline(f)
	if kind != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("kind=%v", kind)
	}
	parks := 0
	for _, st := range steps {
		if strings.HasPrefix(st.GetTitle(), "Parked for you") {
			parks++
		}
	}
	if parks != 1 {
		t.Fatalf("park count=%d", parks)
	}
	af := attentionFacts(f, &f.Snapshots[0])
	if !af.IsPartial {
		t.Fatal("attention facts lost partial marker")
	}
	if got := attention.Compute(sessionattention.Derive(af).Input); got.GetKind() != kind {
		t.Fatalf("attention=%v", got)
	}
}

func TestGetSessionTimelineUsesCurrentHeadParkLabel(t *testing.T) {
	const label = "the verify judge (cannot grade itself)"
	for _, tc := range []struct {
		name, head string
		status     vcs.DisplayStatus
		want       string
	}{
		{"current", timelineHead, vcs.DisplayStatusNeedsHuman, "this PR changes " + label},
		{"case insensitive", strings.ToUpper(timelineHead), vcs.DisplayStatusNeedsHuman, "this PR changes " + label},
		{"stale head", "older-head", vcs.DisplayStatusNeedsHuman, "touches a path that always needs a person's sign-off"},
		{"empty head", "", vcs.DisplayStatusNeedsHuman, "touches a path that always needs a person's sign-off"},
		{"not parked", timelineHead, vcs.DisplayStatusPassing, "touches a path that always needs a person's sign-off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 1
			sqlDB := setupServerTestDB(t)
			repos, sessions := db.NewRepoStore(sqlDB), db.NewSessionStore(sqlDB)
			repo, err := repos.Create(t.Context(), db.CreateRepoParams{DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r.git", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt"})
			if err != nil {
				t.Fatal(err)
			}
			row, err := sessions.Create(t.Context(), db.CreateSessionParams{RepoID: repo.ID, Title: "s", BranchName: "feat/x", BaseBranch: "main", PRNumber: &n})
			if err != nil {
				t.Fatal(err)
			}
			tr := status.NewDisplayTracker()
			tr.Set(row.ID, vcs.DisplayInfo{Status: tc.status, HeadSHA: tc.head, VerifyLabel: label})
			s := New(Config{Repos: repos, Sessions: sessions, DisplayTracker: tr})
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{SessionID: row.ID, HeadSHA: timelineHead, PolledAt: at(10), ComputedStatus: int(vcs.DisplayStatusNeedsHuman), RawJSON: checksJSON(t, greenCI(2, "needs human: always-human-path"))}}}
			resp, err := s.GetSessionTimeline(t.Context(), connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: row.ID}))
			if err != nil {
				t.Fatal(err)
			}
			assertSteps(t, resp.Msg.GetSteps(), []wantStep{{at(10), "CI passed, 2 checks", "", done}, {at(10), "Parked for you", tc.want, current}})
		})
	}
}

func TestTimelineParkReadsCurrentHeadChecklistItems(t *testing.T) {
	for _, head := range []string{timelineHead, "older-head"} {
		t.Run(head, func(t *testing.T) {
			n := 1
			row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n}
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, nil, nil)
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{SessionID: row.ID, HeadSHA: timelineHead, PolledAt: at(10), ComputedStatus: int(vcs.DisplayStatusNeedsHuman), RawJSON: checksJSON(t, greenCI(2, "needs human: ledger-open"))}}}
			s.evidence = openItemsEvidence{head: head, items: []string{"finish the migration", "prove the rollback"}}
			facts, err := s.timelineFacts(t.Context(), row)
			if err != nil {
				t.Fatal(err)
			}
			want := "the PR checklist has open items"
			if head == timelineHead {
				want = "the PR checklist has 2 open items"
			}
			steps := buildTimeline(facts)
			assertSteps(t, steps, []wantStep{{at(10), "CI passed, 2 checks", "", done}, {at(10), "Parked for you", want, current}})
		})
	}
}
