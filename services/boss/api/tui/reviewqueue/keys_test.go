package reviewqueue

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestQueueProofUsesInjectedOpenerAndSafeURLs(t *testing.T) {
	for _, tc := range []struct {
		name, proof, pr, want string
		key                   rune
	}{
		{"proof", "https://proof.test/page", "https://github.test/pr", "https://proof.test/page", 'p'},
		{"fallback", "", "http://github.test/pr", "http://github.test/pr", 'p'},
		{"github", "https://proof.test/page", "https://github.test/pr", "https://github.test/pr", 'g'},
		{"javascript", "javascript:alert(1)", "", "", 'p'},
		{"file", "file:///tmp/proof", "", "", 'p'},
		{"relative", "/proof", "", "", 'p'},
		{"hostless", "https:///proof", "", "", 'p'},
		{"empty", "", "", "", 'p'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := queueRow("proof", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 1, 10)
			row.Session.PrUrl = proto.String(tc.pr)
			row.Session.Attention.ProofUrl = tc.proof
			m := queueModel(&queueScreenClient{}, row)
			opened := ""
			m.openURL = func(url string) error { opened = url; return nil }
			m, cmd := queueKey(m, tc.key)
			if cmd != nil {
				cmd()
			}
			if opened != tc.want {
				t.Fatalf("opened %q want %q", opened, tc.want)
			}
			if tc.want == "" && m.status == "" {
				t.Fatal("missing URL did not set status")
			}
		})
	}
}

func TestQueueMergePinsDisplayedHeadAndParkCodes_Sessions_AC209302(t *testing.T) {
	row := queueRow("merge", pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, 1, 10)
	row.Session.Attention.Kind = pb.AttentionKind_ATTENTION_KIND_REVIEW
	row.Session.Attention.Actions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE}
	row.Session.Attention.ParkCodes = []string{"human-review:ui", "human-review:scope"}
	c := &queueScreenClient{session: proto.CloneOf(row.Session)}
	m := queueModel(c, row)
	m, cmd := queueKey(m, 'm')
	m = queueRun(t, m, cmd)
	if !m.action.Confirm.Active || c.mergedHead != "" {
		t.Fatal("merge should await confirmation")
	}
	m, cmd = queueKey(m, 'y')
	m = queueRun(t, m, cmd)
	if c.mergedID != "merge" || c.mergedHead != "head-shown" || !c.mergedApprove || !reflect.DeepEqual(c.mergedCodes, row.Session.Attention.ParkCodes) {
		t.Fatalf("merge request id=%q head=%q approve=%v codes=%v", c.mergedID, c.mergedHead, c.mergedApprove, c.mergedCodes)
	}
	if m.action.Active() {
		t.Fatal("completed merge left action active")
	}
}

func TestQueueMergeRefusesMovedAndUnofferedHeads(t *testing.T) {
	for _, tc := range []string{"moved", "unoffered", "audit", "fresh-unoffered"} {
		t.Run(tc, func(t *testing.T) {
			row := queueRow("merge", pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, 1, 10)
			row.Session.Attention.Actions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE}
			c := &queueScreenClient{session: proto.CloneOf(row.Session)}
			switch tc {
			case "moved":
				c.session.PrDisplayHeadSha = "new-head"
			case "unoffered":
				row.Session.Attention.Actions = nil
			case "audit":
				row.IsAuditSample = true
			case "fresh-unoffered":
				c.session.Attention.Actions = nil
			}
			m := queueModel(c, row)
			m, cmd := queueKey(m, 'm')
			if cmd != nil {
				m = queueRun(t, m, cmd)
			}
			if c.mergedHead != "" || m.action.Active() {
				t.Fatal("refused row offered merge")
			}
			if tc == "moved" && m.status != ui.HeadMovedError(42).Error() {
				t.Fatalf("moved head status: %q", m.status)
			}
			if (tc == "unoffered" || tc == "audit") && c.sessionReads != 0 {
				t.Fatal("unoffered merge fetched session")
			}
		})
	}
}

