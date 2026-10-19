package evidence

import (
	"encoding/json"
	"regexp"
	"strings"
)

// OpenItemsMarker opens the machine-readable open-items line a verify park
// note carries when the PR's checklist is open: the head it was judged on and
// what keeps the checklist open, one plain line each.
//
//	<!-- boss-open-items: {"head":"<sha>","items":["Open human follow-up: watch the rollout"]} -->
const OpenItemsMarker = "<!-- boss-open-items:"

// CommentMarker is the prefix every machine-readable boss line shares, so one
// comment read can find both the evidence line and the open-items line.
const CommentMarker = "<!-- boss-"

// MaxOpenItems caps how many open items a reader keeps from one line.
const MaxOpenItems = 20

// OpenItems is one parsed open-items line.
type OpenItems struct {
	Head  string
	Items []string
}

type openItemsWire struct {
	Head  string   `json:"head"`
	Items []string `json:"items"`
}

// ParseOpenItems returns the last well-formed open-items line in body: its
// JSON decodes, names a head, and lists at least one non-blank item. Blank
// items are dropped, whitespace is collapsed, and at most MaxOpenItems are
// kept.
func ParseOpenItems(body string) (OpenItems, bool) {
	var (
		found OpenItems
		ok    bool
	)
	rest := body
	for {
		i := strings.Index(rest, OpenItemsMarker)
		if i < 0 {
			return found, ok
		}
		rest = rest[i+len(OpenItemsMarker):]
		end := strings.Index(rest, lineEnd)
		if end < 0 {
			return found, ok
		}
		if items, valid := parseOpenItemsPayload(rest[:end]); valid {
			found, ok = items, true
		}
		rest = rest[end+len(lineEnd):]
	}
}

func parseOpenItemsPayload(payload string) (OpenItems, bool) {
	var w openItemsWire
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &w); err != nil {
		return OpenItems{}, false
	}
	head := strings.TrimSpace(w.Head)
	var items []string
	for _, item := range w.Items {
		if flat := strings.Join(strings.Fields(item), " "); flat != "" && len(items) < MaxOpenItems {
			items = append(items, PlainOpenItem(flat))
		}
	}
	if head == "" || len(items) == 0 {
		return OpenItems{}, false
	}
	return OpenItems{Head: head, Items: items}, true
}

var (
	// headingSectionItem is an open item a park note recorded for a checklist
	// section it named as a markdown heading, such as
	// "## Acceptance criteria is missing.". The name leaves out a trailing
	// "section", and may be empty.
	headingSectionItem = regexp.MustCompile("^`?#{1,6}(?:[ \t]+(.+?)(?i:[ \t]+section)?)?`?[ \t]+(is missing\\.|appears more than once\\b.*|is not a checklist\\b.*)$")
	// leadingHeading is any other leading markdown heading, with the
	// backticks that may surround it.
	leadingHeading = regexp.MustCompile("^(?:`#{1,6}[ \t]+([^`]*)`|#{1,6}[ \t]+)")
)

// PlainOpenItem says an open item in plain words, so no client shows the
// operator a markdown heading: a checklist section a park note named as one
// reads "The Acceptance criteria section is missing.", and any other leading
// heading marker is dropped. Every other item is returned unchanged.
func PlainOpenItem(item string) string {
	if m := headingSectionItem.FindStringSubmatch(item); m != nil {
		problem := m[2]
		switch {
		case strings.HasPrefix(problem, "appears more than once"):
			problem = "appears more than once, so it cannot be read."
		case strings.HasPrefix(problem, "is not a checklist"):
			problem = "is not a checklist."
		}
		if m[1] == "" {
			return "A section with no name " + problem
		}
		return "The " + m[1] + " section " + problem
	}
	return leadingHeading.ReplaceAllString(item, "$1")
}
