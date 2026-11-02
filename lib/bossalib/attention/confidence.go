package attention

import (
	"slices"
	"strconv"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Proof is the current head's proof page and the review requests it records.
// The park, rather than the proof writer, is authoritative for the served level.
type Proof struct {
	URL       string
	Level     string
	Asks      []string
	LoweredBy []string
}

// ParkLevel mirrors verify's parkLevel: an explicit label wins, then inability
// to assess, then a park composed entirely of path sign-off codes.
func ParkLevel(codes []string, label string) pb.ConfidenceLevel {
	switch {
	case strings.HasPrefix(label, "moderate:"):
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE
	case strings.HasPrefix(label, "low:"):
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW
	}
	if slices.Contains(codes, "cannot-assess") {
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS
	}
	if len(codes) == 0 {
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
	}
	for _, code := range codes {
		switch code {
		case "always-human-path", "human-review:database", "human-review:security", "human-review:ui", "human-review:unproven":
		default:
			return pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
		}
	}
	return pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE
}

// labelAsks keeps complete names from the bounded verify status description.
func labelAsks(label string) []string {
	if !strings.HasPrefix(label, "moderate:") && !strings.HasPrefix(label, "low:") {
		return nil
	}
	_, names, _ := strings.Cut(label, ":")
	var asks []string
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		// The writer appends +N to the final ask, without an extra comma.
		if before, suffix, ok := strings.Cut(name, " +"); ok {
			if _, err := strconv.Atoi(suffix); err == nil {
				name = strings.TrimSpace(before)
			}
		}
		if strings.HasPrefix(name, "+") {
			if _, err := strconv.Atoi(name[1:]); err == nil {
				continue
			}
		}
		if name != "" && !strings.HasSuffix(name, "…") {
			asks = append(asks, name)
		}
		if len(asks) == 3 {
			break
		}
	}
	return asks
}
