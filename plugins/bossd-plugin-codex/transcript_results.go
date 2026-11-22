package main

import (
	"strconv"
	"strings"
)

// fileChange is one file's entry in a FileChange item: an added file carries
// its content, an updated one its unified diff.
type fileChange struct {
	Type        string `json:"type"`
	Content     string `json:"content"`
	UnifiedDiff string `json:"unified_diff"`
}

// size is the change's lines as the app's size chips write them, from what
// the rollout records: an added file's content, an update's diff. A delete
// records neither, so it has no size.
func (c fileChange) size() string {
	switch c.Type {
	case "add":
		return lineChanges(lineCount(c.Content), 0)
	case "update":
		return diffSize(c.UnifiedDiff)
	}
	return ""
}

// exitResult is a command's stated non-zero exit status. Codex records -1
// when no process status exists, which states no exit.
func exitResult(code *int) string {
	if code == nil || *code <= 0 {
		return ""
	}
	return "exit " + strconv.Itoa(*code)
}

// diffSize counts the added and removed lines inside a unified diff's hunks;
// anything before the first hunk header is a file header.
func diffSize(diff string) string {
	var added, deleted int
	inHunk := false
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk:
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			deleted++
		}
	}
	return lineChanges(added, deleted)
}

func lineCount(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// lineChanges writes "+12 −3", with a U+2212 minus, leaving out a zero side.
func lineChanges(added, deleted int) string {
	var parts []string
	if added > 0 {
		parts = append(parts, "+"+grouped(added))
	}
	if deleted > 0 {
		parts = append(parts, "−"+grouped(deleted))
	}
	return strings.Join(parts, " ")
}

// grouped writes a count with comma thousands separators.
func grouped(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
