package ui

import (
	"slices"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestProof_Sessions_AC208704(t *testing.T) {
	session := &pb.Session{Attention: &pb.SessionAttention{ProofUrl: "https://proof.test/page", Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_PROOF}}}
	for _, details := range []bool{false, true} {
		if got := AttentionFooter(session, details); len(got) != 1 || got[0] != "[p] proof" {
			t.Fatalf("missing proof control: %v", got)
		}
		if b, ok := AttentionBinding(session, "p", details); !ok || b.Action != pb.AttentionAction_ATTENTION_ACTION_PROOF {
			t.Fatal("proof shortcut is unavailable")
		}
	}
	var opened string
	got := AttentionActionCmd(t.Context(), nil, session, pb.AttentionAction_ATTENTION_ACTION_PROOF, func(url string) error { opened = url; return nil })()
	if opened != session.Attention.ProofUrl {
		t.Fatalf("opened %q, want proof page", opened)
	}
	if result, ok := got.(AttentionActionResultMsg); !ok || result.Err != nil || result.Action != pb.AttentionAction_ATTENTION_ACTION_PROOF {
		t.Fatalf("unexpected proof result %v", got)
	}
	if _, ok := AttentionBinding(&pb.Session{}, "p", true); ok {
		t.Fatal("proof shortcut must not act when unavailable")
	}
}

// proofCases is a session for each ProofTarget case, in its order: a proof
// page, a legacy video, a verify park's PR conversation, and nothing.
func proofCases() []struct {
	name    string
	session *pb.Session
	want    string
} {
	pr := "https://github.com/org/repo/pull/7"
	review := pb.AttentionKind_ATTENTION_KIND_REVIEW
	github, proof, video, merge := pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_PROOF, pb.AttentionAction_ATTENTION_ACTION_VIDEO, pb.AttentionAction_ATTENTION_ACTION_MERGE
	return []struct {
		name    string
		session *pb.Session
		want    string
	}{
		{"proof page", &pb.Session{PrUrl: &pr, Attention: &pb.SessionAttention{Kind: review, ParkCodes: []string{"human-review:ui"}, Actions: []pb.AttentionAction{github, proof, merge}, ProofUrl: "https://proof.test/page", EvidenceUrl: "https://proof.test/video.mp4"}}, "https://proof.test/page"},
		{"legacy video", &pb.Session{PrUrl: &pr, Attention: &pb.SessionAttention{Kind: review, ParkCodes: []string{"human-review:ui"}, Actions: []pb.AttentionAction{github, video, merge}, EvidenceUrl: "https://proof.test/video.mp4"}}, "https://proof.test/video.mp4"},
		{"verify comment", &pb.Session{PrUrl: &pr, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, ParkCodes: []string{"ledger-open"}, Actions: []pb.AttentionAction{github}}}, pr},
		{"none", &pb.Session{PrUrl: &pr, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, Actions: []pb.AttentionAction{merge, github}}}, ""},
	}
}

func TestProofTargetOrder_Sessions_AC208902(t *testing.T) {
	for _, tc := range proofCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ProofTarget(tc.session)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("ProofTarget = %q, %v; want %q", got, ok, tc.want)
			}
			for _, details := range []bool{false, true} {
				footer := AttentionFooter(tc.session, details)
				if slices.Contains(footer, "[p] proof") != ok {
					t.Errorf("details=%v footer %v, want [p] proof listed exactly when there is a target", details, footer)
				}
				for _, label := range footer {
					if strings.HasPrefix(label, "[v]") {
						t.Errorf("details=%v footer offers %q", details, label)
					}
				}
				if _, bound := AttentionBinding(tc.session, "v", details); bound {
					t.Errorf("details=%v: v is bound", details)
				}
				b, bound := AttentionBinding(tc.session, "p", details)
				if bound != ok {
					t.Fatalf("details=%v: p bound=%v, want %v", details, bound, ok)
				}
				if !bound {
					continue
				}
				var opened string
				msg := AttentionActionCmd(t.Context(), nil, tc.session, b.Action, func(url string) error { opened = url; return nil })()
				if result, _ := msg.(AttentionActionResultMsg); opened != tc.want || result.Text != "Opened proof" {
					t.Errorf("details=%v: p opened %q (%q), want %q", details, opened, result.Text, tc.want)
				}
			}
		})
	}
}

// TestProofStandsWhereTheDaemonListedIt_Sessions_AC208902 keeps [p] proof between github and
// merge whether the daemon served PROOF, VIDEO or neither.
func TestProofStandsWhereTheDaemonListedIt_Sessions_AC208902(t *testing.T) {
	for _, tc := range proofCases()[:3] {
		if got, want := AttentionFooter(tc.session, true), []string{"[g] github", "[p] proof"}; !slices.Equal(got[:min(2, len(got))], want) {
			t.Errorf("%s: details footer = %v, want it to open %v", tc.name, got, want)
		}
	}
}
