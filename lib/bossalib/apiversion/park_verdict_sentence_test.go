package apiversion

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func TestParkVerdictSentenceFixtureCompatibility_Sessions_AC215002(t *testing.T) {
	data, err := os.ReadFile("../attention/testdata/park_verdicts.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Codes        []string `json:"codes"`
		Label        string   `json:"label"`
		EvidenceURL  string   `json:"evidenceUrl"`
		VideoSeconds int      `json:"videoSeconds"`
		OpenItems    int      `json:"openItems"`
		Sentence     string   `json:"sentence"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	// Frozen pre-verdict response sentences; these must not read the new verdict implementation.
	fixed := map[string]string{"human-review:database": "Database migration", "human-review:unproven": "Some checks have no evidence", "partial-build": "Partial build", "no-receipt": "No review of the latest commit", "human-merge": "Verified · a person merges"}
	// Admission holds keep their actionable attention copy at both version pins.
	admission := map[string]string{
		"ticket-needs-person": "Ticket waits on you · answer it or label it for build",
		"ticket-not-selected": "Ticket not selected for verify · relabel it or merge by hand",
	}
	for _, r := range rows {
		t.Run(r.Sentence, func(t *testing.T) {
			in := attention.Input{ParkCodes: r.Codes, ParkLabel: r.Label, OpenItems: make([]string, r.OpenItems), Evidence: &attention.Evidence{URL: r.EvidenceURL, VideoDuration: time.Duration(r.VideoSeconds) * time.Second}}
			a := attention.Compute(in)
			code, _ := attention.DecidingParkCode(r.Codes)
			want := fixed[code]
			switch code {
			case "human-review:ui":
				want = "UI change"
				if r.EvidenceURL != "" {
					want += " · evidence video"
					if r.VideoSeconds == 83 {
						want += " (1:23)"
					}
				}
			case "always-human-path", "human-review:security":
				want = "Security change"
				if r.Label != "" {
					want += ": " + r.Label
				}
			case "ledger-open":
				want = "The PR checklist has open items"
				if r.OpenItems == 1 {
					want = "The PR checklist has 1 open item"
				}
				if r.OpenItems == 2 {
					want = "The PR checklist has 2 open items"
				}
			}
			if want == "" {
				want = "Verify is blocked"
			}
			current := r.Sentence
			if sentence, ok := admission[code]; ok {
				want, current = sentence, sentence
			}
			for _, v := range []Version{V20260930, V20261031} {
				resp := &pb.ProxyGetSessionResponse{Session: &pb.Session{Attention: a}}
				ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
				expected, label := want, ""
				if v == V20261031 {
					expected, label = current, r.Label
				}
				if got := resp.Session.Attention; got.GetSentence() != expected || got.GetParkLabel() != label {
					t.Fatalf("pin %s attention = %v, want %q label %q", v, got, expected, label)
				}
				if a.GetSentence() != current || a.GetParkLabel() != r.Label {
					t.Fatal("transform mutated cached current attention")
				}
			}
		})
	}
}

func TestParkVerdictSentenceKeepsDurationWhenLabelNamesEvidence(t *testing.T) {
	current := attention.Compute(attention.Input{
		ParkCodes: []string{"human-review:ui"},
		ParkLabel: "the UI · evidence video recorder",
		Evidence:  &attention.Evidence{URL: "https://example.com/video", VideoDuration: 83 * time.Second},
	})
	resp := &pb.ProxyGetSessionResponse{Session: &pb.Session{Attention: current}}
	ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, V20260930)
	if got := resp.GetSession().GetAttention().GetSentence(); got != "UI change · evidence video (1:23)" {
		t.Fatalf("old pin sentence = %q", got)
	}
}
