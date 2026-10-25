package evidence

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
)

func sendBackOn(head string) string {
	return "**Sent back by the operator**\n\n<!-- boss-send-back head=" + head + " -->"
}

func approveOn(head, waives string) string {
	marker := "<!-- boss-approve head=" + head
	if waives != "" {
		marker += " waives=" + waives
	}
	return "**Approved by the operator**\n\n" + marker + " -->"
}

// TestReader_SentBackIsTheNewestOperatorRecordOnTheHead pins the operator
// decision verify-gate.mjs reads (operatorDecision): the newest trusted
// approve or send-back record on the current head decides it, an approval
// after a send-back lifts it, and a record on another head, by an author who
// is not an owner, member or collaborator, not on the comment's last line, or
// waiving an unknown code does not count.
func TestReader_SentBackIsTheNewestOperatorRecordOnTheHead(t *testing.T) {
	trusted := func(body string, at time.Time) vcs.PRComment {
		return vcs.PRComment{Body: body, AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: at, CreatedAt: at}
	}
	unposted := func(body string, at time.Time) vcs.PRComment {
		return vcs.PRComment{Body: body, AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: at}
	}
	newer, older := recordedAt.Add(time.Hour), recordedAt
	for _, tc := range []struct {
		name     string
		comments []vcs.PRComment
		want     bool
	}{
		{"no record", []vcs.PRComment{}, false},
		{"a send-back", []vcs.PRComment{trusted(sendBackOn(headA), older)}, true},
		{"an upper-case head", []vcs.PRComment{trusted(sendBackOn(strings.ToUpper(headA)), older)}, true},
		{"an approval lifts an older send-back", []vcs.PRComment{trusted(approveOn(headA, ""), newer), trusted(sendBackOn(headA), older)}, false},
		{"a send-back after an approval", []vcs.PRComment{trusted(sendBackOn(headA), newer), trusted(approveOn(headA, "ledger-open"), older)}, true},
		{"a send-back of another head", []vcs.PRComment{trusted(sendBackOn(headB), older)}, false},
		{"an untrusted send-back", []vcs.PRComment{{Body: sendBackOn(headA), AuthorLogin: "drive-by", AuthorAssociation: "NONE", WrittenAt: older}}, false},
		{"a review bot is not an operator", []vcs.PRComment{{Body: sendBackOn(headA), AuthorLogin: "coderabbitai[bot]", AuthorAssociation: "NONE", WrittenAt: older}}, false},
		{"a quoted marker that is not the last line", []vcs.PRComment{trusted(sendBackOn(headA)+"\n\nquoted above", older)}, false},
		{"an approval waiving an unknown code does not lift it", []vcs.PRComment{trusted(approveOn(headA, "made-up"), newer), trusted(sendBackOn(headA), older)}, true},
		{"records without a posting time cannot be ordered, so they read as sent back", []vcs.PRComment{unposted(approveOn(headA, ""), newer), unposted(sendBackOn(headA), older)}, true},
		{"a lone approval without a posting time reads as sent back", []vcs.PRComment{unposted(approveOn(headA, ""), newer)}, true},
		{"an approval without a posting time beside a dated one reads as sent back", []vcs.PRComment{trusted(approveOn(headA, ""), newer), unposted(approveOn(headA, ""), older)}, true},
		{"an undated record on another head changes nothing", []vcs.PRComment{trusted(approveOn(headA, ""), newer), unposted(approveOn(headB, ""), older)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestReader(&fakeFinder{comments: tc.comments})
			r.Observe(context.Background(), parked(headA))
			if got := r.SentBack("s1", headA); got != tc.want {
				t.Fatalf("SentBack = %v, want %v", got, tc.want)
			}
			if r.SentBack("s1", headB) {
				t.Fatal("SentBack answered for a head that is not the session's current head")
			}
		})
	}
}

// TestReader_AFailedReadKeepsTheSendBack checks a read that cannot reach the
// comments says nothing about them, so the known send-back stands.
func TestReader_AFailedReadKeepsTheSendBack(t *testing.T) {
	f := &fakeFinder{comments: []vcs.PRComment{{Body: sendBackOn(headA), AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: recordedAt}}}
	r, c := newTestReader(f)
	r.Observe(context.Background(), parked(headA))
	f.mu.Lock()
	f.err = context.DeadlineExceeded
	f.mu.Unlock()
	c.t = c.t.Add(DefaultRefreshInterval)
	r.Observe(context.Background(), parked(headA))
	if f.count() != 2 || !r.SentBack("s1", headA) {
		t.Fatalf("reads = %d, SentBack = %v; want the failed second read to keep the send-back", f.count(), r.SentBack("s1", headA))
	}
}
