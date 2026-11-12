package attention

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestParkLevelSharedFixture uses the same contract cases as verify-level.test.mjs.
func TestParkLevelSharedFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/park_level.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Name  string   `json:"name"`
		Codes []string `json:"codes"`
		Label string   `json:"label"`
		Level *string  `json:"level"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	levels := map[string]pb.ConfidenceLevel{
		"high":          pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH,
		"moderate":      pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE,
		"low":           pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW,
		"cannot-assess": pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS,
	}
	for _, row := range rows {
		t.Run(row.Name, func(t *testing.T) {
			want := pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
			if row.Level != nil {
				var ok bool
				want, ok = levels[*row.Level]
				if !ok {
					t.Fatalf("unknown fixture level %q", *row.Level)
				}
			}
			if got := ParkLevel(row.Codes, row.Label); got != want {
				t.Errorf("ParkLevel(%v, %q) = %v, want %v", row.Codes, row.Label, got, want)
			}
		})
	}
}

func TestParkLevelSignOffCodes(t *testing.T) {
	codes := []string{
		"human-review:policy", "human-review:judge", "human-review:api-contract",
		"human-review:infra", "human-review:asks", "human-review:size",
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			if got := ParkLevel([]string{code}, ""); got != pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE {
				t.Fatalf("sign-off level = %v, want moderate", got)
			}
		})
	}
	if got := ParkLevel(codes, ""); got != pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE {
		t.Errorf("combined sign-off level = %v, want moderate", got)
	}
	for _, other := range []string{"partial-build", "future-code"} {
		mixed := append(append([]string(nil), codes...), other)
		if got := ParkLevel(mixed, ""); got != pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED {
			t.Errorf("mixed with %s level = %v, want unspecified", other, got)
		}
	}
	for code, info := range knownParkCodes {
		if !info.signOff {
			continue
		}
		if got := ParkLevel([]string{code}, ""); got != pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE {
			t.Errorf("registered sign-off %s level = %v, want moderate", code, got)
		}
	}
}

func TestLabelAsks(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  []string
	}{
		{"moderate: A, B, C", []string{"A", "B", "C"}},
		{"low: A, B, C +2", []string{"A", "B", "C"}},
		{"low: A, B, +2", []string{"A", "B"}},
		{"low: A, B…", []string{"A"}},
		{"Security", nil},
		{"moderate: A, B, C, D", []string{"A", "B", "C"}},
		{"moderate: 3,379 lines", []string{"3,379 lines"}},
		{"low: API contract, 12,004 lines +1", []string{"API contract", "12,004 lines"}},
	} {
		if got := labelAsks(tc.label); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("labelAsks(%q) = %v, want %v", tc.label, got, tc.want)
		}
	}
}

func TestProof_Sessions_AC208701_ProofPark(t *testing.T) {
	proof := &Proof{URL: "https://proof.test/page", Level: "high", Asks: []string{"a", "b", "c", "d"}, LoweredBy: []string{"Migration needs review"}}
	for _, tc := range []struct {
		name, code, label string
		proof             *Proof
		actions           []pb.AttentionAction
		level             pb.ConfidenceLevel
		asks              []string
	}{
		{"signoff", "human-review:database", "moderate: Migrations", proof, []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_PROOF, pb.AttentionAction_ATTENTION_ACTION_MERGE}, pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, []string{"Migrations"}},
		{"blocked", "partial-build", "low: A", proof, []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_PROOF}, pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, []string{"A"}},
		{"fallback", "human-review:database", "", proof, []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_PROOF, pb.AttentionAction_ATTENTION_ACTION_MERGE}, pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, []string{"a", "b", "c"}},
		{"video", "human-review:database", "", nil, []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_VIDEO, pb.AttentionAction_ATTENTION_ACTION_MERGE}, pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Compute(Input{HasPR: true, PRState: PRStateOpen, ParkCodes: []string{tc.code}, ParkLabel: tc.label, Proof: tc.proof, Evidence: &Evidence{URL: "https://video.test/clip"}})
			if !reflect.DeepEqual(got.Actions, tc.actions) || got.ConfidenceLevel != tc.level || !reflect.DeepEqual(got.Asks, tc.asks) || got.EvidenceUrl != "https://video.test/clip" {
				t.Fatalf("attention = %+v", got)
			}
			if tc.proof != nil && (got.ProofUrl != proof.URL || !reflect.DeepEqual(got.LoweredBy, proof.LoweredBy)) {
				t.Fatalf("proof = %+v", got)
			}
			if tc.proof == nil && got.ProofUrl != "" {
				t.Fatal(got)
			}
		})
	}
	yes := true
	for _, in := range []Input{{}, {HasPR: true, PRState: PRStateMerged}, {Fix: &FixFact{Sentence: "fault"}}, {QuestionText: "Question?", QuestionChatID: "chat"}, {HasPR: true, PRState: PRStateOpen, ChecksTotal: 1, ChecksDone: 1, Mergeable: &yes}} {
		in.Proof = proof
		got := Compute(in)
		if got.ConfidenceLevel != pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED || len(got.Asks) > 0 || got.ProofUrl != "" || len(got.LoweredBy) > 0 {
			t.Fatalf("non-park = %+v", got)
		}
	}
}

func TestProofLoweredByBoundedAndCloned(t *testing.T) {
	proof := &Proof{URL: "https://proof.test/page", LoweredBy: []string{"one", "two", "three", "four"}}
	got := Compute(Input{HasPR: true, PRState: PRStateOpen, ParkCodes: []string{"human-review:ui"}, Proof: proof})
	if !reflect.DeepEqual(got.LoweredBy, []string{"one", "two", "three"}) {
		t.Fatalf("lowered by = %v", got.LoweredBy)
	}
	got.LoweredBy[0] = "changed"
	if proof.LoweredBy[0] != "one" {
		t.Fatal("serving attention mutated the proof record")
	}
}
