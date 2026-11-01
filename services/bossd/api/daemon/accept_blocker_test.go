package server

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

type acceptBlockerVerbs struct {
	verifyVerbsFake
	result   domain.VerifyAcceptResult
	blockers domain.VerifyBlockers
	pin      domain.VerifyAccept
	target   domain.VerifyTarget
	calls    int
	head     string
	ctxErr   error
	deadline time.Time
}

func (v *acceptBlockerVerbs) Accept(ctx context.Context, target domain.VerifyTarget, pin domain.VerifyAccept) (domain.VerifyAcceptResult, error) {
	v.calls++
	v.pin = pin
	v.target = target
	v.ctxErr = ctx.Err()
	v.deadline, _ = ctx.Deadline()
	return v.result, v.err
}
func (v *acceptBlockerVerbs) Blockers(ctx context.Context, target domain.VerifyTarget, head string) (domain.VerifyBlockers, error) {
	v.calls++
	v.head = head
	v.target = target
	v.ctxErr = ctx.Err()
	return v.blockers, v.err
}
func acceptBlockerServer(t *testing.T, v *acceptBlockerVerbs) *Server {
	t.Helper()
	sess := verifyTestSession(t)
	s := newVerifyTestServer(sess, nil, &v.verifyVerbsFake)
	s.newVerifyVerbs = func([]string) verifyVerbs { return v }
	s.displayTracker = status.NewDisplayTracker()
	s.displayTracker.Set(sess.ID, vcs.DisplayInfo{HeadSHA: headPin})
	s.repairLease = status.NewRepairLeaseManager()
	return s
}
func TestAcceptSessionPRBlocker_PinsAndRelays_Sessions_AC213302(t *testing.T) {
	for _, reason := range []string{"", "head-moved", "repair-active", "pr-unreadable", "not-open", "comments-unreadable", "no-blockers", "blocker-not-listed", "blocker-not-acceptable", "sent-back", "tracker-create-unavailable", "tracker-create-failed", "tracker-create-indeterminate", "comment-failed"} {
		t.Run(reason, func(t *testing.T) {
			v := &acceptBlockerVerbs{result: domain.VerifyAcceptResult{Accepted: true, Already: true, DryRun: true, Head: headPin, Blocker: "review:123456abcdef", TicketIdentifier: "BOS-123", TicketURL: "https://ticket", CommentURL: "https://comment", ExistingTicket: true, Writes: []string{"planned"}}}
			s := acceptBlockerServer(t, v)
			req := &pb.AcceptSessionPRBlockerRequest{Id: "s1", ExpectedHeadSha: headPin, BlockerId: v.result.Blocker, Reason: "operator reason", IsDryRun: true}
			switch reason {
			case "head-moved":
				req.ExpectedHeadSha = strings.Repeat("f", 40)
			case "repair-active":
				if err := s.repairLease.Acquire("s1", "repair"); err != nil {
					t.Fatal(err)
				}
			case "":
			default:
				v.result = domain.VerifyAcceptResult{Reason: reason, Detail: "detail", Head: headPin}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			resp, err := s.AcceptSessionPRBlocker(ctx, connect.NewRequest(req))
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.Reason != reason || resp.Msg.IsAccepted != (reason == "") {
				t.Fatalf("response %v", resp.Msg)
			}
			if reason == "head-moved" || reason == "repair-active" {
				if v.calls != 0 {
					t.Fatal("verb ran on refusal")
				}
				return
			}
			if v.calls != 1 || v.ctxErr != nil || v.pin.Reason != req.Reason || v.pin.Head != headPin || v.pin.Blocker != req.BlockerId || !v.target.DryRun || v.deadline.IsZero() {
				t.Fatalf("execution %+v", v)
			}
			if reason == "" && (!resp.Msg.IsAlreadyAccepted || !resp.Msg.IsExistingTicket || !resp.Msg.IsDryRun || resp.Msg.HeadSha != v.result.Head || resp.Msg.BlockerId != v.result.Blocker || resp.Msg.TicketIdentifier != v.result.TicketIdentifier || resp.Msg.TicketUrl != v.result.TicketURL || resp.Msg.CommentUrl != v.result.CommentURL || !reflect.DeepEqual(resp.Msg.PlannedWrites, v.result.Writes)) {
				t.Fatalf("relay %v", resp.Msg)
			}
			if reason != "" && (resp.Msg.Detail != "detail" || resp.Msg.HeadSha != headPin) {
				t.Fatalf("refusal relay %v", resp.Msg)
			}
		})
	}
}
func TestAcceptBlockerInvalidInputAndTimeout(t *testing.T) {
	for _, field := range []string{"head", "empty blocker", "long blocker", "control blocker", "empty reason", "long reason", "control reason"} {
		t.Run(field, func(t *testing.T) {
			v := &acceptBlockerVerbs{}
			s := acceptBlockerServer(t, v)
			req := &pb.AcceptSessionPRBlockerRequest{Id: "s1", ExpectedHeadSha: headPin, BlockerId: "review:123456abcdef", Reason: "reason"}
			switch field {
			case "head":
				req.ExpectedHeadSha = "bad"
			case "empty blocker":
				req.BlockerId = ""
			case "long blocker":
				req.BlockerId = strings.Repeat("😀", 257)
			case "control blocker":
				req.BlockerId = "a\n"
			case "empty reason":
				req.Reason = " \t"
			case "long reason":
				req.Reason = strings.Repeat("😀", 2001)
			case "control reason":
				req.Reason = "reason\x00"
			}
			_, err := s.AcceptSessionPRBlocker(context.Background(), connect.NewRequest(req))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || v.calls != 0 {
				t.Fatalf("err=%v calls=%d", err, v.calls)
			}
		})
	}
	v := &acceptBlockerVerbs{}
	v.err = domain.ErrVerifyNodeTimeout
	s := acceptBlockerServer(t, v)
	_, err := s.AcceptSessionPRBlocker(context.Background(), connect.NewRequest(&pb.AcceptSessionPRBlockerRequest{Id: "s1", ExpectedHeadSha: headPin, BlockerId: "b", Reason: "r"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatal(err)
	}
}
func TestListSessionPRBlockers_ReviewFindingsAtDisplayedHead_Sessions_AC213304(t *testing.T) {
	for _, reason := range []string{"", "no-blockers", "comments-unreadable", "no-acceptable-blockers", "head-unknown"} {
		t.Run(reason, func(t *testing.T) {
			findings := []domain.VerifyBlocker{{ID: "review:123456abcdef", Kind: "review-finding", Severity: "P1", File: "a.go", Line: 12, Summary: "summary", Text: "text"}, {ID: "review:abcdef123456", Kind: "review-finding", File: "b.go"}}
			v := &acceptBlockerVerbs{blockers: domain.VerifyBlockers{Outcome: "written", Head: headPin, Blockers: append([]domain.VerifyBlocker{{ID: "check", Kind: "check"}}, findings...)}}
			s := acceptBlockerServer(t, v)
			switch reason {
			case "no-blockers":
				v.blockers.Outcome = "absent"
			case "comments-unreadable":
				v.blockers.Outcome = "unreadable"
			case "no-acceptable-blockers":
				v.blockers.Blockers = v.blockers.Blockers[:1]
			case "head-unknown":
				s.displayTracker = status.NewDisplayTracker()
			}
			resp, err := s.ListSessionPRBlockers(context.Background(), connect.NewRequest(&pb.ListSessionPRBlockersRequest{Id: "s1"}))
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.Reason != reason {
				t.Fatalf("response %v", resp.Msg)
			}
			if reason == "head-unknown" {
				if v.calls != 0 || resp.Msg.HeadSha != "" {
					t.Fatalf("unknown %v calls %d", resp.Msg, v.calls)
				}
				return
			}
			if resp.Msg.HeadSha != headPin || v.head != headPin || v.calls != 1 {
				t.Fatalf("head %v %+v", resp.Msg, v)
			}
			if reason != "" {
				if len(resp.Msg.Blockers) != 0 {
					t.Fatal(resp.Msg)
				}
				return
			}
			if len(resp.Msg.Blockers) != 2 {
				t.Fatal(resp.Msg)
			}
			for i, b := range resp.Msg.Blockers {
				want := findings[i]
				if b.Id != want.ID || b.Severity != want.Severity || b.FilePath != want.File || int(b.LineNumber) != want.Line || b.Summary != want.Summary || b.Text != want.Text {
					t.Fatalf("blocker %v", b)
				}
			}
		})
	}
}

type blockerResolutionStore struct {
	sessionStore
	deadline time.Time
	ctxErr   error
}

func (s *blockerResolutionStore) Get(ctx context.Context, id string) (*models.Session, error) {
	s.deadline, _ = ctx.Deadline()
	s.ctxErr = ctx.Err()
	return s.sessionStore.Get(ctx, id)
}
func TestListSessionPRBlockers_BoundsResolutionAndKeepsCallerCancellation(t *testing.T) {
	v := &acceptBlockerVerbs{blockers: domain.VerifyBlockers{Outcome: "absent"}}
	srv := acceptBlockerServer(t, v)
	store := &blockerResolutionStore{sessionStore: srv.sessions}
	srv.sessions = store
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.ListSessionPRBlockers(ctx, connect.NewRequest(&pb.ListSessionPRBlockersRequest{Id: "s1"})); err != nil {
		t.Fatal(err)
	}
	if store.deadline.IsZero() || time.Until(store.deadline) > domain.VerifyVerbTimeout {
		t.Fatalf("target resolution deadline=%v", store.deadline)
	}
	if store.ctxErr != context.Canceled || v.ctxErr != context.Canceled {
		t.Fatalf("caller cancellation: target=%v verb=%v", store.ctxErr, v.ctxErr)
	}
}
