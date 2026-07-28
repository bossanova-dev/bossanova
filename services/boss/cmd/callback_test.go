package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/table"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

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
