package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/status"
)

type sendBackEvidence struct {
	stubEvidence
	digest string
}

func (e sendBackEvidence) Proof(string, string) (evidence.ProofRecord, bool) {
	return evidence.ProofRecord{Digest: e.digest}, e.digest != ""
}

type sendBackVerbs struct {
	verifyVerbsFake
	calls  int
	result domain.VerifySendBackResult
	pin    domain.VerifySendBack
	ctxErr error
}

func (v *sendBackVerbs) SendBack(ctx context.Context, _ domain.VerifyTarget, pin domain.VerifySendBack) (domain.VerifySendBackResult, error) {
	v.calls++
	v.pin = pin
	v.ctxErr = ctx.Err()
	return v.result, nil
}

func TestSendBackPinsAndRelay_Sessions_AC148_AC149(t *testing.T) {
	for _, reason := range []string{"", "head-moved", "proof-moved", "repair-active", "not-open", "comments-unreadable"} {
		t.Run(reason, func(t *testing.T) {
			sess := verifyTestSession(t)
			verbs := &sendBackVerbs{result: domain.VerifySendBackResult{SentBack: true, Head: headPin, CommentURL: "https://example/comment", AutoRepair: true}}
			srv := newVerifyTestServer(sess, nil, &verbs.verifyVerbsFake)
			srv.newVerifyVerbs = func([]string) verifyVerbs { return verbs }
			srv.displayTracker = status.NewDisplayTracker()
			srv.displayTracker.Set(sess.ID, vcs.DisplayInfo{HeadSHA: headPin})
			digest := "sha256:" + strings.Repeat("a", 64)
			srv.evidence = sendBackEvidence{digest: digest}
			srv.repairLease = status.NewRepairLeaseManager()
			req := &pb.SendBackToRepairRequest{Id: sess.ID, ExpectedHeadSha: headPin, ExpectedProofDigest: digest, Note: "please fix `code`"}
			switch reason {
			case "head-moved":
				req.ExpectedHeadSha = strings.Repeat("f", 40)
			case "proof-moved":
				req.ExpectedProofDigest = ""
			case "repair-active":
				if err := srv.repairLease.Acquire(sess.ID, "repair"); err != nil {
					t.Fatal(err)
				}
			case "not-open", "comments-unreadable":
				verbs.result = domain.VerifySendBackResult{Reason: reason}
			}
			resp, err := srv.SendBackToRepair(context.Background(), connect.NewRequest(req))
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.Reason != reason || resp.Msg.IsSentBack != (reason == "") {
				t.Fatalf("response: %v", resp.Msg)
			}
			if reason == "head-moved" || reason == "proof-moved" || reason == "repair-active" {
				if verbs.calls != 0 {
					t.Fatal("verb ran on pin refusal")
				}
			} else if verbs.calls != 1 {
				t.Fatal("verb did not run")
			}
			if reason == "" && (resp.Msg.CommentUrl != "https://example/comment" || !resp.Msg.IsAutoRepairEnabled || verbs.pin.Note != req.Note) {
				t.Fatalf("relay: %v %v", resp.Msg, verbs.pin)
			}
		})
	}
}
func TestSendBackInvalidInput(t *testing.T) {
	for _, tc := range []struct{ name, head, digest, note string }{
		{"head", "bad", "", "note"}, {"digest", headPin, "bad", "note"}, {"empty", headPin, "", " \n\t"}, {"long", headPin, "", strings.Repeat("😀", 2001)}, {"control", headPin, "", "note\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Server{}).SendBackToRepair(context.Background(), connect.NewRequest(&pb.SendBackToRepairRequest{Id: "s", ExpectedHeadSha: tc.head, ExpectedProofDigest: tc.digest, Note: tc.note}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func TestSendBackMissingTargetAndDetachedCaller(t *testing.T) {
	for _, tc := range []struct {
		name string
		code connect.Code
	}{
		{"unknown", connect.CodeNotFound}, {"no PR", connect.CodeFailedPrecondition}, {"no worktree", connect.CodeFailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := verifyTestSession(t)
			if tc.name == "no PR" {
				sess.PRNumber = nil
			}
			if tc.name == "no worktree" {
				sess.WorktreePath = ""
			}
			if tc.name == "unknown" {
				sess = nil
			}
			srv := newVerifyTestServer(sess, nil, &verifyVerbsFake{})
			_, err := srv.SendBackToRepair(context.Background(), connect.NewRequest(&pb.SendBackToRepairRequest{Id: "s1", ExpectedHeadSha: headPin, Note: "note"}))
			if connect.CodeOf(err) != tc.code {
				t.Fatalf("error: %v", err)
			}
		})
	}
	t.Run("detached caller and absent proof", func(t *testing.T) {
		sess := verifyTestSession(t)
		verbs := &sendBackVerbs{result: domain.VerifySendBackResult{SentBack: true, Head: headPin, CommentURL: "https://example/comment"}}
		srv := newVerifyTestServer(sess, nil, &verbs.verifyVerbsFake)
		srv.newVerifyVerbs = func([]string) verifyVerbs { return verbs }
		srv.displayTracker = status.NewDisplayTracker()
		srv.displayTracker.Set(sess.ID, vcs.DisplayInfo{HeadSHA: headPin})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		resp, err := srv.SendBackToRepair(ctx, connect.NewRequest(&pb.SendBackToRepairRequest{Id: sess.ID, ExpectedHeadSha: headPin, Note: strings.Repeat("😀", 2000)}))
		if err != nil || !resp.Msg.IsSentBack || verbs.ctxErr != nil {
			t.Fatalf("response=%v err=%v verb context=%v", resp, err, verbs.ctxErr)
		}
	})
}
