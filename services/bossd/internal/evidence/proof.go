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

// proofNoteMarker matches the core proof note's last line, mirroring
// PROOF_NOTE_MARKER in skills-toolbox/verify/verify-proof-publish.mjs.
var proofNoteMarker = regexp.MustCompile(`^<!-- boss-proof-note head=[0-9a-fA-F]{40} digest=[0-9a-f]{16} -->$`)

// maxSummaryUnits is the writer's MAX_COMMENT_BLOCK_CHARS, in UTF-16 code
// units like JavaScript string.length.
const maxSummaryUnits = 40_000

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
	Head      string
	Page      string
	Level     string
	Asks      []string
	LoweredBy []string
	Digest    string
	// Summary is the Markdown block of a core proof note; empty for any
	// other comment shape and for a block over maxSummaryUnits.
	Summary string
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
	Head      string
	Page      string
	Level     string
	Asks      []string
	LoweredBy []string
	Digest    string
	Summary   string
	// PageProblem is why verify published no page for Head, from its
	// proof-missing note; zero when the record has a page or none was noted.
	PageProblem PageProblem
	Media       []Media
	Legacy      bool
	RecordedAt  time.Time
}

func (p *ProofRecord) equal(other *ProofRecord) bool {
	if p == nil || other == nil {
		return p == other
	}
	return p.Head == other.Head && p.Page == other.Page && p.Level == other.Level &&
		p.Digest == other.Digest && p.Summary == other.Summary && p.PageProblem == other.PageProblem && p.Legacy == other.Legacy && p.RecordedAt.Equal(other.RecordedAt) &&
		slices.Equal(p.LoweredBy, other.LoweredBy) && slices.Equal(p.Asks, other.Asks) && slices.Equal(p.Media, other.Media)
}

func (p ProofRecord) clone() ProofRecord {
	p.Asks = slices.Clone(p.Asks)
	p.LoweredBy = slices.Clone(p.LoweredBy)
	p.Media = slices.Clone(p.Media)
	return p
}

var (
	proofHead   = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
	proofDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	proofAsk    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

type proofWire struct {
	LoweredBy json.RawMessage `json:"loweredBy"`
	Schema    string          `json:"schema"`
	Head      string          `json:"head"`
	Page      json.RawMessage `json:"page"`
	Level     string          `json:"level"`
	Asks      []string        `json:"asks"`
	Digest    string          `json:"digest"`
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
	// Reasons are optional, additive metadata. A malformed field or element
	// must not discard an otherwise valid head-pinned proof record.
	var rawReasons []json.RawMessage
	var loweredBy []string
	if json.Unmarshal(w.LoweredBy, &rawReasons) == nil {
		for _, raw := range rawReasons {
			var reason string
			if json.Unmarshal(raw, &reason) == nil && strings.TrimSpace(reason) != "" {
				loweredBy = append(loweredBy, reason)
			}
		}
	}
	return ProofLine{LoweredBy: loweredBy, Head: head, Page: page, Level: w.Level, Asks: w.Asks, Digest: w.Digest}, true
}

// isHTTPSURL matches the proof writer's URL contract. Its character cap uses
// UTF-16 code units, matching JavaScript string.length.
func isHTTPSURL(raw string) bool {
	if utf16Len(raw) > 2048 || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' }) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

// utf16Len counts s in UTF-16 code units, as JavaScript string.length does.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// proofNoteSummary returns the Markdown block of a core proof note: the text
// before its last proof line, trimmed. A body whose last non-empty line is not
// the note marker, such as a repository evidence comment, has none, and a
// block longer than the writer's cap is dropped.
func proofNoteSummary(body string) string {
	lines := strings.Split(body, "\n")
	last := ""
	for i := len(lines) - 1; i >= 0 && last == ""; i-- {
		last = strings.TrimSpace(lines[i])
	}
	at := strings.LastIndex(body, ProofMarker)
	if !proofNoteMarker.MatchString(last) || at < 0 {
		return ""
	}
	summary := strings.TrimSpace(body[:at])
	if utf16Len(summary) > maxSummaryUnits {
		return ""
	}
	return summary
}

func buildRecord(proof *ProofLine, proofAt time.Time, legacy *Entry) *ProofRecord {
	if proof == nil && legacy == nil {
		return nil
	}
	record := &ProofRecord{}
	if proof != nil {
		record.Head, record.Page, record.Level = proof.Head, proof.Page, proof.Level
		record.LoweredBy = slices.Clone(proof.LoweredBy)
		record.Asks, record.Digest, record.RecordedAt = slices.Clone(proof.Asks), proof.Digest, proofAt
		record.Summary = proof.Summary
	} else {
		record.Head, record.Legacy, record.RecordedAt = legacy.Summary.Head, true, legacy.RecordedAt
	}
	if legacy != nil && legacy.Summary.VideoURL != "" {
		record.Media = []Media{{Kind: "video", URL: legacy.Summary.VideoURL, Duration: legacy.Summary.VideoDuration}}
	}
	return record
}
