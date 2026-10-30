package evidence

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
)

// ProofMarker opens a machine-readable proof line.
const ProofMarker = "<!-- boss-proof:"

// ProofSchema is the supported proof marker version.
const ProofSchema = "boss-proof/1"

// Proof confidence levels, shared with the verify writer.
const (
	LevelHigh         = "high"
	LevelModerate     = "moderate"
	LevelLow          = "low"
	LevelBlocked      = "blocked"
	LevelCannotAssess = "cannot-assess"
)

// ProofLine is the confidence and page pin recorded for a PR head.
type ProofLine struct {
	Head   string
	Page   string
	Level  string
	Asks   []string
	Digest string
}

// Media is a legacy evidence video attached to a proof record.
type Media struct {
	Kind     string
	URL      string
	Duration time.Duration
}

// ProofRecord combines proof confidence with the same head's legacy media.
// Legacy records have no proof page, level, asks or digest.
type ProofRecord struct {
	Head       string
	Page       string
	Level      string
	Asks       []string
	Digest     string
	Media      []Media
	Legacy     bool
	RecordedAt time.Time
}

func (p *ProofRecord) equal(other *ProofRecord) bool {
	if p == nil || other == nil {
		return p == other
	}
	return p.Head == other.Head && p.Page == other.Page && p.Level == other.Level &&
		p.Digest == other.Digest && p.Legacy == other.Legacy && p.RecordedAt.Equal(other.RecordedAt) &&
		slices.Equal(p.Asks, other.Asks) && slices.Equal(p.Media, other.Media)
}

func (p ProofRecord) clone() ProofRecord {
	p.Asks = slices.Clone(p.Asks)
	p.Media = slices.Clone(p.Media)
	return p
}

var (
	proofHead   = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
	proofDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	proofAsk    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

type proofWire struct {
	Schema string          `json:"schema"`
	Head   string          `json:"head"`
	Page   json.RawMessage `json:"page"`
	Level  string          `json:"level"`
	Asks   []string        `json:"asks"`
	Digest string          `json:"digest"`
}

// ParseProof returns the last well-formed boss-proof/1 line. Unknown keys are
// ignored for additive schema changes; malformed or future lines are skipped.
func ParseProof(body string) (ProofLine, bool) {
	var found ProofLine
	ok := lastValid(body, ProofMarker, parseProofPayload, &found)
	return found, ok
}

func parseProofPayload(payload string) (ProofLine, bool) {
	var w proofWire
	if err := json.Unmarshal([]byte(payload), &w); err != nil {
		return ProofLine{}, false
	}
	head := strings.TrimSpace(w.Head)
	if w.Schema != ProofSchema || !proofHead.MatchString(head) || !proofDigest.MatchString(w.Digest) ||
		!slices.Contains([]string{LevelHigh, LevelModerate, LevelLow, LevelBlocked, LevelCannotAssess}, w.Level) ||
		w.Asks == nil || len(w.Asks) > 32 || len(w.Page) == 0 {
		return ProofLine{}, false
	}
	var page string
	if string(w.Page) != "null" {
		if err := json.Unmarshal(w.Page, &page); err != nil || !isHTTPSURL(page) {
			return ProofLine{}, false
		}
	}
	for _, ask := range w.Asks {
		// The writer's asks are area identifiers, not free-form text.
		if len(ask) > 300 || !proofAsk.MatchString(ask) {
			return ProofLine{}, false
		}
	}
	return ProofLine{Head: head, Page: page, Level: w.Level, Asks: w.Asks, Digest: w.Digest}, true
}

// isHTTPSURL matches the proof writer's URL contract. Its character cap uses
// UTF-16 code units, matching JavaScript string.length.
func isHTTPSURL(raw string) bool {
	if len(utf16.Encode([]rune(raw))) > 2048 || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' }) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func buildRecord(proof *ProofLine, proofAt time.Time, legacy *Entry) *ProofRecord {
	if proof == nil && legacy == nil {
		return nil
	}
	record := &ProofRecord{}
	if proof != nil {
		record.Head, record.Page, record.Level = proof.Head, proof.Page, proof.Level
		record.Asks, record.Digest, record.RecordedAt = slices.Clone(proof.Asks), proof.Digest, proofAt
	} else {
		record.Head, record.Legacy, record.RecordedAt = legacy.Summary.Head, true, legacy.RecordedAt
	}
	if legacy != nil && legacy.Summary.VideoURL != "" {
		record.Media = []Media{{Kind: "video", URL: legacy.Summary.VideoURL, Duration: legacy.Summary.VideoDuration}}
	}
	return record
}
