package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

func (f *fakeCallbackClient) ListGithubCallbacks(context.Context, *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error) {
	return f.callbacks, nil
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
			State:        "active",
		},
		NoticeText: notice,
	}, nil
}

func (f *fakeCallbackClient) DeleteGithubCallback(_ context.Context, targetChatID, id string) (*pb.DeleteGithubCallbackResponse, error) {
	f.deletedChat = targetChatID
	f.deletedID = id
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if f.deleteResp != nil {
		return f.deleteResp, nil
	}
	return &pb.DeleteGithubCallbackResponse{Outcome: "deleted"}, nil
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
