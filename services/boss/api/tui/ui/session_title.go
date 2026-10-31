package ui

import (
	"strings"
	"unicode"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const sessionTitleLimit = 40

// ShortSessionTitle removes the first tracker token and limits the result to
// 40 runes, including an ellipsis when shortening at a word boundary.
func ShortSessionTitle(title, trackerID string) string {
	if trackerID != "" {
		title = strings.Replace(title, "["+trackerID+"]", "", 1)
	}
	title = strings.Join(strings.Fields(title), " ")
	runes := []rune(title)
	if len(runes) <= sessionTitleLimit {
		return title
	}
	window := runes[:sessionTitleLimit-1]
	for i := len(window) - 1; i >= 0; i-- {
		if unicode.IsSpace(window[i]) {
			window = window[:i]
			break
		}
	}
	return strings.TrimRightFunc(string(window), unicode.IsSpace) + "…"
}

// SessionListTitle composes a tracker's ID and a shortened display title for a row.
func SessionListTitle(sess *pb.Session) string {
	title := sess.GetTitle()
	if title == "" {
		title = sess.GetBranchName()
	}
	id := sess.GetTrackerId()
	short := ShortSessionTitle(title, id)
	if id == "" {
		return short
	}
	if short == "" {
		return id
	}
	return id + " · " + short
}

// trackerTitleToken prefers bracketed tokens for full titles, and also accepts
// the ID at the beginning of a composed session-list title.
func trackerTitleToken(title, trackerID string) (int, string) {
	target := "[" + trackerID + "]"
	if idx := strings.Index(title, target); idx >= 0 {
		return idx, target
	}
	if title == trackerID || strings.HasPrefix(title, trackerID+" · ") {
		return 0, trackerID
	}
	return -1, ""
}
