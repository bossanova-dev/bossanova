package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/verifybridge"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// prFake is the daemon client and the verify-gate bridge behind `boss pr`. Every
// call lands in one ordered log, so a test can assert the comment is posted
// before the chat is created and the chat before the message is delivered.
type prFake struct {
	sessions []*pb.Session
	listErr  error
	log      []string

	approve    verifybridge.ApproveResult
	approveErr error
	sendBack   verifybridge.SendBackResult
	sendErr    error
	targets    []verifybridge.Target
	notes      []string

	chatTmux   string
	recordErr  error
	chatTitle  string
	chatSessID string
	sent       *pb.SendChatMessageRequest
	sendResp   *pb.SendChatMessageResponse
	deliverErr error
}

func (f *prFake) ListSessions(_ context.Context, req *pb.ListSessionsRequest, _ client.SessionReadOptions) ([]*pb.Session, error) {
	f.log = append(f.log, "list")
	return f.sessions, f.listErr
}

func (f *prFake) RecordChat(_ context.Context, sessionID, agentSessionID, title, _ string, resume bool) (*pb.ClaudeChat, error) {
	f.log = append(f.log, "record-chat")
	f.chatTitle, f.chatSessID = title, sessionID
	if resume {
		return nil, errors.New("send-back must start a new chat, not resume one")
	}
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	tmux := f.chatTmux
	return &pb.ClaudeChat{SessionId: sessionID, AgentSessionId: agentSessionID, Title: title, TmuxSessionName: tmux}, nil
}

func (f *prFake) SendChatMessage(_ context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	f.log = append(f.log, "send-message")
	f.sent = req
	if f.deliverErr != nil {
		return nil, f.deliverErr
	}
	if f.sendResp != nil {
		return f.sendResp, nil
	}
	return &pb.SendChatMessageResponse{Delivered: true, TmuxSessionName: "tmux-1"}, nil
}

func (f *prFake) Approve(_ context.Context, t verifybridge.Target) (verifybridge.ApproveResult, error) {
	f.log = append(f.log, "approve")
	f.targets = append(f.targets, t)
	return f.approve, f.approveErr
}

func (f *prFake) SendBack(_ context.Context, t verifybridge.Target, note string) (verifybridge.SendBackResult, error) {
	f.log = append(f.log, "send-back")
	f.targets = append(f.targets, t)
	f.notes = append(f.notes, note)
	return f.sendBack, f.sendErr
}

func prSession(id string, pr int32, origin, worktree string) *pb.Session {
	return &pb.Session{Id: id, PrNumber: &pr, RepoOriginUrl: origin, WorktreePath: worktree}
}

func prTestCmd(asJSON bool) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.Flags().Bool(jsonFlagName, asJSON, "")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	return cmd, &out
}

const (
	originA = "git@github.com:acme/app.git"
	originB = "https://github.com/other/app"
)

