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
// log, so a test can assert the session is resolved before the verb runs.
type prFake struct {
	sessions []*pb.Session
	listErr  error
	log      []string

	approve    *pb.VerifyApproveResult
	approveErr error
	verbs      []prVerbCall
}

func (f *prFake) ListSessions(_ context.Context, req *pb.ListSessionsRequest, _ client.SessionReadOptions) ([]*pb.Session, error) {
	f.log = append(f.log, "list")
	return f.sessions, f.listErr
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
		want := "Approved #7: waived open checklist items on head abcdef01 (record: https://x/9). Verify will re-check and merge.\n" +
			"  The PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section\n" +
			"Approving overrode open checklist items. A new push is judged afresh.\n" +
			"The needs-human label could not be removed because the tracker's credentials are missing.\n"
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
		want := "Would approve #7: record an approval waiving open checklist items on head abcdef01, then remove the needs-human label (dry run; nothing written).\n" +
			"  The PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section\n" +
			"Approving would override open checklist items.\n"
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
		want := "Approved #7: waived database migration on head abcdef01 (record: https://x/9). Verify will re-check and merge.\n" +
			"  A database migration for a person to review\n"
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

func TestPrVerbsRefuseInsideAnAgentChat(t *testing.T) {
	orig := osGetenv
	t.Cleanup(func() { osGetenv = orig })
	osGetenv = func(key string) string {
		if key == "BOSS_AGENT_SESSION_ID" {
			return "chat-123"
		}
		return ""
	}
	for _, verb := range []string{"approve"} {
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
	if c, _, err := root.Find([]string{"pr", "send-back"}); err == nil && c.Name() == "send-back" {
		t.Fatal("boss pr send-back is still registered; the operator removed send-back")
	}
	for _, path := range [][]string{{"pr", "approve"}} {
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
