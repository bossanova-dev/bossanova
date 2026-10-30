// Package evidence defines the machine-readable verify-evidence line: one HTML
// comment, inside a PR conversation comment, that names the head the evidence
// was recorded on, the evidence video and its length, and how many criteria
// were proven. Proof lines additionally pin confidence and a proof page.
//
//	<!-- boss-evidence: {"head":"<sha>","video":"<url>","videoMs":160000,"proven":5,"total":5} -->
//
// The contract is generic: any verify extension that posts evidence may write
// the line, and a reader never depends on a repo-local comment marker.
package evidence

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// Marker opens every evidence line. A reader looking for the evidence comment
// searches PR comments for it.
const Marker = "<!-- boss-evidence:"

// lineEnd closes an evidence line.
const lineEnd = "-->"

// Summary is one parsed evidence line.
type Summary struct {
	// Head is the commit SHA the evidence was recorded on.
	Head string
	// VideoURL is the evidence video; empty when none was published.
	VideoURL string
	// VideoDuration is the video's length; zero when unknown.
	VideoDuration time.Duration
	// Proven and Total count the criteria with a passing verdict and all
	// criteria.
	Proven int
	Total  int
}

// wire is the JSON payload. Pointers tell an absent count from a zero one.
type wire struct {
	Head    string `json:"head"`
	Video   string `json:"video,omitempty"`
	VideoMs int64  `json:"videoMs,omitempty"`
	Proven  *int   `json:"proven"`
	Total   *int   `json:"total"`
}

// Parse returns the last well-formed evidence line in body. A line is
// well-formed when its JSON decodes, names a head, carries both counts with
// 0 <= proven <= total, has a non-negative length, and any video is an
// absolute http(s) URL. A malformed line is skipped, so it can never hide an
// earlier valid one; ok is false when no line is well-formed.
func Parse(body string) (Summary, bool) {
	var found Summary
	ok := lastValid(body, Marker, parsePayload, &found)
	return found, ok
}

// lastValid scans complete marker payloads without letting malformed records
// hide an earlier valid one.
func lastValid[T any](body, marker string, parse func(string) (T, bool), found *T) bool {
	var ok bool
	rest := body
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			return ok
		}
		rest = rest[i+len(marker):]
		end := strings.Index(rest, lineEnd)
		if end < 0 {
			return ok
		}
		if value, valid := parse(rest[:end]); valid {
			*found, ok = value, true
		}
		rest = rest[end+len(lineEnd):]
	}
}

func parsePayload(payload string) (Summary, bool) {
	var w wire
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &w); err != nil {
		return Summary{}, false
	}
	head := strings.TrimSpace(w.Head)
	if head == "" || w.Proven == nil || w.Total == nil {
		return Summary{}, false
	}
	if *w.Proven < 0 || *w.Total < 0 || *w.Proven > *w.Total || w.VideoMs < 0 {
		return Summary{}, false
	}
	if w.Video != "" && !isHTTPURL(w.Video) {
		return Summary{}, false
	}
	return Summary{
		Head:          head,
		VideoURL:      w.Video,
		VideoDuration: time.Duration(w.VideoMs) * time.Millisecond,
		Proven:        *w.Proven,
		Total:         *w.Total,
	}, true
}

// Format renders s as an evidence line. HTML-significant characters in the
// JSON are escaped, so no value can close the comment early.
func Format(s Summary) string {
	proven, total := s.Proven, s.Total
	w := wire{
		Head:    s.Head,
		Video:   s.VideoURL,
		VideoMs: s.VideoDuration.Milliseconds(),
		Proven:  &proven,
		Total:   &total,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	// Encoding a struct of strings and ints cannot fail.
	_ = enc.Encode(w)
	return Marker + " " + strings.TrimSpace(buf.String()) + " " + lineEnd
}

// isHTTPURL reports whether raw is an absolute http(s) URL with a host and no
// whitespace.
func isHTTPURL(raw string) bool {
	if strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