func TestQueueAcceptBlockerAndCancelStages(t *testing.T) {
	for _, stop := range []string{"loading", "picker", "reason", "confirmation", "accept"} {
		t.Run(stop, func(t *testing.T) {
			row := queueRow("blocked", pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED, 1, 10)
			c := &queueScreenClient{blockers: &pb.ListSessionPRBlockersResponse{HeadSha: "picker-head", Blockers: []*pb.VerifyBlocker{{Id: "F-1", Summary: "first"}, {Id: "F-2", Summary: "second"}}}}
			m := queueModel(c, row)
			m, cmd := queueKey(m, 'a')
			if !m.accept.Active() {
				t.Fatal("blocked row did not open accept")
			}
			cancel := func() {
				t.Helper()
				m, _ = queueKey(m, tea.KeyEscape)
				if m.accept.Active() || len(c.accepted) != 0 {
					t.Fatal("cancel accepted a blocker or left flow active")
				}
			}
			if stop == "loading" {
				cancel()
				m = queueRun(t, m, cmd)
				if m.accept.Active() {
					t.Fatal("late response reopened cancelled flow")
				}
				return
			}
			m = queueRun(t, m, cmd)
			if stop == "picker" {
				cancel()
				return
			}
			m, _ = queueKey(m, tea.KeyDown)
			m, _ = queueKey(m, tea.KeyEnter)
			if !m.TextEntryActive() {
				t.Fatal("reason step is not text entry")
			}
			if stop == "reason" {
				cancel()
				return
			}
			m = queueUpdate(m, tea.PasteMsg{Content: "safe to defer"})
			m, _ = queueKey(m, tea.KeyEnter)
			if m.TextEntryActive() {
				t.Fatal("confirmation remained text entry")
			}
			if stop == "confirmation" {
				cancel()
				return
			}
			m, cmd = queueKey(m, 'y')
			m = queueRun(t, m, cmd)
			if len(c.accepted) != 1 {
				t.Fatalf("accepted %d blockers", len(c.accepted))
			}
			req := c.accepted[0]
			if req.GetId() != "blocked" || req.GetExpectedHeadSha() != "picker-head" || req.GetBlockerId() != "F-2" || req.GetReason() != "safe to defer" {
				t.Fatalf("accept request: %v", req)
			}
			if !strings.Contains(m.status, "BOS-9001") {
				t.Fatalf("follow-up missing: %q", m.status)
			}
		})
	}
	m := queueModel(&queueScreenClient{}, queueRow("low", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 1, 10))
	m, cmd := queueKey(m, 'a')
	if cmd != nil || m.accept.Active() {
		t.Fatal("non-blocked row opened accept")
	}
}

func TestQueueProofDoesNotForgetUnansweredMerge(t *testing.T) {
	row := queueRow("waiting", pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, 1, 10)
	row.Session.Attention.Actions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE}
	c := &queueScreenClient{}
	m := queueModel(c, row)
	m.action = ui.AwaitingMerge("waiting", "Waiting for merge")
	m, cmd := queueKey(m, 'p')
	m = queueRun(t, m, cmd)
	m, cmd = queueKey(m, 'm')
	if !m.action.MergeUnresolved() || cmd != nil || c.sessionReads != 0 {
		t.Fatal("proof opening forgot an unresolved merge and enabled a second merge")
	}
}

func TestQueueMergePreflightCannotReplaceBusyMerge(t *testing.T) {
	row := queueRow("merge", pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, 1, 10)
	row.Session.Attention.Actions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE}
	c := &queueScreenClient{session: proto.CloneOf(row.Session)}
	m := queueModel(c, row)
	m, first := queueKey(m, 'm')
	m, second := queueKey(m, 'm')
	if second != nil {
		t.Fatal("second merge key launched overlapping preflight")
	}
	response := first()
	m = queueUpdate(m, response)
	if !m.action.Confirm.Active {
		t.Fatal("first preflight did not offer confirmation")
	}
	m, merge := queueKey(m, 'y')
	if !m.action.Busy || merge == nil {
		t.Fatal("merge did not start")
	}
	m = queueUpdate(m, response)
	if !m.action.Busy || m.action.Confirm.Active {
		t.Fatal("delayed preflight replaced running merge")
	}
	m, duplicate := queueKey(m, 'y')
	if duplicate != nil {
		t.Fatal("delayed preflight enabled duplicate merge")
	}
	m = queueRun(t, m, merge)
	if c.mergedID != "merge" || c.sessionReads != 1 {
		t.Fatalf("merge=%s reads=%d", c.mergedID, c.sessionReads)
	}
}
