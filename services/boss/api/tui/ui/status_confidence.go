package ui

import (
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ConfidenceCell names the confidence and the asks or reasons behind it.
func ConfidenceCell(level pb.ConfidenceLevel, asks, reasons []string) string {
	label := "? Unrated"
	switch level {
	case pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED:
	case pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH:
		label = "● High"
	case pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE:
		label = "◐ Moderate"
	case pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW:
		label = "○ Low"
	case pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED:
		label = "! Blocked"
	case pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS:
		label = "? Cannot assess"
	}
	detail := asks
	if len(detail) == 0 {
		detail = reasons
	}
	if len(detail) > 0 {
		label += " · " + strings.Join(detail, ", ")
	}
	return label
}
