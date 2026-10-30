package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/api/upstream"
)

// TestMergeSessionRequiresHeadAndVerdict_Sessions_AC139_AC140_AC143 proves sessions.AC139, sessions.AC140 and sessions.AC143.
func TestMergeSessionRequiresHeadAndVerdict_Sessions_AC139_AC140_AC143(t *testing.T) {
	for _, tc := range []struct {
		name, pin, status, want     string
		verified, conflict, failing bool
	}{
		{name: "missing pin", want: "EXPECTED_HEAD_REQUIRED"},
		{name: "no verdict", pin: headPin, want: "NO_VERDICT"},
		{name: "parked", pin: headPin, status: "needs human: human-review:ui", want: "human-review:ui"},
		{name: "verifying", pin: headPin, status: "verifying… claim", want: "being verified"},
		{name: "CI wins", pin: headPin, failing: true, want: "gate=ci"},
		{name: "conflict wins", pin: headPin, conflict: true, want: "gate=conflict"},
		{name: "verified", pin: headPin, verified: true},
		{name: "verified approved", pin: headPin, verified: true, status: "verified (approved)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			h.prov.checks = nil
			if tc.verified {
				h.prov.checks = livePassingChecks()
				h.prov.checks[0].Description = tc.status
			} else if tc.status != "" {
				h.prov.checks = []vcs.CheckResult{{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: tc.status}}
			}
			if tc.failing {
				h.prov.checks = append(h.prov.checks, vcs.CheckResult{Name: "CI", Status: vcs.CheckStatusCompleted, Conclusion: checkConclusionPtr(vcs.CheckConclusionFailure)})
			}
			if tc.conflict {
				h.prov.prStatus.Mergeable = boolPtr(false)
				h.prov.prStatus.MergeStateStatus = vcs.MergeStateStatusDirty
			}
			_, err := h.srv.MergeSession(context.Background(), pinnedMergeRequest(tc.pin))
			if tc.want == "" {
				if err != nil || !h.prov.mergeCalled {
					t.Fatalf("verified merge = %v, called=%v", err, h.prov.mergeCalled)
				}
				if h.prov.mergeOpts[0].ExpectedHeadSHA != headPin {
					t.Fatal("merge lost pin")
				}
				return
			}
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), tc.want) || h.prov.mergeCalled {
				t.Fatalf("refusal = %v, called=%v; want %s", err, h.prov.mergeCalled, tc.want)
			}
			if (tc.conflict || tc.failing) && strings.Contains(err.Error(), "NO_VERDICT") {
				t.Fatalf("existing gate must precede verdict: %v", err)
			}
			if tc.pin != "" && !tc.conflict && !tc.failing {
				if !errors.Is(err, vcs.ErrNoVerdict) {
					t.Fatalf("missing sentinel: %v", err)
				}
				for _, want := range []string{headPin, "/boss-verify", "boss merge --approve"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal missing %q: %v", want, err)
					}
				}
			}
		})
	}
}

// TestOperatorMergeSession_UnverifiedMergeRecordsApproval_ChatPicker_AC21_Sessions_AC142 proves chat-picker.AC21 and sessions.AC142.
func TestOperatorMergeSession_UnverifiedMergeRecordsApproval_ChatPicker_AC21_Sessions_AC142(t *testing.T) {
	for _, verified := range []bool{false, true} {
		t.Run(map[bool]string{false: "unverified", true: "verified"}[verified], func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			if !verified {
				h.prov.checks = nil
			}
			resp, err := h.merge(false)
			if err != nil || !resp.GetIsMerged() {
				t.Fatalf("operator merge: %v, %v", resp, err)
			}
			if verified {
				if len(h.verbs.forMerge) != 0 {
					t.Fatal("verified head must not record approval")
				}
			} else if len(h.verbs.forMerge) != 1 || h.verbs.forMerge[0].Head != headPin {
				t.Fatalf("approval pin = %+v", h.verbs.forMerge)
			}
		})
	}
}

func TestStreamMergeRequiresPinAndVerdict_Sessions_AC146_McpServer_AC4(t *testing.T) {
	for _, tc := range []struct{ pin, want string }{
		{"", "EXPECTED_HEAD_REQUIRED"}, {headPin, "NO_VERDICT"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			h.prov.checks = nil
			adapter := &upstream.CommandHandlerAdapter{Commands: h.srv}
			_, err := adapter.MergeSession(context.Background(), "s1", tc.pin)
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), tc.want) || h.prov.mergeCalled {
				t.Fatalf("stream merge = %v, provider called=%v", err, h.prov.mergeCalled)
			}
		})
	}
}

func TestOperatorMergeSessionApprovalAtOtherHeadCannotAuthorizeMerge(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	h.prov.checks = nil
	h.verbs.approve.HeadSHA = strings.Repeat("f", 40)
	_, err := h.merge(true)
	if !errors.Is(err, vcs.ErrNoVerdict) && !strings.Contains(err.Error(), "NO_VERDICT") {
		t.Fatalf("want NO_VERDICT: %v", err)
	}
	if h.prov.mergeCalled {
		t.Fatal("approval of another head merged")
	}
}
