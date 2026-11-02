package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

func TestFollowUpSessionPRNotes_Selection(t *testing.T) {
	for _, req := range []*pb.FollowUpSessionPRNotesRequest{
		{Id: "s1"}, {Id: "s1", ShouldFollowUpAll: true, VerifyNoteIds: []string{"note:0123456789ab"}}, {Id: "s1", VerifyNoteIds: []string{"bad"}},
	} {
		verbs := &verifyVerbsFake{}
		srv := newVerifyTestServer(verifyTestSession(t), nil, verbs)
		_, err := srv.FollowUpSessionPRNotes(context.Background(), connect.NewRequest(req))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || verbs.builtCount != 0 {
			t.Fatalf("invalid selection ran verb: %v, built %d", err, verbs.builtCount)
		}
	}
}

func TestFollowUpSessionPRNotes_DetachedCredentialsAndCheckout(t *testing.T) {
	sess := verifyTestSession(t)
	sess.WorktreePath += "/gone"
	repo := &models.Repo{ID: "r1", OriginURL: "git@github.com:Acme/App.git", LocalPath: t.TempDir(), LinearAPIKey: "repo-key"}
	verbs := &verifyVerbsFake{followUp: domain.VerifyFollowUpResult{OK: true, Comment: "current", Notes: []domain.VerifyNoteFollowUp{{VerifyNoteID: "note:0123456789ab", Outcome: "created", Ticket: &domain.VerifyFollowUpTicket{Identifier: "BOS-7", URL: "https://tracker/7"}, IsLinked: true}}}}
	srv := newVerifyTestServer(sess, repo, verbs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := srv.FollowUpSessionPRNotes(ctx, connect.NewRequest(&pb.FollowUpSessionPRNotesRequest{Id: "s1", ShouldFollowUpAll: true}))
	if err != nil {
		t.Fatal(err)
	}
	if verbs.followCtxErr != nil || verbs.followDeadline.IsZero() || time.Until(verbs.followDeadline) > domain.VerifyVerbTimeout {
		t.Fatalf("not detached/bounded: %v %v", verbs.followCtxErr, verbs.followDeadline)
	}
	if len(verbs.targets) != 1 || verbs.targets[0].Dir != repo.LocalPath || !verbs.followSelection.All {
		t.Fatalf("target/selection: %+v %+v", verbs.targets, verbs.followSelection)
	}
	if got, _ := envValue(verbs.env, "LINEAR_API_KEY"); got != "repo-key" {
		t.Fatalf("tracker key: %q", got)
	}
	for _, key := range srv.verifyProofKeys {
		if got, _ := envValue(verbs.env, key); got != "" {
			t.Fatalf("proof key %s leaked", key)
		}
	}
	r := resp.Msg.Result
	if !r.IsOk || len(r.Notes) != 1 || r.Notes[0].TicketIdentifier != "BOS-7" || !r.Notes[0].IsLinked {
		t.Fatalf("result: %+v", r)
	}
}

func TestFollowUpSessionPRNotes_ErrorsAndRefusal(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code connect.Code
	}{{domain.ErrVerifyNodeTimeout, connect.CodeDeadlineExceeded}, {domain.ErrVerifyToolboxMissing, connect.CodeFailedPrecondition}, {errors.New("bad output"), connect.CodeInternal}} {
		verbs := &verifyVerbsFake{err: tc.err}
		srv := newVerifyTestServer(verifyTestSession(t), nil, verbs)
		_, err := srv.FollowUpSessionPRNotes(context.Background(), connect.NewRequest(&pb.FollowUpSessionPRNotesRequest{Id: "s1", ShouldFollowUpAll: true}))
		if connect.CodeOf(err) != tc.code {
			t.Fatalf("%v: %v", tc.err, err)
		}
	}
	verbs := &verifyVerbsFake{followUp: domain.VerifyFollowUpResult{Reason: "unknown-note", UnknownVerifyNoteIDs: []string{"note:0123456789ab"}}}
	srv := newVerifyTestServer(verifyTestSession(t), nil, verbs)
	r, err := srv.FollowUpSessionPRNotes(context.Background(), connect.NewRequest(&pb.FollowUpSessionPRNotesRequest{Id: "s1", VerifyNoteIds: []string{"note:0123456789ab", "note:0123456789ab"}}))
	if err != nil || r.Msg.Result.IsOk || len(r.Msg.Result.UnknownVerifyNoteIds) != 1 || len(verbs.followSelection.VerifyNoteIDs) != 1 {
		t.Fatalf("refusal: %v %+v", err, r)
	}
}

func TestFollowUpCallerGate_Sessions_AC213803(t *testing.T) {
	for _, kind := range []domain.CallerKind{domain.CallerOperator, domain.CallerAgentChat, domain.CallerDaemonChild, domain.CallerUnattested} {
		a := &gateAuthority{principal: domain.CallerPrincipal{Kind: kind, AgentSessionID: "chat-follow"}}
		i := NewCallerInterceptor(a, nil)
		err := i.check(a.ConnContext(context.Background(), nil), bossanovav1connect.DaemonServiceFollowUpSessionPRNotesProcedure)
		if kind == domain.CallerOperator {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("%s: %v", kind, err)
		}
		if kind == domain.CallerAgentChat && (!strings.Contains(err.Error(), "chat-follow") || !strings.Contains(err.Error(), "your own terminal")) {
			t.Fatal(err)
		}
	}
}
