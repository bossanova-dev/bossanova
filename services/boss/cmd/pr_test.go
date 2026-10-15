package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// prFake is the daemon client behind `boss pr`. Every call lands in one ordered
// log, so a test can assert the comment is posted before the chat is created
// and the chat before the message is delivered.
type prFake struct {
	sessions []*pb.Session
	listErr  error
	log      []string

	approve    *pb.VerifyApproveResult
	approveErr error
	sendBack   *pb.SendBackSessionPRResponse
	sendErr    error
	verbs      []prVerbCall
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

func (f *prFake) RecordChat(_ context.Context, sessionID, agentSessionID, title, _ string, _ pb.ChatRole, resume bool) (*pb.ClaudeChat, error) {
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

// prVerbCall is one daemon verify verb request.
type prVerbCall struct {
	sessionID string
	dryRun    bool
}

func (f *prFake) ApproveSessionPR(_ context.Context, id string, dryRun bool) (*pb.VerifyApproveResult, error) {
	f.log = append(f.log, "approve")
	f.verbs = append(f.verbs, prVerbCall{id, dryRun})
	if f.approveErr != nil {
		return nil, f.approveErr
	}
	return f.approve, nil
}

func (f *prFake) SendBackSessionPR(_ context.Context, id, note string, dryRun bool) (*pb.SendBackSessionPRResponse, error) {
	f.log = append(f.log, "send-back")
	f.verbs = append(f.verbs, prVerbCall{id, dryRun})
	f.notes = append(f.notes, note)
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return f.sendBack, nil
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

	t.Run("approved asks the daemon for the PR's session and prints the shared verdict", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: &pb.VerifyApproveResult{IsApproved: true, Waives: "ledger-open", HeadSha: "abcdef0123456789", TrackerWrites: "unavailable", Why: "TRACKER_CREDENTIALS_MISSING", ApprovalUrl: "https://x/9"}}
		cmd, out := prTestCmd(false)
		if err := prApprove(context.Background(), cmd, f, prOptions{pr: 7}); err != nil {
			t.Fatalf("prApprove: %v", err)
		}
		if len(f.verbs) != 1 || f.verbs[0] != (prVerbCall{"s-a", false}) {
			t.Fatalf("verbs = %+v, want one approve of s-a", f.verbs)
		}
		want := "Approved #7: waived ledger-open on head abcdef01 (record: https://x/9). Verify will re-check and merge.\n" +
			"  ledger-open: review left must-fix findings open\n" +
			"Approving overrode ledger-open (review left must-fix findings open); if that was not intended, boss pr send-back 7 \"<note>\" revokes the approval and sends the head back.\n" +
			"The needs-human label could not be removed (unavailable: TRACKER_CREDENTIALS_MISSING); remove it on the ticket by hand if this head still stands.\n"
		if got := out.String(); got != want {
			t.Fatalf("output = %q", got)
		}
	})

	t.Run("a refusal is a non-zero exit naming the reason", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: &pb.VerifyApproveResult{Reason: "no-standing-park", HeadSha: "abc"}}
		cmd, _ := prTestCmd(false)
		err := prApprove(context.Background(), cmd, f, prOptions{pr: 7})
		if err == nil || !strings.Contains(err.Error(), "PR #7 not approved: no `needs human:` park stands on the PR's current head [no-standing-park]") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a daemon timeout says the approval may already be recorded, except on a dry run", func(t *testing.T) {
		f := &prFake{sessions: sessions, approveErr: connect.NewError(connect.CodeDeadlineExceeded, errors.New("verify-gate timed out"))}
		cmd, _ := prTestCmd(false)
		err := prApprove(context.Background(), cmd, f, prOptions{pr: 7})
		if connect.CodeOf(err) != connect.CodeDeadlineExceeded || !strings.Contains(err.Error(), "an approval record may already be posted, so re-run approve to confirm") {
			t.Fatalf("err = %v", err)
		}
		cmd, _ = prTestCmd(false)
		err = prApprove(context.Background(), cmd, f, prOptions{pr: 7, dryRun: true})
		if connect.CodeOf(err) != connect.CodeDeadlineExceeded || strings.Contains(err.Error(), "may already be posted") {
			t.Fatalf("dry run: err = %v", err)
		}
	})

	t.Run("any other daemon failure is an error without the timeout advice", func(t *testing.T) {
		f := &prFake{sessions: sessions, approveErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("boss-verify toolbox not found"))}
		cmd, _ := prTestCmd(false)
		err := prApprove(context.Background(), cmd, f, prOptions{pr: 7})
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "toolbox not found") || strings.Contains(err.Error(), "may already be posted") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("the human-readable dry run passes --dry-run and names what would be written", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: &pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Waives: "ledger-open", HeadSha: "abcdef0123456789"}}
		cmd, out := prTestCmd(false)
		if err := prApprove(context.Background(), cmd, f, prOptions{pr: 7, dryRun: true}); err != nil {
			t.Fatalf("prApprove: %v", err)
		}
		if !f.verbs[0].dryRun {
			t.Fatal("--dry-run did not reach the daemon")
		}
		want := "Would approve #7: record an approval waiving ledger-open on head abcdef01, then remove the needs-human label (dry run; nothing written).\n" +
			"  ledger-open: review left must-fix findings open\n" +
			"Approving would override ledger-open (review left must-fix findings open); boss pr send-back 7 \"<note>\" sends the head back instead.\n"
		if got := out.String(); got != want {
			t.Fatalf("output = %q", got)
		}
	})

	t.Run("a sign-off approval lists the codes in plain words and warns of nothing", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: &pb.VerifyApproveResult{IsApproved: true, Waives: "human-review:database", HeadSha: "abcdef0123456789", TrackerWrites: "ok", ApprovalUrl: "https://x/9"}}
		cmd, out := prTestCmd(false)
		if err := prApprove(context.Background(), cmd, f, prOptions{pr: 7}); err != nil {
			t.Fatalf("prApprove: %v", err)
		}
		want := "Approved #7: waived human-review:database on head abcdef01 (record: https://x/9). Verify will re-check and merge.\n" +
			"  human-review:database: a database migration for a person to review\n"
		if got := out.String(); got != want {
			t.Fatalf("output = %q", got)
		}
	})

	t.Run("--json keeps verify-gate's field names", func(t *testing.T) {
		f := &prFake{sessions: sessions, approve: &pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Waives: "ledger-open", HeadSha: "abc"}}
		cmd, out := prTestCmd(true)
		if err := prApprove(context.Background(), cmd, f, prOptions{pr: 7, dryRun: true, asJSON: true}); err != nil {
			t.Fatalf("prApprove: %v", err)
		}
		want := `{"pr":7,"repo":"acme/app","session_id":"s-a","result":{"approved":false,"planned":true,"dryRun":true,"waives":"ledger-open","headSha":"abc"}}`
		var got, wantV any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		_ = json.Unmarshal([]byte(want), &wantV)
		if fmt.Sprint(got) != fmt.Sprint(wantV) {
			t.Fatalf("envelope = %s, want %s", out.String(), want)
		}
	})

	t.Run("an ambiguous number never runs the verb", func(t *testing.T) {
		f := &prFake{sessions: append(sessions, prSession("s-b", 7, originB, "/wt/b"))}
		cmd, _ := prTestCmd(false)
		if err := prApprove(context.Background(), cmd, f, prOptions{pr: 7}); errorCodeFor(err) != codeAmbiguousPR {
			t.Fatalf("err = %v", err)
		}
		if len(f.verbs) != 0 {
			t.Fatal("the verb ran for an ambiguous PR")
		}
	})
}

