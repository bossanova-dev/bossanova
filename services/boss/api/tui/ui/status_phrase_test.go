package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/attention"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// servedPark is a sign-off park as bossd serves it, phrase included.
func servedPark(codes ...string) *pb.Session {
	return &pb.Session{
		Title: "Hide the line", DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		DisplayLabel: "needs sign-off", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		Attention: attention.Compute(attention.Input{HasPR: true, PRState: attention.PRStateOpen, ParkCodes: codes}),
	}
}

// TestSessionHeaderNamesTheServedPhrase_ChatPicker_AC233601: the session
// header's status suffix is the phrase the home row shows, a daemon without
// phrases keeps the display status, and an ending label still wins.
func TestSessionHeaderNamesTheServedPhrase_ChatPicker_AC233601(t *testing.T) {
	sp := NewStatusSpinner()
	park := servedPark("human-review:ui")
	if park.GetAttention().GetStatusPhrase() != "review UI" {
		t.Fatalf("precondition: served phrase %q", park.GetAttention().GetStatusPhrase())
	}
	banner := ansi.Strip(RenderBanner(ViewChatPicker, BannerOpts{Session: park, Width: 160}))
	if !strings.Contains(banner, "Hide the line (review UI)") {
		t.Errorf("header does not name the served phrase:\n%s", banner)
	}

	older := servedPark("human-review:ui")
	older.Attention.StatusPhrase, older.Attention.StatusDetail = "", ""
	if got := SessionStatusText(older, sp); got != RenderDisplayStatus(older, sp) {
		t.Errorf("older daemon header = %q, want the display status", got)
	}

	merging := servedPark("human-review:ui")
	merging.DisplayLabel = "merging"
	if got := ansi.Strip(SessionStatusText(merging, sp)); got != "merging" {
		t.Errorf("merging header = %q, want the ending label", got)
	}
}

// TestRenderStatusPhraseStylesInProgressWork: checks style like the running
// checks label and a repair like live work, whatever chat state the label
// reports; both lead with the spinner.
func TestRenderStatusPhraseStylesInProgressWork(t *testing.T) {
	sp := NewStatusSpinner()
	for _, tc := range []struct {
		label, phrase string
		style         func(...string) string
		spin          bool
	}{
		{"checking 12/14", "checking 12/14", StyleStatusWarning.Render, true},
		{"checking 12/14", "checks 12/14", StyleStatusWarning.Render, true},
		{"idle", "repairing 2 checks", StyleStatusSuccess.Render, true},
		{attention.RepairingLabel, "repairing 1 P2", StyleStatusSuccess.Render, true},
		{attention.RebasingLabel, "rebasing", StyleStatusSuccess.Render, true},
		{"waiting: a-reason-far-too-long-for-the-cell", "waiting", StyleStatusInfo.Render, true},
		{"stopped", "stopped", StyleStatusMuted.Render, false},
	} {
		a := &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: tc.label, StatusPhrase: tc.phrase}
		got, ok := RenderStatusPhrase(a, sp)
		want := tc.phrase
		if tc.spin {
			want = sp.View() + want
		}
		if !ok || got != tc.style(want) {
			t.Errorf("%q over %q = %q, want %q", tc.phrase, tc.label, got, tc.style(want))
		}
	}
	if _, ok := RenderStatusPhrase(&pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW}, sp); ok {
		t.Error("a phrase-less attention rendered a phrase")
	}
}

// TestSessionStatusDetailIsOneLine: the detail line folds served whitespace
// onto one line and is empty without attention.
func TestSessionStatusDetailIsOneLine(t *testing.T) {
	s := &pb.Session{Attention: &pb.SessionAttention{StatusDetail: "API schema,\n  API compatibility · no proof yet"}}
	if got := SessionStatusDetail(s); got != "API schema, API compatibility · no proof yet" {
		t.Errorf("detail = %q", got)
	}
	if got := SessionStatusDetail(&pb.Session{}); got != "" {
		t.Errorf("no attention gives %q", got)
	}
}