func TestResolvePRSession(t *testing.T) {
	sessions := []*pb.Session{
		prSession("s-other-pr", 8, originA, "/wt/8"),
		prSession("s-a", 7, originA, "/wt/a"),
		prSession("s-b", 7, originB, "/wt/b"),
		{Id: "s-no-pr", RepoOriginUrl: originA},
	}

	t.Run("an unambiguous number resolves to its session", func(t *testing.T) {
		s, slug, err := resolvePRSession(context.Background(), &prFake{sessions: sessions}, 8, "")
		if err != nil || s.GetId() != "s-other-pr" || slug != "acme/app" {
			t.Fatalf("got %v %q %v", s.GetId(), slug, err)
		}
	})

	t.Run("--repo picks between repos with the same number", func(t *testing.T) {
		s, slug, err := resolvePRSession(context.Background(), &prFake{sessions: sessions}, 7, "Other/App")
		if err != nil || s.GetId() != "s-b" || slug != "other/app" {
			t.Fatalf("got %v %q %v", s.GetId(), slug, err)
		}
	})

	t.Run("the same number in two repos without --repo is ambiguous", func(t *testing.T) {
		_, _, err := resolvePRSession(context.Background(), &prFake{sessions: sessions}, 7, "")
		if errorCodeFor(err) != codeAmbiguousPR || !strings.Contains(err.Error(), "--repo") {
			t.Fatalf("err = %v (code %s), want %s naming --repo", err, errorCodeFor(err), codeAmbiguousPR)
		}
	})

	t.Run("no session for the number is not found", func(t *testing.T) {
		_, _, err := resolvePRSession(context.Background(), &prFake{sessions: sessions}, 99, "")
		if errorCodeFor(err) != codeNotFound {
			t.Fatalf("err = %v, want %s", err, codeNotFound)
		}
		_, _, err = resolvePRSession(context.Background(), &prFake{sessions: sessions}, 8, "other/app")
		if errorCodeFor(err) != codeNotFound {
			t.Fatalf("a --repo that excludes the only match: err = %v, want %s", err, codeNotFound)
		}
	})

	t.Run("a list failure is an error, not an empty match", func(t *testing.T) {
		_, _, err := resolvePRSession(context.Background(), &prFake{listErr: errors.New("daemon down")}, 7, "")
		if err == nil || !strings.Contains(err.Error(), "daemon down") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestParsePRNumber(t *testing.T) {
	for in, want := range map[string]int{"7": 7, "#42": 42} {
		if got, err := parsePRNumber(in); err != nil || got != want {
			t.Fatalf("parsePRNumber(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "-1", "abc", "7x"} {
		if _, err := parsePRNumber(in); errorCodeFor(err) != codeInvalidArgument {
			t.Fatalf("parsePRNumber(%q) err = %v, want %s", in, err, codeInvalidArgument)
		}
	}
}

func TestPrApprove(t *testing.T) {
	sessions := []*pb.Session{prSession("s-a", 7, originA, "/wt/a")}

	t.Run("approved runs the verb in the session worktree and says verify will re-check", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: verifybridge.ApproveResult{Approved: true, Waives: "ledger-open,human-review:ui", HeadSHA: "abcdef0123456789", TrackerWrites: "ok", ApprovalURL: "https://github.com/acme/app/pull/7#issuecomment-9"}}
		cmd, out := prTestCmd(false)
		if err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7}); err != nil {
			t.Fatalf("prApprove: %v", err)
		}
		want := verifybridge.Target{PR: 7, Repo: "acme/app", Dir: "/wt/a"}
		if len(f.targets) != 1 || f.targets[0] != want {
			t.Fatalf("targets = %+v, want %+v", f.targets, want)
		}
		if got := out.String(); got != "Approved #7: waived ledger-open,human-review:ui on head abcdef01 (record: https://github.com/acme/app/pull/7#issuecomment-9). Verify will re-check and merge.\n" {
			t.Fatalf("output = %q", got)
		}
	})

	t.Run("a refusal is a non-zero exit naming the reason", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: verifybridge.ApproveResult{Reason: "no-standing-park", HeadSHA: "abc"}}
		cmd, _ := prTestCmd(false)
		err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7})
		if err == nil || !strings.Contains(err.Error(), "no-standing-park") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a refusal after the record was posted names the record and what it still does", func(t *testing.T) {
		const url = "https://github.com/acme/app/pull/7#issuecomment-9"
		for _, tc := range []struct{ reason, want string }{
			{"head-moved", "the record at " + url + " names head abcdef01 only and waives nothing that stands now"},
			{"approval-superseded", "a later operator decision on the head (a send-back or another approval)"},
			{"approval-unconfirmed", "the record at " + url + " may still stand for head abcdef01, so re-run approve to confirm"},
		} {
			f := &prFake{sessions: sessions, approve: verifybridge.ApproveResult{Reason: tc.reason, HeadSHA: "abcdef0123456789", ApprovalURL: url}}
			cmd, _ := prTestCmd(false)
			err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7})
			if err == nil || !strings.Contains(err.Error(), "["+tc.reason+"]") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: err = %v", tc.reason, err)
			}
		}
		unconfirmed := &prFake{sessions: sessions, approve: verifybridge.ApproveResult{Reason: "approval-unconfirmed", HeadSHA: "abc"}}
		cmd, _ := prTestCmd(false)
		err := prApprove(context.Background(), cmd, unconfirmed, unconfirmed, prOptions{pr: 7})
		if err == nil || !strings.Contains(err.Error(), "whether the approval record was posted and took effect could not be read back [approval-unconfirmed]; re-run approve to confirm") {
			t.Fatalf("approval-unconfirmed without a URL: err = %v", err)
		}
		f := &prFake{sessions: sessions, approve: verifybridge.ApproveResult{Reason: "approval-record-failed", HeadSHA: "abc"}}
		cmd, _ = prTestCmd(false)
		err = prApprove(context.Background(), cmd, f, f, prOptions{pr: 7})
		if err == nil || !strings.Contains(err.Error(), "could not be posted on the PR, so nothing is approved") || strings.Contains(err.Error(), "the record at") {
			t.Fatalf("approval-record-failed: err = %v", err)
		}
	})

	t.Run("an approval whose label removal failed still reports the approval", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: verifybridge.ApproveResult{Approved: true, Waives: "ledger-open", HeadSHA: "abcdef0123456789", ApprovalURL: "https://x/9", TrackerWrites: "failed", Why: "rate-limited"}}
		cmd, out := prTestCmd(false)
		if err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7}); err != nil {
			t.Fatalf("prApprove: %v", err)
		}
		want := "Approved #7: waived ledger-open on head abcdef01 (record: https://x/9). Verify will re-check and merge.\n" +
			"The needs-human label could not be removed (failed: rate-limited); remove it on the ticket by hand if this head still stands.\n"
		if got := out.String(); got != want {
			t.Fatalf("output = %q", got)
		}
	})

	t.Run("a lifted send-back says so and names how to merge", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: verifybridge.ApproveResult{Approved: true, Lifted: "sent-back", HeadSHA: "abcdef0123456789", ApprovalURL: "https://x/9"}}
		cmd, out := prTestCmd(false)
		if err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7}); err != nil {
			t.Fatalf("prApprove: %v", err)
		}
		if got := out.String(); got != "Approved #7: lifted the send-back of head abcdef01 (record: https://x/9). Run /boss-verify 7 to merge it.\n" {
			t.Fatalf("output = %q", got)
		}
	})

	t.Run("an approval whose label removal did not happen names why the label stays", func(t *testing.T) {
		for _, tc := range []struct{ writes, why, want string }{
			{"unavailable", "TRACKER_CREDENTIALS_MISSING", "The needs-human label could not be removed (unavailable: TRACKER_CREDENTIALS_MISSING); remove it on the ticket by hand if this head still stands.\n"},
			{"skipped", "head-moved", "The PR's head or its park changed after the approval, so the needs-human label was left on.\n"},
			{"skipped", "status-unreadable", "The needs-human label was left on (status-unreadable); remove it on the ticket by hand if this head still stands.\n"},
			{"skipped", "pr-not-open", "The PR is no longer open, so the needs-human label was left on; remove it on the ticket by hand if the work is done.\n"},
			{"unavailable", "status-unreadable", "The needs-human label could not be removed (unavailable: status-unreadable); remove it on the ticket by hand if this head still stands.\n"},
		} {
			f := &prFake{sessions: sessions, approve: verifybridge.ApproveResult{Approved: true, Waives: "ledger-open", HeadSHA: "abcdef0123456789", ApprovalURL: "https://x/9", TrackerWrites: tc.writes, Why: tc.why}}
			cmd, out := prTestCmd(false)
			if err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7}); err != nil {
				t.Fatalf("%s: prApprove: %v", tc.writes, err)
			}
			if got := out.String(); !strings.HasSuffix(got, tc.want) || !strings.HasPrefix(got, "Approved #7: waived ledger-open on head abcdef01") {
				t.Fatalf("%s: output = %q", tc.writes, got)
			}
		}
	})

	t.Run("the human-readable dry run names what would be written", func(t *testing.T) {
		for _, tc := range []struct {
			result verifybridge.ApproveResult
			want   string
		}{
			{verifybridge.ApproveResult{Planned: true, DryRun: true, Waives: "ledger-open", HeadSHA: "abcdef0123456789"},
				"Would approve #7: record an approval waiving ledger-open on head abcdef01, then remove the needs-human label (dry run; nothing written).\n"},
			{verifybridge.ApproveResult{Planned: true, DryRun: true, Lifted: "sent-back", HeadSHA: "abcdef0123456789"},
				"Would approve #7: record an approval lifting the send-back of head abcdef01 (dry run; nothing written).\n"},
		} {
			f := &prFake{sessions: sessions, approve: tc.result}
			cmd, out := prTestCmd(false)
			if err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7, dryRun: true}); err != nil {
				t.Fatalf("prApprove: %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		}
	})

	t.Run("a bridge timeout says the approval may already be recorded, except on a dry run", func(t *testing.T) {
		f := &prFake{sessions: sessions, approveErr: fmt.Errorf("%w: slow", verifybridge.ErrNodeTimeout)}
		cmd, _ := prTestCmd(false)
		err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7})
		if !errors.Is(err, verifybridge.ErrNodeTimeout) || !strings.Contains(err.Error(), "re-run approve to confirm") {
			t.Fatalf("err = %v", err)
		}
		cmd, _ = prTestCmd(false)
		err = prApprove(context.Background(), cmd, f, f, prOptions{pr: 7, dryRun: true})
		if !errors.Is(err, verifybridge.ErrNodeTimeout) || strings.Contains(err.Error(), "may already be posted") {
			t.Fatalf("dry run: err = %v", err)
		}
	})

	t.Run("every reason approve refuses with renders its own detail", func(t *testing.T) {
		// The refusal reasons skills-toolbox/verify-gate.mjs approve returns.
		for _, reason := range []string{
			"pr-unreadable", "pr-not-open", "head-unreadable", "status-unreadable",
			"park-codes-unknown", "comments-unreadable", "no-standing-park",
			"sent-back-approved-head", "approver-not-writer", "approver-unreadable",
			"head-moved", "approval-superseded", "approval-record-failed", "approval-unconfirmed",
		} {
			if got := refusalDetail(reason, "", ""); strings.HasPrefix(got, "refused [") {
				t.Errorf("reason %q falls back to the generic detail: %q", reason, got)
			}
		}
	})

	t.Run("a bridge failure is an error", func(t *testing.T) {
		f := &prFake{sessions: sessions, approveErr: verifybridge.ErrEmptyOutput}
		cmd, _ := prTestCmd(false)
		if err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7}); !errors.Is(err, verifybridge.ErrEmptyOutput) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("--dry-run --json reports the plan", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: verifybridge.ApproveResult{Planned: true, DryRun: true, Waives: "ledger-open", HeadSHA: "abc"}}
		cmd, out := prTestCmd(true)
		if err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7, dryRun: true, asJSON: true}); err != nil {
			t.Fatalf("prApprove: %v", err)
		}
		if !f.targets[0].DryRun {
			t.Fatal("--dry-run did not reach the verb")
		}
		var env prApproveJSON
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		if env.PR != 7 || env.Repo != "acme/app" || env.SessionID != "s-a" || !env.Result.Planned || env.Result.Waives != "ledger-open" {
			t.Fatalf("envelope = %+v", env)
		}
	})

	t.Run("an ambiguous number never runs the verb", func(t *testing.T) {
		f := &prFake{sessions: append(sessions, prSession("s-b", 7, originB, "/wt/b"))}
		cmd, _ := prTestCmd(false)
		if err := prApprove(context.Background(), cmd, f, f, prOptions{pr: 7}); errorCodeFor(err) != codeAmbiguousPR {
			t.Fatalf("err = %v", err)
		}
		if len(f.targets) != 0 {
			t.Fatal("the verb ran for an ambiguous PR")
		}
	})
}

