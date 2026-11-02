package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/vcs"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/prstate"
)

// fakePRWaiter records each wait and answers res/err, after hold when set.
type fakePRWaiter struct {
	res  prstate.WaitResult
	err  error
	hold time.Duration
	reqs []prstate.WaitRequest
}

func (f *fakePRWaiter) Wait(_ context.Context, req prstate.WaitRequest) (prstate.WaitResult, error) {
	f.reqs = append(f.reqs, req)
	if f.hold > 0 {
		<-time.After(f.hold)
	}
	return f.res, f.err
}

func prWaitServer(w PRWaiter) *Server {
	return &Server{prWaits: w, repos: prStateRepos{repos: []*domain.Repo{{ID: "r1", OriginURL: "git@github.com:Acme/Widgets.git"}}}}
}

func waitPRState(srv *Server, req *pb.WaitPRStateRequest) (*pb.WaitPRStateResponse, error) {
	resp, err := srv.WaitPRState(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func checksSettledRequest(timeout int32) *pb.WaitPRStateRequest {
	return &pb.WaitPRStateRequest{Repo: "acme/widgets", PrNumber: 7, Condition: pb.PRWaitCondition_PR_WAIT_CONDITION_CHECKS_SETTLED, TimeoutSeconds: timeout}
}

func TestWaitPRStateClampsTheTimeoutAndForwardsTheRequest(t *testing.T) {
	for _, tc := range []struct {
		requested int32
		want      time.Duration
	}{
		{0, config.PRWaitDefault},
		{90, 90 * time.Second},
		{int32(config.PRWaitCeiling/time.Second) * 4, config.PRWaitCeiling},
	} {
		w := &fakePRWaiter{res: prstate.WaitResult{Outcome: prstate.WaitMet, Snapshot: prstate.Snapshot{Status: vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "abc"}}}}
		req := checksSettledRequest(tc.requested)
		req.Repo, req.SinceHeadSha, req.ShouldBypassCache, req.ShouldAcceptNoGateRan = "https://github.com/ACME/widgets.git", "old", true, true
		resp, err := waitPRState(prWaitServer(w), req)
		if err != nil {
			t.Fatalf("timeout %d: WaitPRState: %v", tc.requested, err)
		}
		want := prstate.WaitRequest{Repo: "acme/widgets", PR: 7, Condition: prstate.WaitChecksSettled, SinceHeadSHA: "old", Timeout: tc.want, Fresh: true, AcceptNoGateRan: true}
		if len(w.reqs) != 1 || w.reqs[0] != want {
			t.Fatalf("timeout %d: waits = %+v, want %+v", tc.requested, w.reqs, want)
		}
		if resp.GetTimeoutSeconds() != int32(tc.want/time.Second) || resp.GetOutcome() != pb.PRWaitOutcome_PR_WAIT_OUTCOME_MET {
			t.Fatalf("timeout %d: response timeout=%d outcome=%s", tc.requested, resp.GetTimeoutSeconds(), resp.GetOutcome())
		}
		if s := resp.GetSnapshot(); s.GetRepo() != "acme/widgets" || s.GetHeadSha() != "abc" || s.GetState() != pb.PRState_PR_STATE_OPEN {
			t.Fatalf("timeout %d: snapshot = %+v", tc.requested, s)
		}
	}
}

func TestWaitPRStateRefusesBeforeAnyWait(t *testing.T) {
	unspecified := checksSettledRequest(0)
	unspecified.Condition = pb.PRWaitCondition_PR_WAIT_CONDITION_UNSPECIFIED
	unregistered := checksSettledRequest(0)
	unregistered.Repo = "acme/gadgets"
	noPR := checksSettledRequest(0)
	noPR.PrNumber = 0
	for _, tc := range []struct {
		name string
		req  *pb.WaitPRStateRequest
		want connect.Code
	}{
		{"unspecified condition", unspecified, connect.CodeInvalidArgument},
		{"negative timeout", checksSettledRequest(-1), connect.CodeInvalidArgument},
		{"missing PR number", noPR, connect.CodeInvalidArgument},
		{"unregistered repo", unregistered, connect.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &fakePRWaiter{}
			_, err := waitPRState(prWaitServer(w), tc.req)
			if connect.CodeOf(err) != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if len(w.reqs) != 0 {
				t.Fatalf("waited %+v, want no wait", w.reqs)
			}
		})
	}
	if _, err := waitPRState(&Server{}, checksSettledRequest(0)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("nil hub err = %v, want failed_precondition", err)
	}
}

func TestWaitPRStateMapsWaitErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want connect.Code
	}{
		{vcs.ErrRateLimited, connect.CodeResourceExhausted},
		{prstate.ErrWaitHubClosed, connect.CodeUnavailable},
		{errors.New("boom"), connect.CodeUnavailable},
	} {
		if _, err := waitPRState(prWaitServer(&fakePRWaiter{err: tc.err}), checksSettledRequest(0)); connect.CodeOf(err) != tc.want {
			t.Fatalf("wait error %v: err = %v, want %s", tc.err, err, tc.want)
		}
	}
}

func TestPRWaitEnumNumbering(t *testing.T) {
	for _, p := range []struct {
		hub   int
		proto int32
	}{
		{int(prstate.WaitChecksSettled), int32(pb.PRWaitCondition_PR_WAIT_CONDITION_CHECKS_SETTLED)},
		{int(prstate.WaitMergeable), int32(pb.PRWaitCondition_PR_WAIT_CONDITION_MERGEABLE)},
		{int(prstate.WaitHeadChanged), int32(pb.PRWaitCondition_PR_WAIT_CONDITION_HEAD_CHANGED)},
		{int(prstate.WaitMet), int32(pb.PRWaitOutcome_PR_WAIT_OUTCOME_MET)},
		{int(prstate.WaitUnreachable), int32(pb.PRWaitOutcome_PR_WAIT_OUTCOME_UNREACHABLE)},
		{int(prstate.WaitTimedOut), int32(pb.PRWaitOutcome_PR_WAIT_OUTCOME_TIMED_OUT)},
	} {
		if int32(p.hub) != p.proto {
			t.Fatalf("hub value %d != proto value %d", p.hub, p.proto)
		}
	}
}

// TestWaitPRStateOutlivesTheWriteTimeout serves the RPC over HTTP with a
// write timeout shorter than the wait: through the exemption the answer
// arrives, and without it the transport cuts the response off.
func TestWaitPRStateOutlivesTheWriteTimeout(t *testing.T) {
	if _, ok := writeDeadlineExemptProcedures[bossanovav1connect.DaemonServiceWaitPRStateProcedure]; !ok {
		t.Fatal("WaitPRState is not in writeDeadlineExemptProcedures")
	}
	for _, exempt := range []bool{true, false} {
		w := &fakePRWaiter{res: prstate.WaitResult{Outcome: prstate.WaitMet}, hold: 400 * time.Millisecond}
		mux := http.NewServeMux()
		mux.Handle(bossanovav1connect.NewDaemonServiceHandler(prWaitServer(w)))
		var handler http.Handler = mux
		if exempt {
			handler = withStreamingWriteDeadlineOverride(mux)
		}
		ts := httptest.NewUnstartedServer(handler)
		ts.Config.WriteTimeout = 50 * time.Millisecond
		ts.Start()
		client := bossanovav1connect.NewDaemonServiceClient(ts.Client(), ts.URL)
		resp, err := client.WaitPRState(context.Background(), connect.NewRequest(checksSettledRequest(0)))
		ts.Close()
		if exempt && (err != nil || resp.Msg.GetOutcome() != pb.PRWaitOutcome_PR_WAIT_OUTCOME_MET) {
			t.Fatalf("exempt: WaitPRState = %v, %v; want met", resp, err)
		}
		if !exempt && err == nil {
			t.Fatal("without the exemption the write timeout did not cut the wait off; the test cannot tell the exemption apart")
		}
	}
}