func TestPrSendBack(t *testing.T) {
	sessions := []*pb.Session{prSession("s-a", 7, originA, "/wt/a")}

	t.Run("a daemon timeout warns the note may already be posted, except on a dry run", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendErr: connect.NewError(connect.CodeDeadlineExceeded, errors.New("verify-gate timed out"))}
		cmd, _ := prTestCmd(false)
		err := prSendBack(context.Background(), cmd, f, prOptions{pr: 7}, "fix it")
		if connect.CodeOf(err) != connect.CodeDeadlineExceeded || !strings.Contains(err.Error(), "the note may already be posted") {
			t.Fatalf("err = %v", err)
		}
		cmd, _ = prTestCmd(false)
		err = prSendBack(context.Background(), cmd, f, prOptions{pr: 7, dryRun: true}, "fix it")
		if connect.CodeOf(err) != connect.CodeDeadlineExceeded || strings.Contains(err.Error(), "may already be posted") {
			t.Fatalf("dry run: err = %v", err)
		}
	})
	posted := &pb.SendBackSessionPRResponse{IsPosted: true, CommentUrl: "https://github.com/acme/app/pull/7#issuecomment-1", HeadSha: "abc"}

	t.Run("posts the comment, then starts a send back chat delivering the repair", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: posted, chatTmux: "tmux-1"}
		cmd, out := prTestCmd(false)
		note := "The empty state copy is wrong."
		if err := prSendBack(context.Background(), cmd, f, prOptions{pr: 7}, note); err != nil {
			t.Fatalf("prSendBack: %v", err)
		}
		if got := strings.Join(f.log, ","); got != "list,send-back,record-chat,send-message" {
			t.Fatalf("call order = %s", got)
		}
		if f.notes[0] != note || f.verbs[0] != (prVerbCall{"s-a", false}) {
			t.Fatalf("verb got %+v %q", f.verbs[0], f.notes[0])
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
		if !strings.Contains(got, posted.GetCommentUrl()) || !strings.Contains(got, f.sent.GetAgentSessionId()) {
			t.Fatalf("output %q must name the comment URL and the chat id", got)
		}
	})

	t.Run("a refused comment starts no chat", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: &pb.SendBackSessionPRResponse{Reason: "pr-not-open"}}
		cmd, _ := prTestCmd(false)
		err := prSendBack(context.Background(), cmd, f, prOptions{pr: 7}, "x")
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
		err := prSendBack(context.Background(), cmd, f, prOptions{pr: 7}, "x")
		if errorCodeFor(err) != codeAgentNotSpawned || !strings.Contains(err.Error(), posted.GetCommentUrl()) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(strings.Join(f.log, ","), "send-message") {
			t.Fatal("delivered into a chat with nothing listening")
		}
	})

	t.Run("a failed delivery is an error naming the chat to retry", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: posted, chatTmux: "tmux-1", deliverErr: errors.New("pane gone")}
		cmd, _ := prTestCmd(false)
		err := prSendBack(context.Background(), cmd, f, prOptions{pr: 7}, "x")
		if err == nil || !strings.Contains(err.Error(), "pane gone") || !strings.Contains(err.Error(), "boss chat send") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("--dry-run plans the comment and starts no chat", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: &pb.SendBackSessionPRResponse{IsPlanned: true, IsDryRun: true, HeadSha: "abc"}}
		cmd, out := prTestCmd(false)
		if err := prSendBack(context.Background(), cmd, f, prOptions{pr: 7, dryRun: true}, "x"); err != nil {
			t.Fatalf("prSendBack: %v", err)
		}
		if got := strings.Join(f.log, ","); got != "list,send-back" {
			t.Fatalf("call order = %s", got)
		}
		if !strings.Contains(out.String(), "/boss-repair 7") {
			t.Fatalf("dry-run output %q should show the repair it would deliver", out.String())
		}
	})

	t.Run("an empty note is refused before any daemon call", func(t *testing.T) {
		f := &prFake{sessions: sessions}
		cmd, _ := prTestCmd(false)
		if err := prSendBack(context.Background(), cmd, f, prOptions{pr: 7}, "  "); errorCodeFor(err) != codeInvalidArgument {
			t.Fatalf("err = %v", err)
		}
		if len(f.log) != 0 {
			t.Fatalf("calls = %v", f.log)
		}
	})

	t.Run("--json carries the comment, the chat and the delivery", func(t *testing.T) {
		f := &prFake{sessions: sessions, sendBack: posted, chatTmux: "tmux-1"}
		cmd, out := prTestCmd(true)
		if err := prSendBack(context.Background(), cmd, f, prOptions{pr: 7, asJSON: true}, "x"); err != nil {
			t.Fatalf("prSendBack: %v", err)
		}
		var env prSendBackJSON
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		if env.Result.CommentURL != posted.GetCommentUrl() || env.Chat == nil || env.Chat.Title != "send back" || env.Chat.AgentSessionID == "" || env.Delivery == nil || !env.Delivery.Delivered {
			t.Fatalf("envelope = %+v", env)
		}
	})
}

func TestPrVerbsRefuseInsideAnAgentChat(t *testing.T) {
	orig := osGetenv
	t.Cleanup(func() { osGetenv = orig })
	osGetenv = func(key string) string {
		if key == "BOSS_AGENT_SESSION_ID" {
			return "chat-123"
		}
		return ""
	}
	for _, verb := range []string{"approve", "send-back"} {
		cmd, _ := prTestCmd(false)
		cmd.Use = verb
		cmd.Flags().Bool("dry-run", false, "")
		cmd.Flags().String("repo", "", "")
		ran := false
		err := runPrVerb(cmd, "7", func(context.Context, client.BossClient, prOptions) error {
			ran = true
			return nil
		})
		if ran {
			t.Fatalf("%s ran inside an agent chat", verb)
		}
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "chat-123") || !strings.Contains(err.Error(), "your own terminal") {
			t.Fatalf("%s: err = %v", verb, err)
		}
	}
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
