package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/table"
	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/githubcallback"

	"github.com/recurser/boss/internal/views"
)

// fixedTime is an arbitrary stable timestamp for deterministic JSON assertions.
func fixedTime() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// newChatFlagCmd builds a bare command carrying just the --chat flag so
// resolveCallbackChat can be exercised without wiring the full cobra tree.
func newChatFlagCmd(chat string, changed bool) *cobra.Command {
	cmd := &cobra.Command{Use: "add"}
	cmd.Flags().String("chat", "", "")
	if changed {
		_ = cmd.Flags().Set("chat", chat)
	}
	return cmd
}

func TestResolveCallbackChat(t *testing.T) {
	orig := osGetenv
	t.Cleanup(func() { osGetenv = orig })

	tests := []struct {
		name       string
		flag       string
		flagSet    bool
		env        string
		want       string
		wantErrSub string
	}{
		{name: "explicit flag wins", flag: "chat-flag", flagSet: true, env: "chat-env", want: "chat-flag"},
		{name: "flag trims whitespace", flag: "  chat-flag  ", flagSet: true, want: "chat-flag"},
		{name: "falls back to env", env: "chat-env", want: "chat-env"},
		{name: "env trims whitespace", env: "  chat-env  ", want: "chat-env"},
		{name: "flag beats env even when env set", flag: "flag", flagSet: true, env: "env", want: "flag"},
		{name: "no flag, no env errors", wantErrSub: "--chat"},
		{name: "blank flag falls through to env", flag: "   ", flagSet: true, env: "chat-env", want: "chat-env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			osGetenv = func(key string) string {
				if key == "BOSS_AGENT_SESSION_ID" {
					return tt.env
				}
				return ""
			}
			cmd := newChatFlagCmd(tt.flag, tt.flagSet)
			got, err := resolveCallbackChat(cmd)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error, got chat=%q", got)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("error %q missing %q", err.Error(), tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGithubCallbackToJSON_NeverLeaksMessage is the security-critical assertion:
// the message body is a secret, so no marshaling of a GithubCallback may ever
// surface it, regardless of what fields the proto carries.
func TestGithubCallbackToJSON_NeverLeaksMessage(t *testing.T) {
	const secret = "TOP-SECRET-DELIVERY-PROMPT"
	cb := &pb.GithubCallback{
		Id:           "cb-1",
		GroupId:      "grp-9",
		TargetChatId: "chat-1",
		RepoOwner:    "acme",
		RepoName:     "widgets",
		PrNumber:     42,
		Trigger:      "merged",
		State:        "active",
		AttemptCount: 3,
		LastEvent:    "some-event",
		LastError:    "some-error",
		Message:      secret,
		ExpiresAt:    timestamppb.New(fixedTime()),
		CreatedAt:    timestamppb.New(fixedTime()),
	}

	out := githubCallbackToJSON(cb)

	// The struct has no Message field at all; marshal it and prove the secret is
	// absent from the serialized bytes.
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("SECRET LEAK: message body present in JSON: %s", b)
	}
	// Sanity: the non-secret fields did round-trip.
	if out.ID != "cb-1" || out.GroupID != "grp-9" || out.PRNumber != 42 {
		t.Fatalf("unexpected mapping: %+v", out)
	}
	if out.RepoOwner != "acme" || out.RepoName != "widgets" || out.Trigger != "merged" {
		t.Fatalf("unexpected repo/trigger mapping: %+v", out)
	}
	if out.AttemptCount != 3 || out.LastEvent != "some-event" || out.LastError != "some-error" {
		t.Fatalf("unexpected status mapping: %+v", out)
	}
	if out.ExpiresAt == "" || out.CreatedAt == "" {
		t.Fatalf("expected RFC3339 timestamps, got %+v", out)
	}
	// Timestamps left nil must render empty, not a zero-time string.
	if out.TriggeredAt != "" || out.DeliveredAt != "" || out.UpdatedAt != "" {
		t.Fatalf("nil timestamps must render empty, got %+v", out)
	}
	if out.ShouldRequireTransition || out.HasObservedBaseline {
		t.Fatalf("zero bools should map false, got %+v", out)
	}
}

func TestGithubCallbackToJSON_TransitionFields(t *testing.T) {
	out := githubCallbackToJSON(&pb.GithubCallback{
		Id:                      "cb-1",
		ShouldRequireTransition: true,
		HasObservedBaseline:     true,
	})
	if !out.ShouldRequireTransition || !out.HasObservedBaseline {
		t.Fatalf("transition fields not mapped: %+v", out)
	}
}

// TestCallbackListColumns_TriggerNeverTruncated is a rendering regression guard:
// the table clips each cell to its column width, so a TRIGGER cap shorter than
// the longest trigger silently displays `checks_passed_re` instead of
// `checks_passed_ready`. Render the real table with every valid trigger and
// assert each one survives verbatim.
func TestCallbackListColumns_TriggerNeverTruncated(t *testing.T) {
	triggers := githubcallback.ValidTriggerStrings()
	n := len(triggers)
	ids := make([]string, n)
	prs := make([]string, n)
	states := make([]string, n)
	chats := make([]string, n)
	expiries := make([]string, n)
	rows := make([]table.Row, n)
	for i, tr := range triggers {
		// Deliberately opaque ids: an id echoing the trigger would let an
		// unbounded neighbouring column satisfy the Contains assertion below
		// even while the TRIGGER cell itself is clipped.
		ids[i] = fmt.Sprintf("cb-%02d", i)
		prs[i] = "acme/widgets#42"
		states[i] = "active"
		chats[i] = "chat-1"
		expiries[i] = "2026-01-02T03:04:05Z"
		rows[i] = table.Row{ids[i], prs[i], tr, states[i], chats[i], expiries[i]}
	}

	cols := callbackListColumns(ids, prs, triggers, states, chats, expiries)
	tbl := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	view := tbl.View()

	for _, tr := range triggers {
		if !strings.Contains(view, tr) {
			t.Errorf("trigger %q truncated in rendered table (TRIGGER width %d):\n%s",
				tr, cols[2].Width, view)
		}
	}
}

type fakeCallbackClient struct {
	client.BossClient
	callbacks   []*pb.GithubCallback
	deletedChat string
	deletedID   string
	deleteResp  *pb.DeleteGithubCallbackResponse
	deleteErr   error
	notice      string
	createReq   *pb.CreateGithubCallbackRequest

	// Bulk-remove knobs. They only apply when deleteResp/deleteErr are unset,
	// so the per-id tests above keep their fixed responses.
	listReqs   []*pb.ListGithubCallbacksRequest
	deletedIDs []string
	// stickyIDs answer "deleted" but stay active: the post-condition must
	// catch a delete that did not take.
	stickyIDs map[string]bool
	// vanishedIDs disappear before the delete lands and answer not_found:
	// the row fired or expired between the list and the delete.
	vanishedIDs map[string]bool
	// vanishedAsError reports a vanished row as a connect NotFound error
	// instead of a not_found outcome.
	vanishedAsError bool
}

// ListGithubCallbacks honours the chat and state filters the way the daemon
// does, so a scoping test fails if the CLI stops sending them.
func (f *fakeCallbackClient) ListGithubCallbacks(_ context.Context, req *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error) {
	f.listReqs = append(f.listReqs, req)
	var out []*pb.GithubCallback
	for _, cb := range f.callbacks {
		if req.TargetChatId != nil && cb.GetTargetChatId() != req.GetTargetChatId() {
			continue
		}
		if req.State != nil && cb.GetState() != req.GetState() {
			continue
		}
		out = append(out, cb)
	}
	return out, nil
}

func (f *fakeCallbackClient) CreateGithubCallback(_ context.Context, req *pb.CreateGithubCallbackRequest) (*pb.CreateGithubCallbackResponse, error) {
	f.createReq = req
	notice := f.notice
	// Mirror the daemon: independent_watch suppresses the advisory at source.
	if req.GetIsIndependentWatch() {
		notice = ""
	}
	return &pb.CreateGithubCallbackResponse{
		GithubCallback: &pb.GithubCallback{
			Id:           "cb-new",
			TargetChatId: req.GetTargetChatId(),
			RepoOwner:    req.GetRepoOwner(),
			RepoName:     req.GetRepoName(),
			PrNumber:     req.GetPrNumber(),
			Trigger:      req.GetTrigger(),
			GroupId:      req.GetGroupId(),
			State:        "active",
		},
		NoticeText: notice,
	}, nil
}

func (f *fakeCallbackClient) DeleteGithubCallback(_ context.Context, targetChatID, id string) (*pb.DeleteGithubCallbackResponse, error) {
	f.deletedChat = targetChatID
	f.deletedID = id
	f.deletedIDs = append(f.deletedIDs, id)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if f.deleteResp != nil {
		return f.deleteResp, nil
	}
	if f.stickyIDs[id] {
		return &pb.DeleteGithubCallbackResponse{Outcome: "deleted"}, nil
	}
	f.dropCallback(id)
	if f.vanishedIDs[id] {
		if f.vanishedAsError {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("missing"))
		}
		return &pb.DeleteGithubCallbackResponse{Outcome: "not_found"}, nil
	}
	return &pb.DeleteGithubCallbackResponse{Outcome: "deleted"}, nil
}

