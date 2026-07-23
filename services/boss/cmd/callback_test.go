package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/githubcallback"
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

func TestTriggerLabel(t *testing.T) {
	// Bind expectations to the canonical trigger order so a reordering of
	// ValidTriggers() is caught here rather than silently mislabeling output.
	triggers := githubcallback.ValidTriggers()
	cases := map[string]string{
		string(triggers[0]): "is merged",
		string(triggers[1]): "is closed",
		string(triggers[2]): "passes checks",
		string(triggers[3]): "fails checks",
		"unknown-trigger":   "unknown-trigger", // unmapped falls through verbatim
	}
	for in, want := range cases {
		if got := triggerLabel(in); got != want {
			t.Errorf("triggerLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
