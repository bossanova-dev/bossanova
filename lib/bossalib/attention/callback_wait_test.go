package attention

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCallbackWaitAgreement_ChatPicker_AC225802(t *testing.T) {
	for _, tc := range []struct {
		trigger     string
		total, done int
		want        string
	}{
		{"checks_passed_ready", 20, 12, "checks 12/20"},
		{"checks_passed_ready", 20, 20, "waiting: retry"},
		{"checks_failed", 20, 20, "waiting: retry"},
		{"checks_passed", 0, 0, "waiting: CI"},
		{"ready_for_review", 0, 0, "waiting: ready"},
		{"merged", 0, 0, "waiting: merge"},
		{"closed", 0, 0, "waiting: close"},
		{"", 0, 0, "waiting"},
	} {
		t.Run(tc.trigger+tc.want, func(t *testing.T) {
			in := Input{HasPR: true, PRState: PRStateOpen, ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING, ChecksTotal: tc.total, ChecksDone: tc.done, Wait: CallbackWait{Trigger: tc.trigger}}
			served := Compute(in)
			if got := served.GetStatusPhrase(); got != tc.want {
				t.Fatalf("row = %q, want %q", got, tc.want)
			}
			if got := ChatWaitLabel(tc.trigger, served); got != tc.want {
				t.Errorf("chat = %q, row %q", got, tc.want)
			}
		})
	}
	in := Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING, Wait: CallbackWait{Trigger: "merged", OtherPR: 2960}}
	if got := Compute(in).GetProgressLabel(); got != "waiting: #2960" {
		t.Fatal(got)
	}
}

func TestChatWaitPRFacts(t *testing.T) {
	for _, label := range []string{"checking 12/20", "tests busy", "queued", "main red", "rebasing", "waiting: attached"} {
		if got := ChatWaitLabel("checks_passed_ready", &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: label}); got != label {
			t.Errorf("%q became %q", label, got)
		}
	}
	for _, label := range []string{"working", "building", "idle", "stopped"} {
		if got := ChatWaitLabel("checks_passed_ready", &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: label}); got != "waiting: CI" {
			t.Errorf("%q became %q", label, got)
		}
	}
	if got := ChatWaitLabel("checks_passed_ready", &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE}); got != "waiting: retry" {
		t.Fatal(got)
	}
	// A daemon that serves the row's phrase gives the chat the same words.
	served := &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "checking 12/14", StatusPhrase: "checks 12/14"}
	if got := ChatWaitLabel("checks_passed_ready", served); got != "checks 12/14" {
		t.Errorf("served phrase became %q, want the row's checks 12/14", got)
	}
}

func TestChatWaitDoesNotInferChecksFromOtherCallback(t *testing.T) {
	for _, label := range []string{"waiting", "waiting: CI", "waiting: ready", "waiting: merge", "waiting: close", "waiting: #2960"} {
		t.Run(label, func(t *testing.T) {
			served := &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: label}
			if got := ChatWaitLabel("checks_passed_ready", served); got != "waiting: CI" {
				t.Errorf("checks callback with served %q = %q, want waiting: CI", label, got)
			}
		})
	}
	served := &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "waiting: retry"}
	if got := ChatWaitLabel("checks_passed_ready", served); got != "waiting: retry" {
		t.Errorf("completed checks = %q, want waiting: retry", got)
	}
}