func (f *fakeCallbackClient) dropCallback(id string) {
	kept := make([]*pb.GithubCallback, 0, len(f.callbacks))
	for _, cb := range f.callbacks {
		if cb.GetId() != id {
			kept = append(kept, cb)
		}
	}
	f.callbacks = kept
}

func newCallbackTestCmd() (*cobra.Command, *bytes.Buffer) {
	cmd, out, _ := newCallbackTestCmdSplit()
	return cmd, out
}

// newCallbackTestCmdSplit gives stdout and stderr SEPARATE buffers. The shared
// buffer newCallbackTestCmd hands back cannot tell the two apart, so a test
// asserting "the notice is on stderr, not stdout" passes vacuously against it.
// Anything checking the stdout/stderr split must use this.
func newCallbackTestCmdSplit() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{Use: "callback-test"}
	cmd.Flags().String("chat", "", "")
	cmd.Flags().String("id", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("message", "", "")
	cmd.Flags().String("expires-in", "", "")
	cmd.Flags().String("group", "", "")
	cmd.Flags().Bool("on-transition", false, "")
	cmd.Flags().Bool("independent-watch", false, "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().Int32("pr", 0, "")
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	return cmd, &out, &errOut
}

func TestRunCallbackListWithClient_TableFooterAndIDFilter(t *testing.T) {
	cmd, out := newCallbackTestCmd()
	if err := cmd.Flags().Set("id", "cb-2"); err != nil {
		t.Fatalf("set id: %v", err)
	}
	fake := &fakeCallbackClient{callbacks: []*pb.GithubCallback{
		{Id: "cb-1", RepoOwner: "acme", RepoName: "widgets", PrNumber: 7, Trigger: "merged", State: "active", TargetChatId: "chat-1"},
		{Id: "cb-2", RepoOwner: "acme", RepoName: "widgets", PrNumber: 7, Trigger: "closed", State: "active", TargetChatId: "chat-1"},
	}}
	if err := runCallbackListWithClient(cmd, fake); err != nil {
		t.Fatalf("list: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "cb-1") {
		t.Fatalf("id filter leaked cb-1:\n%s", got)
	}
	if !strings.Contains(got, "cb-2") || !strings.Contains(got, "1 callbacks (complete listing)") {
		t.Fatalf("list missing row/footer:\n%s", got)
	}
}

func TestRunCallbackListWithClient_IDNotFoundIsExplicit(t *testing.T) {
	cmd, out := newCallbackTestCmd()
	if err := cmd.Flags().Set("id", "missing"); err != nil {
		t.Fatalf("set id: %v", err)
	}
	err := runCallbackListWithClient(cmd, &fakeCallbackClient{callbacks: []*pb.GithubCallback{{Id: "cb-1"}}})
	if err == nil {
		t.Fatal("expected missing id error")
	}
	if !strings.Contains(out.String(), "Callback missing not found.") {
		t.Fatalf("missing id output not explicit: %q", out.String())
	}
}

func TestRunCallbackRemoveWithClientRejectsMultiIDBlob(t *testing.T) {
	cmd, _ := newCallbackTestCmd()
	fake := &fakeCallbackClient{}
	for _, id := range []string{"id1 id2 id3", "id1,id2,id3"} {
		t.Run(id, func(t *testing.T) {
			err := runCallbackRemoveWithClient(cmd, fake, id)
			if err == nil {
				t.Fatal("expected multi-id rejection")
			}
			if !strings.Contains(err.Error(), "id1") || !strings.Contains(err.Error(), "id2") || !strings.Contains(err.Error(), "id3") {
				t.Fatalf("error should name parsed ids, got %v", err)
			}
			if fake.deletedID != "" {
				t.Fatalf("delete RPC should not run, deleted %q", fake.deletedID)
			}
		})
	}
}

func TestRunCallbackRemoveWithClientOutcomes(t *testing.T) {
	orig := osGetenv
	t.Cleanup(func() { osGetenv = orig })
	osGetenv = func(key string) string {
		if key == "BOSS_AGENT_SESSION_ID" {
			return "chat-1"
		}
		return ""
	}

	cases := []struct {
		name      string
		resp      *pb.DeleteGithubCallbackResponse
		err       error
		wantOut   string
		wantError bool
	}{
		{name: "deleted", resp: &pb.DeleteGithubCallbackResponse{Outcome: "deleted"}, wantOut: "Removed callback cb-1.", wantError: false},
		{name: "not_found response", resp: &pb.DeleteGithubCallbackResponse{Outcome: "not_found"}, wantOut: "Callback cb-1 not found.", wantError: true},
		{name: "not_owned response", resp: &pb.DeleteGithubCallbackResponse{Outcome: "not_owned"}, wantOut: "Callback cb-1 not owned by chat-1.", wantError: true},
		{name: "not_found error", err: connect.NewError(connect.CodeNotFound, errors.New("missing")), wantOut: "Callback cb-1 not found.", wantError: true},
		{name: "not_owned error", err: connect.NewError(connect.CodePermissionDenied, errors.New("wrong chat")), wantOut: "Callback cb-1 not owned by chat-1.", wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, out := newCallbackTestCmd()
			fake := &fakeCallbackClient{deleteResp: tc.resp, deleteErr: tc.err}
			err := runCallbackRemoveWithClient(cmd, fake, "cb-1")
			if tc.wantError && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fake.deletedChat != "chat-1" || fake.deletedID != "cb-1" {
				t.Fatalf("delete args = %q/%q, want chat-1/cb-1", fake.deletedChat, fake.deletedID)
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("output %q missing %q", out.String(), tc.wantOut)
			}
		})
	}
}

func TestTriggerLabel(t *testing.T) {
	// Bind expectations to the canonical trigger order so a reordering of
	// ValidTriggers() is caught here rather than silently mislabeling output.
	triggers := githubcallback.ValidTriggers()
	// Fail cleanly rather than panicking on an index if the vocabulary ever
	// shrinks; a removed trigger is a deliberate breaking change and should
	// read as such here.
	if len(triggers) < 6 {
		t.Fatalf("ValidTriggers() returned %d triggers, want at least 6: %v", len(triggers), triggers)
	}
	cases := map[string]string{
		string(triggers[0]): "is merged",
		string(triggers[1]): "is closed",
		string(triggers[2]): "passes checks",
		string(triggers[3]): "fails checks",
		string(triggers[4]): "is ready for review",
		string(triggers[5]): "passes checks while ready for review",
		"unknown-trigger":   "unknown-trigger", // unmapped falls through verbatim
	}
	for in, want := range cases {
		if got := triggerLabel(in); got != want {
			t.Errorf("triggerLabel(%q) = %q, want %q", in, got, want)
		}
	}
	// Every valid trigger must have a real label: an unlabelled trigger would
	// silently fall through to its raw wire string in human output, which is
	// exactly the regression this guards against as the vocabulary grows.
	for _, tr := range triggers {
		if got := triggerLabel(string(tr)); got == string(tr) {
			t.Errorf("trigger %q has no label (fell through to raw string)", tr)
		}
	}
}

// ---------------------------------------------------------------------------
// Split-group advisory on `boss callback add` (BOS-1268)
// ---------------------------------------------------------------------------

// conflictNotice is the shape the daemon composes. The CLI is a pass-through,
// so the test asserts the CLI put it on the right stream — not that the CLI
// reworded it.
const conflictNotice = "warning: checks_failed in group \"ski109-final-fail\" cannot be satisfied at the same time as " +
	"callback cb-9 (group \"ski109-final-pass\", trigger checks_passed, expires 2026-09-18T06:50:18Z), " +
	"which is still armed for this chat and PR."

func addCmdWithFlags(t *testing.T, fake *fakeCallbackClient, flags map[string]string) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd, out, errOut := newCallbackTestCmdSplit()
	if err := cmd.Flags().Set("chat", "chat-1"); err != nil {
		t.Fatalf("set chat: %v", err)
	}
	if err := cmd.Flags().Set("repo", "acme/widgets"); err != nil {
		t.Fatalf("set repo: %v", err)
	}
	if err := cmd.Flags().Set("message", "PR #123 went red"); err != nil {
		t.Fatalf("set message: %v", err)
	}
	for k, v := range flags {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	if err := runCallbackAddWithClient(cmd, fake, "123", "checks_failed"); err != nil {
		t.Fatalf("add: %v", err)
	}
	return out, errOut
}

// TestRunCallbackAddWithClient_NoticeGoesToStderr is R4's gate: the advisory
// must not enter stdout, which carries the human result and (with --json) a
// documented machine contract.
func TestRunCallbackAddWithClient_NoticeGoesToStderr(t *testing.T) {
	fake := &fakeCallbackClient{notice: conflictNotice}
	out, errOut := addCmdWithFlags(t, fake, nil)

	if !strings.Contains(errOut.String(), conflictNotice) {
		t.Fatalf("notice missing from stderr:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), callbackNoticePrefix) {
		t.Fatalf("notice leaked into stdout:\n%s", out.String())
	}
	// stdout still carries the ordinary confirmation.
	if !strings.Contains(out.String(), "cb-new") {
		t.Fatalf("stdout lost the registration line:\n%s", out.String())
	}
}

// TestRunCallbackAddWithClient_JSONStdoutStaysClean pins that --json stdout is
// exactly the documented envelope: parseable, and with no extra top-level key
// smuggling the advisory in.
func TestRunCallbackAddWithClient_JSONStdoutStaysClean(t *testing.T) {
	fake := &fakeCallbackClient{notice: conflictNotice}
	out, errOut := addCmdWithFlags(t, fake, map[string]string{"json": "true"})

	var envelope map[string]any
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("stdout is not the documented JSON envelope: %v\n%s", err, out.String())
	}
	var want map[string]any
	if err := json.Unmarshal(mustJSON(t, githubCallbackToJSON(&pb.GithubCallback{
		Id: "cb-new", TargetChatId: "chat-1", RepoOwner: "acme", RepoName: "widgets",
		PrNumber: 123, Trigger: "checks_failed", State: "active",
	})), &want); err != nil {
		t.Fatalf("reference envelope: %v", err)
	}
	for k := range envelope {
		if _, ok := want[k]; !ok {
			t.Errorf("--json stdout grew an undocumented top-level key %q", k)
		}
	}
	if len(envelope) != len(want) {
		t.Errorf("--json envelope has %d keys, documented schema has %d", len(envelope), len(want))
	}
	if !strings.Contains(errOut.String(), conflictNotice) {
		t.Fatalf("notice must still reach stderr under --json:\n%s", errOut.String())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestRunCallbackAddWithClient_IndependentWatchSuppresses proves the flag both
// reaches the daemon and results in silence on stderr.
func TestRunCallbackAddWithClient_IndependentWatchSuppresses(t *testing.T) {
	fake := &fakeCallbackClient{notice: conflictNotice}
	_, errOut := addCmdWithFlags(t, fake, map[string]string{"independent-watch": "true"})

	if fake.createReq == nil || !fake.createReq.GetIsIndependentWatch() {
		t.Fatalf("--independent-watch not forwarded: %+v", fake.createReq)
	}
	if strings.TrimSpace(errOut.String()) != "" {
		t.Fatalf("stderr should be silent under --independent-watch:\n%s", errOut.String())
	}
}

// TestRunCallbackAddWithClient_NoNoticeIsSilent guards the ordinary path: the
// advisory must stay rare, or readers learn to skim it.
func TestRunCallbackAddWithClient_NoNoticeIsSilent(t *testing.T) {
	fake := &fakeCallbackClient{}
	_, errOut := addCmdWithFlags(t, fake, nil)
	if strings.TrimSpace(errOut.String()) != "" {
		t.Fatalf("stderr should be empty with no conflict:\n%s", errOut.String())
	}
}

// TestCallbackAddIndependentWatchFlagIsRegistered pins the real command's flag
// set, since the test harness above declares its own.
func TestCallbackAddIndependentWatchFlagIsRegistered(t *testing.T) {
	root := callbackCmd()
	add, _, err := root.Find([]string{"add"})
	if err != nil {
		t.Fatalf("find add: %v", err)
	}
	f := add.Flags().Lookup("independent-watch")
	if f == nil {
		t.Fatal("boss callback add is missing --independent-watch")
	}
	if !strings.Contains(f.Usage, "outlive") {
		t.Errorf("--independent-watch help should say it records intent to outlive a sibling, got %q", f.Usage)
	}
}

// ---------------------------------------------------------------------------
// Adjacent fixes: --chat help and the group-size hint (BOS-1268 U6)
// ---------------------------------------------------------------------------

// TestCallbackRemoveChatHelpDoesNotClaimItIsIgnored pins the RULE, not the
// sentence: resolveCallbackChat honours --chat locally and
// runCallbackRemoveWithClient passes it as expect_target_chat_id, an ownership
// guard that returns PermissionDenied on a mismatch. Documenting it as ignored
// invites an operator to omit it and delete another chat's callback.
func TestCallbackRemoveChatHelpDoesNotClaimItIsIgnored(t *testing.T) {
	root := callbackCmd()
	remove, _, err := root.Find([]string{"remove"})
	if err != nil {
		t.Fatalf("find remove: %v", err)
	}
	f := remove.Flags().Lookup("chat")
	if f == nil {
		t.Fatal("boss callback remove is missing --chat")
	}
	if strings.Contains(strings.ToLower(f.Usage), "ignored") {
		t.Errorf("--chat help still claims it is ignored: %q", f.Usage)
	}
	if !strings.Contains(strings.ToLower(f.Usage), "ownership") {
		t.Errorf("--chat help should say it is the ownership guard: %q", f.Usage)
	}
}

func TestCallbackGroupSummary(t *testing.T) {
	cb := func(id, group string) *pb.GithubCallback {
		return &pb.GithubCallback{Id: id, GroupId: group}
	}
	cases := []struct {
		name string
		in   []*pb.GithubCallback
		want string
	}{
		{
			name: "empty",
			in:   nil,
			want: "Group sizes among the callbacks shown: none",
		},
		{
			// The split-pair shape: two groups of ONE, which is the whole point.
			name: "split pair reads as two groups of one",
			in:   []*pb.GithubCallback{cb("a", "ski109-final-pass"), cb("b", "ski109-final-fail")},
			want: "Group sizes among the callbacks shown: ski109-final-fail=1, ski109-final-pass=1",
		},
		{
			name: "correctly grouped pair reads as one group of two",
			in:   []*pb.GithubCallback{cb("a", "pr123-settle"), cb("b", "pr123-settle")},
			want: "Group sizes among the callbacks shown: pr123-settle=2",
		},
		{
			// Ungrouped rows must never be summed into one bucket: each is its
			// own group of one, which is why it cancels nothing.
			name: "ungrouped rows are counted as groups of one",
			in:   []*pb.GithubCallback{cb("a", ""), cb("b", ""), cb("c", "g1")},
			want: "Group sizes among the callbacks shown: g1=1, ungrouped=2 (each its own group of one)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := callbackGroupSummary(tc.in); got != tc.want {
				t.Fatalf("callbackGroupSummary =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestRunCallbackListWithClient_GroupSizeFooter proves a group of one is
// distinguishable from a group of two in the RENDERED output, and that the
// footer is honest about being scoped to the rows shown.
func TestRunCallbackListWithClient_GroupSizeFooter(t *testing.T) {
	cmd, out := newCallbackTestCmd()
	fake := &fakeCallbackClient{callbacks: []*pb.GithubCallback{
		{Id: "cb-1", GroupId: "pair", RepoOwner: "acme", RepoName: "widgets", PrNumber: 7, Trigger: "checks_passed", State: "active", TargetChatId: "chat-1"},
		{Id: "cb-2", GroupId: "pair", RepoOwner: "acme", RepoName: "widgets", PrNumber: 7, Trigger: "checks_failed", State: "active", TargetChatId: "chat-1"},
		{Id: "cb-3", GroupId: "lonely", RepoOwner: "acme", RepoName: "widgets", PrNumber: 7, Trigger: "merged", State: "active", TargetChatId: "chat-1"},
	}}
	if err := runCallbackListWithClient(cmd, fake); err != nil {
		t.Fatalf("list: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "pair=2") {
		t.Errorf("footer should report the group of two:\n%s", got)
	}
	if !strings.Contains(got, "lonely=1") {
		t.Errorf("footer should report the group of one:\n%s", got)
	}
	if !strings.Contains(got, "among the callbacks shown") {
		t.Errorf("footer must scope the tally to the listed rows:\n%s", got)
	}
	if !strings.Contains(got, "3 callbacks (complete listing)") {
		t.Errorf("existing footer line lost:\n%s", got)
	}
}

// The empty-list branch returns before the table, so it must not grow a group
// line that reads as a second, contradictory count.
func TestRunCallbackListWithClient_EmptyListHasNoGroupFooter(t *testing.T) {
	cmd, out := newCallbackTestCmd()
	if err := runCallbackListWithClient(cmd, &fakeCallbackClient{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "0 callbacks (complete listing)") {
		t.Fatalf("empty footer changed:\n%s", got)
	}
	if strings.Contains(got, "Group sizes") {
		t.Fatalf("empty listing should not print a group tally:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// BOS-1352: verifiable cleanup, strict flags, discoverable usage
// ---------------------------------------------------------------------------

func activeCB(id, chat string, pr int32) *pb.GithubCallback {
	return &pb.GithubCallback{Id: id, TargetChatId: chat, RepoOwner: "acme", RepoName: "widgets", PrNumber: pr, Trigger: "checks_passed", State: "active"}
}

// removeAllCmd builds the test command with --chat chat-1 and --all set, plus
// any extra flags.
func removeAllCmd(t *testing.T, flags map[string]string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd, out, errOut := newCallbackTestCmdSplit()
	set := map[string]string{"chat": "chat-1", "all": "true"}
	for k, v := range flags {
		set[k] = v
	}
	for k, v := range set {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	return cmd, out, errOut
}

func TestRunCallbackRemoveAll_RemovesOnlyThisChatsActiveRows(t *testing.T) {
	fake := &fakeCallbackClient{callbacks: []*pb.GithubCallback{
		activeCB("cb-1", "chat-1", 7),
		activeCB("cb-2", "chat-1", 7),
		activeCB("peer", "chat-2", 7),
		{Id: "fired", TargetChatId: "chat-1", PrNumber: 7, State: "delivered"},
	}}
	cmd, out, _ := removeAllCmd(t, nil)
	if err := runCallbackRemoveAllWithClient(cmd, fake); err != nil {
		t.Fatalf("remove --all: %v", err)
	}
	if strings.Join(fake.deletedIDs, ",") != "cb-1,cb-2" {
		t.Fatalf("deleted %v, want only chat-1's active rows [cb-1 cb-2]", fake.deletedIDs)
	}
	if fake.deletedChat != "chat-1" {
		t.Fatalf("delete ran as chat %q, want the ownership guard chat-1", fake.deletedChat)
	}
	for _, req := range fake.listReqs {
		if req.GetTargetChatId() != "chat-1" {
			t.Fatalf("list request not scoped to chat-1: %+v", req)
		}
	}
	if len(fake.listReqs) != 2 {
		t.Fatalf("want a select list and a post-condition re-list, got %d list calls", len(fake.listReqs))
	}
	if got := out.String(); got != "Removed 2 callback(s) for chat chat-1; 0 active remain.\n" {
		t.Fatalf("stdout = %q", got)
	}
}

// A leased or triggered row has not delivered yet and still wakes the chat,
// so --all must remove it rather than report "0 active remain" around it.
func TestRunCallbackRemoveAll_RemovesLeasedAndTriggeredRows(t *testing.T) {
	leased := activeCB("leased", "chat-1", 7)
	leased.State = "leased"
	triggered := activeCB("triggered", "chat-1", 7)
	triggered.State = "triggered"
	fake := &fakeCallbackClient{callbacks: []*pb.GithubCallback{
		activeCB("cb-1", "chat-1", 7),
		leased,
		triggered,
		{Id: "fired", TargetChatId: "chat-1", PrNumber: 7, State: "delivered"},
	}}
	cmd, out, _ := removeAllCmd(t, nil)
	if err := runCallbackRemoveAllWithClient(cmd, fake); err != nil {
		t.Fatalf("remove --all: %v", err)
	}
	if strings.Join(fake.deletedIDs, ",") != "cb-1,leased,triggered" {
		t.Fatalf("deleted %v, want every live row [cb-1 leased triggered]", fake.deletedIDs)
	}
	if got := out.String(); got != "Removed 3 callback(s) for chat chat-1; 0 active remain.\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRunCallbackRemoveAll_PRScope(t *testing.T) {
	fake := &fakeCallbackClient{callbacks: []*pb.GithubCallback{
		activeCB("cb-7", "chat-1", 7),
		activeCB("cb-8", "chat-1", 8),
	}}
	cmd, out, _ := removeAllCmd(t, map[string]string{"pr": "7", "repo": "acme/widgets"})
	if err := runCallbackRemoveAllWithClient(cmd, fake); err != nil {
		t.Fatalf("remove --all --pr 7: %v", err)
	}
	if strings.Join(fake.deletedIDs, ",") != "cb-7" {
		t.Fatalf("deleted %v, want [cb-7] only", fake.deletedIDs)
	}
	if req := fake.listReqs[0]; req.GetRepoOwner() != "acme" || req.GetRepoName() != "widgets" {
		t.Fatalf("--repo not forwarded: %+v", req)
	}
	if got := out.String(); got != "Removed 1 callback(s) for chat chat-1 PR #7; 0 active remain.\n" {
		t.Fatalf("stdout = %q", got)
	}
}

// The zero-match case is the one the old list-jq-remove loop got wrong: it
// printed nothing, which read as success. It must say so explicitly.
func TestRunCallbackRemoveAll_ZeroMatchIsExplicit(t *testing.T) {
	fake := &fakeCallbackClient{callbacks: []*pb.GithubCallback{activeCB("peer", "chat-2", 7)}}
	cmd, out, _ := removeAllCmd(t, map[string]string{"pr": "7"})
	if err := runCallbackRemoveAllWithClient(cmd, fake); err != nil {
		t.Fatalf("zero-match must exit 0: %v", err)
	}
	if len(fake.deletedIDs) != 0 {
		t.Fatalf("nothing should be deleted, got %v", fake.deletedIDs)
	}
	if got := out.String(); got != "Removed 0 callback(s) for chat chat-1 PR #7; 0 active remain.\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRunCallbackRemoveAll_SurvivorFailsPostCondition(t *testing.T) {
	fake := &fakeCallbackClient{
		callbacks: []*pb.GithubCallback{activeCB("cb-1", "chat-1", 7), activeCB("stuck", "chat-1", 7)},
		stickyIDs: map[string]bool{"stuck": true},
	}
	cmd, out, _ := removeAllCmd(t, nil)
	err := runCallbackRemoveAllWithClient(cmd, fake)
	if err == nil {
		t.Fatal("a row still active after removal must fail the command")
	}
	if !strings.Contains(err.Error(), "stuck") || !strings.Contains(err.Error(), "remain") {
		t.Fatalf("error must name the surviving id: %v", err)
	}
	if got := out.String(); got != "Removed 1 callback(s) for chat chat-1; 1 active remain: stuck.\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRunCallbackRemoveAll_VanishedRowCountsAsGone(t *testing.T) {
	for _, asError := range []bool{false, true} {
		t.Run(fmt.Sprintf("asError=%v", asError), func(t *testing.T) {
			fake := &fakeCallbackClient{
				callbacks:       []*pb.GithubCallback{activeCB("cb-1", "chat-1", 7), activeCB("gone", "chat-1", 7)},
				vanishedIDs:     map[string]bool{"gone": true},
				vanishedAsError: asError,
			}
			cmd, out, _ := removeAllCmd(t, nil)
			if err := runCallbackRemoveAllWithClient(cmd, fake); err != nil {
				t.Fatalf("a row that vanished between list and delete must not fail cleanup: %v", err)
			}
			if got := out.String(); got != "Removed 2 callback(s) for chat chat-1; 0 active remain.\n" {
				t.Fatalf("stdout = %q", got)
			}
		})
	}
}

func TestRunCallbackRemoveAll_DeleteErrorFails(t *testing.T) {
	fake := &fakeCallbackClient{
		callbacks: []*pb.GithubCallback{activeCB("cb-1", "chat-1", 7)},
		deleteErr: connect.NewError(connect.CodeUnavailable, errors.New("daemon down")),
	}
	cmd, _, _ := removeAllCmd(t, nil)
	err := runCallbackRemoveAllWithClient(cmd, fake)
	if err == nil || !strings.Contains(err.Error(), "cb-1") {
		t.Fatalf("a failed delete must fail the command naming the id, got %v", err)
	}
}

func TestRunCallbackRemoveAll_JSONOnStdout(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fake := &fakeCallbackClient{callbacks: []*pb.GithubCallback{activeCB("cb-1", "chat-1", 7)}}
		cmd, out, errOut := removeAllCmd(t, map[string]string{"pr": "7", "json": "true"})
		if err := runCallbackRemoveAllWithClient(cmd, fake); err != nil {
			t.Fatalf("remove --all --json: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
		}
		want := map[string]any{"removed": []any{"cb-1"}, "remaining_active": []any{}, "chat": "chat-1", "pr_number": float64(7)}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("JSON = %v, want %v", got, want)
		}
		if errOut.Len() != 0 {
			t.Fatalf("stderr should be empty: %q", errOut.String())
		}
	})
	t.Run("no pr is null and zero-match lists are empty arrays", func(t *testing.T) {
		cmd, out, _ := removeAllCmd(t, map[string]string{"json": "true"})
		if err := runCallbackRemoveAllWithClient(cmd, &fakeCallbackClient{}); err != nil {
			t.Fatalf("remove --all --json: %v", err)
		}
		if got := strings.Join(strings.Fields(out.String()), ""); got != `{"removed":[],"remaining_active":[],"chat":"chat-1","pr_number":null}` {
			t.Fatalf("stdout = %s", got)
		}
	})
	t.Run("failure still writes one envelope and is marked reported", func(t *testing.T) {
		fake := &fakeCallbackClient{
			callbacks: []*pb.GithubCallback{activeCB("stuck", "chat-1", 7)},
			stickyIDs: map[string]bool{"stuck": true},
		}
		cmd, out, _ := removeAllCmd(t, map[string]string{"json": "true"})
		err := runCallbackRemoveAllWithClient(cmd, fake)
		var reported *jsonReportedError
		if err == nil || !errors.As(err, &reported) {
			t.Fatalf("want a reported failure, got %v", err)
		}
		var got callbackRemoveAllJSON
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
		}
		if strings.Join(got.RemainingActive, ",") != "stuck" || len(got.Removed) != 0 {
			t.Fatalf("envelope = %+v", got)
		}
	})
}

func TestValidateCallbackRemoveArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		flags   map[string]string
		wantAll bool
		wantErr string
	}{
		{name: "id alone", args: []string{"cb-1"}},
		{name: "all alone", flags: map[string]string{"all": "true"}, wantAll: true},
		{name: "all with scope", flags: map[string]string{"all": "true", "pr": "7", "json": "true"}, wantAll: true},
		{name: "all and id refused", args: []string{"cb-1"}, flags: map[string]string{"all": "true"}, wantErr: "not both"},
		{name: "neither refused", wantErr: "--all"},
		{name: "pr without all refused", args: []string{"cb-1"}, flags: map[string]string{"pr": "7"}, wantErr: "--pr only applies with --all"},
		{name: "json without all refused", args: []string{"cb-1"}, flags: map[string]string{"json": "true"}, wantErr: "--json only applies with --all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _ := newCallbackTestCmd()
			for k, v := range tc.flags {
				if err := cmd.Flags().Set(k, v); err != nil {
					t.Fatalf("set %s: %v", k, err)
				}
			}
			all, err := validateCallbackRemoveArgs(cmd, tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if all != tc.wantAll {
				t.Fatalf("all = %v, want %v", all, tc.wantAll)
			}
		})
	}
}

// The real command must refuse both shapes before building a client.
func TestCallbackRemoveCommand_RefusesAmbiguousTargets(t *testing.T) {
	for _, argv := range [][]string{{"remove"}, {"remove", "cb-1", "--all"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			root := callbackCmd()
			root.SetArgs(argv)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			if err := root.Execute(); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}
}

func TestRunCallbackListWithClient_PRFilter(t *testing.T) {
	cmd, out := newCallbackTestCmd()
	if err := cmd.Flags().Set("pr", "7"); err != nil {
		t.Fatalf("set pr: %v", err)
	}
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json: %v", err)
	}
	fake := &fakeCallbackClient{callbacks: []*pb.GithubCallback{
		activeCB("cb-7", "chat-1", 7),
		activeCB("cb-8", "chat-1", 8),
	}}
	if err := runCallbackListWithClient(cmd, fake); err != nil {
		t.Fatalf("list --pr: %v", err)
	}
	var rows []githubCallbackJSON
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if len(rows) != 1 || rows[0].ID != "cb-7" {
		t.Fatalf("--pr 7 returned %+v, want only cb-7", rows)
	}
	// The filter must not have mutated the client's own storage.
	if len(fake.callbacks) != 2 {
		t.Fatalf("list --pr mutated the source rows: %d left", len(fake.callbacks))
	}
}

func TestCallbackPRFilterRejectsNonPositive(t *testing.T) {
	cmd, _ := newCallbackTestCmd()
	if err := cmd.Flags().Set("pr", "0"); err != nil {
		t.Fatalf("set pr: %v", err)
	}
	if _, err := callbackPRFilter(cmd); err == nil || !strings.Contains(err.Error(), "--pr") {
		t.Fatalf("--pr 0 must be refused, got %v", err)
	}
}

// executeCallbackAdd runs the REAL `boss callback add` command so cobra's flag
// parsing — and the FlagErrorFunc installed on it — is exercised.
func executeCallbackAdd(t *testing.T, args ...string) error {
	t.Helper()
	root := callbackCmd()
	root.SetArgs(append([]string{"add"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

func TestCallbackAdd_EventFlagsNameTheRealSignature(t *testing.T) {
	for _, flag := range []string{"--on", "--trigger", "--pr"} {
		t.Run(flag, func(t *testing.T) {
			err := executeCallbackAdd(t, "1", "merged", flag, "settled", "--message", "x")
			if err == nil {
				t.Fatal("expected a flag error")
			}
			msg := err.Error()
			for _, want := range []string{"unknown flag: " + flag, "boss callback add <pr> <trigger>", "boss broadcast subscribe"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q missing %q", msg, want)
				}
			}
			if code := errorCodeFor(err); code != codeInvalidArgument {
				t.Errorf("code = %q, want %q", code, codeInvalidArgument)
			}
		})
	}
}

func TestCallbackAdd_UnrelatedUnknownFlagKeepsCobraMessage(t *testing.T) {
	err := executeCallbackAdd(t, "1", "merged", "--bogus-flag", "--message", "x")
	if err == nil {
		t.Fatal("expected a flag error")
	}
	if !strings.Contains(err.Error(), "unknown flag: --bogus-flag") {
		t.Fatalf("cobra's message lost: %q", err.Error())
	}
	if strings.Contains(err.Error(), "boss broadcast subscribe") {
		t.Fatalf("signature hint leaked onto an unrelated flag: %q", err.Error())
	}
}

// The hint must still reach a --json caller as the root backstop's envelope.
func TestCallbackAdd_EventFlagHintFlowsThroughJSONBackstop(t *testing.T) {
	root := &cobra.Command{Use: "boss", SilenceErrors: true, SilenceUsage: true}
	installFlagErrorHook(root)
	root.AddCommand(callbackCmd())
	argv := []string{"callback", "add", "1", "merged", "--on", "settled", "--message", "x", "--json"}
	root.SetArgs(argv)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	err := emitRootJSONFailure(root, argv, root.Execute())
	if err == nil {
		t.Fatal("expected failure")
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &env); jerr != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", jerr, out.String())
	}
	if env.Error.Code != codeInvalidArgument || !strings.Contains(env.Error.Message, "boss callback add <pr> <trigger>") {
		t.Fatalf("envelope missing code or hint:\n%s", out.String())
	}
}

// The fan-out loops pipe trigger names on stdin; `add` must never read it, or
// the first leg would swallow the rest of the list.
func TestRunCallbackAddWithClient_LeavesStdinUnconsumed(t *testing.T) {
	const pending = "checks_failed\nmerged\n"
	cmd, _, _ := newCallbackTestCmdSplit()
	cmd.SetIn(strings.NewReader(pending))
	for k, v := range map[string]string{"chat": "chat-1", "repo": "acme/widgets", "message": "m"} {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	if err := runCallbackAddWithClient(cmd, &fakeCallbackClient{}, "123", "checks_passed"); err != nil {
		t.Fatalf("add: %v", err)
	}
	rest, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if string(rest) != pending {
		t.Fatalf("add consumed stdin: %q left of %q", rest, pending)
	}
}

// The group key is group_id. A `.group` read yields null because that key does
// not exist — this pins the receipt end to end through the fake daemon.
func TestRunCallbackAddWithClient_JSONCarriesGroupID(t *testing.T) {
	fake := &fakeCallbackClient{}
	out, _ := addCmdWithFlags(t, fake, map[string]string{"group": "g1", "json": "true"})
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if got["group_id"] != "g1" {
		t.Fatalf("group_id = %v, want g1 (stdout %s)", got["group_id"], out.String())
	}
	if _, ok := got["group"]; ok {
		t.Fatalf("schema grew an undocumented `group` alias: %s", out.String())
	}
}
