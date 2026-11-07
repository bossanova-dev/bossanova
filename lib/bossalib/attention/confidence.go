package attention

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
// to assess, then a park composed entirely of sign-off codes.
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
		if !knownParkCodes[code].signOff {
			return pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
		}
	}
	return pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE
}

var levelWords = map[pb.ConfidenceLevel]string{
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH:          "High",
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE:      "Moderate",
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW:           "Low",
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED:       "Blocked",
	pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS: "Cannot assess",
}

// ConfidenceSentence states a verify park's level in words, then what lowered
// it, e.g. "Moderate: changes Infrastructure. Reviewing this area would raise
// this." The reasons keep their served order, joined by a space, blank ones dropped; the first one
// continues the sentence, so its first letter is lower-cased unless it opens
// an acronym. A level without reasons reads "Moderate."; no level reads "".
// The web's confidenceSentence mirrors it through
// testdata/confidence_sentences.json.
func ConfidenceSentence(level pb.ConfidenceLevel, loweredBy []string) string {
	word, ok := levelWords[level]
	if !ok {
		return ""
	}
	var reasons []string
	for _, reason := range loweredBy {
		if reason = strings.TrimSpace(reason); reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) == 0 {
		return word + "."
	}
	return word + ": " + continueSentence(strings.Join(reasons, " "))
}

// continueSentence lower-cases a sentence's first letter so it can follow a
// colon, leaving an acronym ("CI failed") as written.
func continueSentence(s string) string {
	first, size := utf8.DecodeRuneInString(s)
	if next, _ := utf8.DecodeRuneInString(s[size:]); unicode.IsUpper(next) {
		return s
	}
	return string(unicode.ToLower(first)) + s[size:]
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

// ProofConfidenceLevel maps a boss-proof/1 level; legacy or unknown is unrated.
func ProofConfidenceLevel(level string) pb.ConfidenceLevel {
	switch level {
	case "high":
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH
	case "moderate":
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE
	case "low":
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW
	case "blocked":
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED
	case "cannot-assess":
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS
	default:
		return pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
	}
}
