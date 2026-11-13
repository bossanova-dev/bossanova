package evidence

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ParkAsksMarker opens the head-bound full ask names in a verify park note.
const ParkAsksMarker = "<!-- boss-park-asks:"

// MaxParkAsks bounds accepted payloads; larger lists are rejected whole.
const MaxParkAsks = 20

// ParkAsks is a verify park note's ordered operator-facing ask names.
type ParkAsks struct {
	Head string   `json:"head"`
	Asks []string `json:"asks"`
}

// ParseParkAsks returns the last valid payload naming a head and at least one
// non-blank ask. Oversized lists are rejected so consumers never mistake a
// partial list for all asks. Whitespace is collapsed; names over 300 runes are dropped.
func ParseParkAsks(body string) (ParkAsks, bool) {
	var found ParkAsks
	ok := lastValid(body, ParkAsksMarker, parseParkAsksPayload, &found)
	return found, ok
}

func parseParkAsksPayload(payload string) (ParkAsks, bool) {
	var wire ParkAsks
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &wire); err != nil {
		return ParkAsks{}, false
	}
	if len(wire.Asks) > MaxParkAsks {
		return ParkAsks{}, false
	}
	var asks []string
	for _, ask := range wire.Asks {
		flat := strings.Join(strings.Fields(ask), " ")
		if flat != "" && utf8.RuneCountInString(flat) <= 300 {
			asks = append(asks, flat)
		}
	}
	head := strings.TrimSpace(wire.Head)
	if head == "" || len(asks) == 0 {
		return ParkAsks{}, false
	}
	return ParkAsks{Head: head, Asks: asks}, true
}
