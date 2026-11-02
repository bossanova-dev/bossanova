package ui

import (
	"strings"

	"github.com/recurser/bossalib/attention"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Confidence is a verify park's confidence as a status cell reads it:
// Icon + " " + Word + Sep + Detail, in Intent's colour. Each level has its own
// glyph and word, so the level never rests on colour alone. The web mirrors it
// in services/web/src/features/sessions/confidence.ts.
type Confidence struct {
	Level  pb.ConfidenceLevel
	Icon   string
	Word   string
	Sep    string
	Detail string
	Intent pb.DisplayIntent
}

// Text is the cell's resting text, with its glyph.
func (c Confidence) Text() string {
	return c.Icon + " " + c.Words()
}

// Words is the cell without its glyph, which a working chat's spinner replaces.
func (c Confidence) Words() string {
	return c.Word + c.Sep + c.Detail
}

type confidenceLook struct {
	icon, word string
	intent     pb.DisplayIntent
}

var confidenceLooks = map[pb.ConfidenceLevel]confidenceLook{
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH:          {"✓", "high", pb.DisplayIntent_DISPLAY_INTENT_SUCCESS},
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE:      {"◐", "moderate", pb.DisplayIntent_DISPLAY_INTENT_WARNING},
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW:           {"▽", "low", pb.DisplayIntent_DISPLAY_INTENT_WARNING},
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED:       {"⨯", "blocked", pb.DisplayIntent_DISPLAY_INTENT_DANGER},
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS: {"◌", "cannot assess", pb.DisplayIntent_DISPLAY_INTENT_MUTED},
}

// ConfidenceLevels lists every level a cell can show.
func ConfidenceLevels() []pb.ConfidenceLevel {
	return []pb.ConfidenceLevel{
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED,
		pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS,
	}
}

// ConfidenceCell is the confidence cell of a verify park's row: an attention
// with a served level, or a blocked park (a FIX with park codes), which an
// older daemon serves without one and reads as BLOCKED. Every other row has
// none (ok=false) and renders as before. Asks and reasons show as served.
func ConfidenceCell(s *pb.Session) (Confidence, bool) {
	a := s.GetAttention()
	level := a.GetConfidenceLevel()
	if level == pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED && a.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX && len(a.GetParkCodes()) > 0 {
		level = pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED
	}
	look, ok := confidenceLooks[level]
	if !ok {
		return Confidence{}, false
	}
	c := Confidence{Level: level, Icon: look.icon, Word: look.word, Intent: look.intent}
	var detail []string
	switch level {
	case pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED:
		if s.GetDisplayIsRepairing() {
			c.Sep, c.Detail = " → ", "repairing"
			return c, true
		}
		detail = attention.ParkCategories(a.GetParkCodes())
	case pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH:
	default:
		detail = a.GetAsks()
		if len(detail) == 0 {
			detail = a.GetLoweredBy()
		}
	}
	if len(detail) > 0 {
		c.Sep, c.Detail = " · ", strings.Join(detail, ", ")
	}
	return c, true
}
