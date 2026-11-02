package ui

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// confidenceCases is the shared case table; the web mirrors it in
// services/web/src/features/sessions/confidence.test.ts.
var confidenceCases = []struct {
	name    string
	session *pb.Session
	ok      bool
	level   pb.ConfidenceLevel
	words   string
}{
	{"moderate with asks", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:database"}, ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, Asks: []string{"schema", "api"}, LoweredBy: []string{"touches migrations"}}}, true, pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, "moderate · schema, api"},
	{"low with only reasons", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:security"}, ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, LoweredBy: []string{"large diff", "Auth path"}}}, true, pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, "low · large diff, Auth path"},
	{"cannot assess with neither", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:unproven"}, ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS}}, true, pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS, "cannot assess"},
	{"high", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, Asks: []string{"ignored"}}}, true, pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, "high"},
	{"blocked park", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, ParkCodes: []string{"repair-exhausted", "ledger-open"}}}, true, pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED, "blocked · repair exhausted, open checklist items"},
	{"blocked park repairing", &pb.Session{DisplayIsRepairing: true, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, ParkCodes: []string{"ledger-open"}}}, true, pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED, "blocked → repairing"},
	{"review without a level", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:ui"}, Asks: []string{"schema"}}}, false, 0, ""},
	{"fix without park codes", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Log in again"}}, false, 0, ""},
	{"question", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_QUESTION}}, false, 0, ""},
	{"merge", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE}}, false, 0, ""},
	{"in progress", &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}}, false, 0, ""},
	{"no attention", &pb.Session{}, false, 0, ""},
}

func TestConfidenceCell_Sessions_AC208901(t *testing.T) {
	for _, tc := range confidenceCases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := ConfidenceCell(tc.session)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if c.Level != tc.level || c.Words() != tc.words {
				t.Errorf("cell = %v %q, want %v %q", c.Level, c.Words(), tc.level, tc.words)
			}
			if c.Text() != c.Icon+" "+tc.words {
				t.Errorf("text %q does not lead with the glyph", c.Text())
			}
		})
	}
}

// TestConfidenceGlyphsAreDistinct_Sessions_AC208901: every level has a glyph
// and a word of its own, so the level never rests on colour alone.
func TestConfidenceGlyphsAreDistinct_Sessions_AC208901(t *testing.T) {
	glyphs, words := map[string]pb.ConfidenceLevel{}, map[string]pb.ConfidenceLevel{}
	for _, level := range ConfidenceLevels() {
		c, ok := ConfidenceCell(&pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ConfidenceLevel: level}})
		if !ok || c.Icon == "" || c.Word == "" {
			t.Fatalf("%v has no glyph or word: %+v", level, c)
		}
		if prev, dup := glyphs[c.Icon]; dup {
			t.Errorf("%v and %v share glyph %q", prev, level, c.Icon)
		}
		if prev, dup := words[c.Word]; dup {
			t.Errorf("%v and %v share word %q", prev, level, c.Word)
		}
		glyphs[c.Icon], words[c.Word] = level, level
	}
	if len(ConfidenceLevels()) != len(pb.ConfidenceLevel_name)-1 {
		t.Errorf("ConfidenceLevels lists %d levels, the enum has %d", len(ConfidenceLevels()), len(pb.ConfidenceLevel_name)-1)
	}
}