func TestPrSendBack(t *testing.T) {
	sessions := []*pb.Session{prSession("s-a", 7, originA, "/wt/a")}

	t.Run("a bridge timeout warns the note may already be posted, except on a dry run", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendErr: fmt.Errorf("%w: slow", verifybridge.ErrNodeTimeout)}
		cmd, _ := prTestCmd(false)
		err := prSendBack(context.Background(), cmd, f, f, prOptions{pr: 7}, "fix it")
		if !errors.Is(err, verifybridge.ErrNodeTimeout) || !strings.Contains(err.Error(), "the note may already be posted") {
			t.Fatalf("err = %v", err)
		}
		cmd, _ = prTestCmd(false)
		err = prSendBack(context.Background(), cmd, f, f, prOptions{pr: 7, dryRun: true}, "fix it")
		if !errors.Is(err, verifybridge.ErrNodeTimeout) || strings.Contains(err.Error(), "may already be posted") {
			t.Fatalf("dry run: err = %v", err)
		}
	})
	posted := verifybridge.SendBackResult{Posted: true, CommentURL: "https://github.com/acme/app/pull/7#issuecomment-1", HeadSHA: "abc"}

	t.Run("posts the comment, then starts a send back chat delivering the repair", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: posted, chatTmux: "tmux-1"}
		cmd, out := prTestCmd(false)
		note := "The empty state copy is wrong."
		if err := prSendBack(context.Background(), cmd, f, f, prOptions{pr: 7}, note); err != nil {
			t.Fatalf("prSendBack: %v", err)
		}
		if got := strings.Join(f.log, ","); got != "list,send-back,record-chat,send-message" {
			t.Fatalf("call order = %s", got)
		}
		if f.notes[0] != note || f.targets[0] != (verifybridge.Target{PR: 7, Repo: "acme/app", Dir: "/wt/a"}) {
			t.Fatalf("verb got %+v %q", f.targets[0], f.notes[0])
		}
		if f.chatTitle != "send back" || f.chatSessID != "s-a" {
			t.Fatalf("chat = %q in %q", f.chatTitle, f.chatSessID)
		}
		msg := f.sent.GetMessage()
		if !strings.HasPrefix(msg, "/boss-repair 7\n") || !strings.Contains(msg, note) {
			t.Fatalf("message = %q", msg)
		}
		if f.sent.GetAgentSessionId() == "" || !f.sent.GetSubmit() || !f.sent.GetWakeIfAsleep() {
			t.Fatalf("send request = %+v", f.sent)
		}
		got := out.String()
		if !strings.Contains(got, posted.CommentURL) || !strings.Contains(got, f.sent.GetAgentSessionId()) {
			t.Fatalf("output %q must name the comment URL and the chat id", got)
		}
	})

	t.Run("a refused comment starts no chat", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: verifybridge.SendBackResult{Reason: "pr-not-open"}}
		cmd, _ := prTestCmd(false)
		err := prSendBack(context.Background(), cmd, f, f, prOptions{pr: 7}, "x")
		if err == nil || !strings.Contains(err.Error(), "pr-not-open") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(strings.Join(f.log, ","), "record-chat") {
			t.Fatal("a chat was started for a send-back that posted nothing")
		}
	})

	t.Run("a chat with no live agent is an error naming the posted comment", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: posted, chatTmux: ""}
		cmd, _ := prTestCmd(false)
		err := prSendBack(context.Background(), cmd, f, f, prOptions{pr: 7}, "x")
		if errorCodeFor(err) != codeAgentNotSpawned || !strings.Contains(err.Error(), posted.CommentURL) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(strings.Join(f.log, ","), "send-message") {
			t.Fatal("delivered into a chat with nothing listening")
		}
	})

	t.Run("a failed delivery is an error naming the chat to retry", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: posted, chatTmux: "tmux-1", deliverErr: errors.New("pane gone")}
		cmd, _ := prTestCmd(false)
		err := prSendBack(context.Background(), cmd, f, f, prOptions{pr: 7}, "x")
		if err == nil || !strings.Contains(err.Error(), "pane gone") || !strings.Contains(err.Error(), "boss chat send") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("--dry-run plans the comment and starts no chat", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: verifybridge.SendBackResult{Planned: true, DryRun: true, HeadSHA: "abc"}}
		cmd, out := prTestCmd(false)
		if err := prSendBack(context.Background(), cmd, f, f, prOptions{pr: 7, dryRun: true}, "x"); err != nil {
			t.Fatalf("prSendBack: %v", err)
		}
		if got := strings.Join(f.log, ","); got != "list,send-back" {
			t.Fatalf("call order = %s", got)
		}
		if !strings.Contains(out.String(), "/boss-repair 7") {
			t.Fatalf("dry-run output %q should show the repair it would deliver", out.String())
		}
	})

	t.Run("--json carries the comment, the chat and the delivery", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: posted, chatTmux: "tmux-1"}
		cmd, out := prTestCmd(true)
		if err := prSendBack(context.Background(), cmd, f, f, prOptions{pr: 7, asJSON: true}, "x"); err != nil {
			t.Fatalf("prSendBack: %v", err)
		}
		var env prSendBackJSON
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		if env.Result.CommentURL != posted.CommentURL || env.Chat == nil || env.Chat.Title != "send back" || env.Chat.AgentSessionID == "" || env.Delivery == nil || !env.Delivery.Delivered {
			t.Fatalf("envelope = %+v", env)
		}
	})
}

func TestPrCmdIsRegistered(t *testing.T) {
	root := rootCmd()
	for _, path := range [][]string{{"pr", "approve"}, {"pr", "send-back"}} {
		c, _, err := root.Find(path)
		if err != nil || c.Name() != path[1] {
			t.Fatalf("boss %s not registered: %v", strings.Join(path, " "), err)
		}
		for _, flag := range []string{"repo", jsonFlagName, "dry-run"} {
			if c.Flags().Lookup(flag) == nil {
				t.Fatalf("boss %s lacks --%s", strings.Join(path, " "), flag)
			}
		}
	}
}
