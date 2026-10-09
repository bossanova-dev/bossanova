package server_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/testharness"
)

// The receipt must travel through the real provider/poller/tracker/RPC path.
// ListSessions supplies this same composite to the TUI and web clients.
func TestReadyHandoffProviderToListSessions(t *testing.T) {
	h := testharness.New(t)
	// The harness's lifecycle server does not wire its chat tracker into list
	// reads. Use the same real stores and display tracker with that input wired.
	srv := server.New(server.Config{Repos: h.Repos, Sessions: h.Sessions, AgentChats: h.AgentChats, DisplayTracker: h.DisplayTracker, ChatStatus: h.ChatTracker})
	_, handler := bossanovav1connect.NewDaemonServiceHandler(srv)
	rpc := httptest.NewServer(handler)
	t.Cleanup(rpc.Close)
	client := bossanovav1connect.NewDaemonServiceClient(rpc.Client(), rpc.URL)
	ctx, cancel := context.WithTimeout(h.Ctx(), 10*time.Second)
	defer cancel()
	const repoURL = "https://github.com/recurser/bossanova"
	const pr = 345
	repoID := h.SeedRepo(t, repoURL)
	sessionID := h.SeedSession(t, repoID, pr, pb.SessionState_SESSION_STATE_GREEN_DRAFT)
	const agentID = "ready-handoff-agent"
	if _, err := h.AgentChats.Create(ctx, db.CreateAgentChatParams{SessionID: sessionID, AgentSessionID: agentID, Title: "handoff"}); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"action":"created","issue":{"number":345,"pull_request":{"url":"https://api.github.com/repos/recurser/bossanova/pulls/345"}},"comment":{"body":"status refreshed","user":{"login":"dave"}},"repository":{"html_url":"https://github.com/recurser/bossanova"}}`)
	mergeable := true
	success := vcs.CheckConclusionSuccess
	ci := vcs.CheckResult{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &success}
	for _, tc := range []struct {
		name, head, verifyDescription, wantLabel string
		receipt, verify, waiting                 bool
		wantStatus                               pb.DisplayStatus
	}{
		{name: "build handoff", waiting: true, head: "head-a", receipt: true, wantLabel: "✓ ready", wantStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING},
		{name: "human push clears receipt", waiting: true, head: "head-b", wantLabel: "waiting", wantStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING},
		{name: "repair repost restores ready", waiting: true, head: "head-c", receipt: true, wantLabel: "✓ ready", wantStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING},
		{name: "verify claim outranks ready", head: "head-c", receipt: true, verify: true, verifyDescription: "claimed", wantLabel: "verifying", wantStatus: pb.DisplayStatus_DISPLAY_STATUS_VERIFYING},
		{name: "verify park outranks ready", head: "head-c", receipt: true, verify: true, verifyDescription: "needs human: always-human-path", wantLabel: "needs human", wantStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.Provider.SetPRStatus(pr, &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: tc.head, Mergeable: &mergeable, MergeStateStatus: vcs.MergeStateStatusClean})
			checks := []vcs.CheckResult{ci}
			if tc.verify {
				checks = append(checks, vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: tc.verifyDescription})
			}
			h.Provider.SetCheckResults(pr, checks)
			h.Provider.SetHasBuildReceipt(pr, tc.receipt)
			if tc.waiting {
				h.SeedChatStatus(agentID, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
				h.ChatTracker.SetWaiting(agentID, "awaiting checks_failed on recurser/bossanova#345")
			} else {
				h.ChatTracker.SetWaiting(agentID, "")
				h.SeedChatStatus(agentID, pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
			}
			h.PostGitHubWebhook(t, "issue_comment", payload, pr, repoURL)
			entry := h.DisplayTracker.Get(sessionID)
			if entry == nil || entry.HeadSHA != tc.head || entry.HasBuildReceipt != tc.receipt {
				t.Fatalf("polled entry = %+v, want head %s receipt %v", entry, tc.head, tc.receipt)
			}
			response, err := client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			var got *pb.Session
			for _, row := range response.Msg.Sessions {
				if row.Id == sessionID {
					got = row
					break
				}
			}
			if got == nil {
				t.Fatal("session missing from ListSessions")
			}
			if got.GetDisplayLabel() != tc.wantLabel || got.GetDisplayStatus() != tc.wantStatus || got.GetHasBuildReceipt() != tc.receipt {
				t.Fatalf("label/status/receipt = %q/%v/%v, want %q/%v/%v", got.GetDisplayLabel(), got.GetDisplayStatus(), got.GetHasBuildReceipt(), tc.wantLabel, tc.wantStatus, tc.receipt)
			}
			if got.GetIsReadyOverWaiting() != (tc.waiting && tc.receipt) {
				t.Fatalf("Ready over waiting mark = %v, want %v", got.GetIsReadyOverWaiting(), tc.waiting && tc.receipt)
			}
		})
	}
}
