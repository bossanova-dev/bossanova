package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// resultSummaries reads each tool's result record, the line's toolUseResult,
// into the few words a step's result column shows. A tool missing here, or a
// record that does not state the fact, gets no summary.
var resultSummaries = map[string]func(record json.RawMessage, block contentBlock) string{
	"Bash":      bashResult,
	"Read":      readResult,
	"Edit":      editResult,
	"MultiEdit": editResult,
	"Write":     writeResult,
	"Grep":      grepResult,
	"Glob":      globResult,
}

// resultSummary is the step result for one tool's result. A failed call keeps
// only what its failure states, so a rejected edit never reports its patch.
func resultSummary(tool string, record json.RawMessage, block contentBlock) string {
	summarize, ok := resultSummaries[tool]
	if !ok {
		return ""
	}
	return summarize(record, block)
}

// soleResultRecord is a line's toolUseResult when the line carries exactly one
// tool result. The record names no call, so it belongs to none of several.
func soleResultRecord(record json.RawMessage, blocks []contentBlock) json.RawMessage {
	n := 0
	for _, b := range blocks {
		if b.Type == "tool_result" {
			n++
		}
	}
	if n != 1 {
		return nil
	}
	return record
}

// exitLine is the first line Claude Code writes into a failed command's
// result.
var exitLine = regexp.MustCompile(`^Exit code (\d+)$`)

func bashResult(record json.RawMessage, block contentBlock) string {
	if block.IsError {
		first, _, _ := strings.Cut(resultText(block.Content), "\n")
		if m := exitLine.FindStringSubmatch(first); m != nil {
			return "exit " + m[1]
		}
		return ""
	}
	var r struct {
		Interrupted bool `json:"interrupted"`
	}
	if json.Unmarshal(record, &r) == nil && r.Interrupted {
		return "interrupted"
	}
	return ""
}

func readResult(record json.RawMessage, block contentBlock) string {
	var r struct {
		Type string `json:"type"`
		File struct {
			NumLines *int `json:"numLines"`
		} `json:"file"`
	}
	if block.IsError || json.Unmarshal(record, &r) != nil || r.Type != "text" || r.File.NumLines == nil {
		return ""
	}
	return counted(*r.File.NumLines, "line", "lines")
}

// patchHunk is one hunk of a structuredPatch: its lines carry a leading
// "+", "-" or " ".
type patchHunk struct {
	Lines []string `json:"lines"`
}

func editResult(record json.RawMessage, block contentBlock) string {
	var r struct {
		StructuredPatch []patchHunk `json:"structuredPatch"`
	}
	if block.IsError || json.Unmarshal(record, &r) != nil {
		return ""
	}
	return patchSize(r.StructuredPatch)
}

func writeResult(record json.RawMessage, block contentBlock) string {
	var r struct {
		Type            string      `json:"type"`
		Content         string      `json:"content"`
		StructuredPatch []patchHunk `json:"structuredPatch"`
	}
	if block.IsError || json.Unmarshal(record, &r) != nil {
		return ""
	}
	switch r.Type {
	case "create":
		return lineChanges(lineCount(r.Content), 0)
	case "update":
		return patchSize(r.StructuredPatch)
	}
	return ""
}

func grepResult(record json.RawMessage, block contentBlock) string {
	var r struct {
		Mode         string `json:"mode"`
		NumFiles     *int   `json:"numFiles"`
		NumLines     *int   `json:"numLines"`
		NumMatches   *int   `json:"numMatches"`
		AppliedLimit int    `json:"appliedLimit"`
	}
	if block.IsError || json.Unmarshal(record, &r) != nil {
		return ""
	}
	var n *int
	var one, many string
	switch r.Mode {
	case "files_with_matches":
		n, one, many = r.NumFiles, "file", "files"
	case "content":
		n, one, many = r.NumLines, "line", "lines"
	case "count":
		n, one, many = r.NumMatches, "match", "matches"
	}
	if n == nil {
		return ""
	}
	if r.AppliedLimit > 0 {
		return grouped(*n) + "+ " + many
	}
	return counted(*n, one, many)
}

func globResult(record json.RawMessage, block contentBlock) string {
	var r struct {
		NumFiles        *int `json:"numFiles"`
		Truncated       bool `json:"truncated"`
		TotalMatches    *int `json:"totalMatches"`
		CountIsComplete bool `json:"countIsComplete"`
	}
	if block.IsError || json.Unmarshal(record, &r) != nil || r.NumFiles == nil {
		return ""
	}
	switch {
	case !r.Truncated:
		return counted(*r.NumFiles, "file", "files")
	case r.TotalMatches != nil && r.CountIsComplete:
		return counted(*r.TotalMatches, "file", "files")
	}
	return grouped(*r.NumFiles) + "+ files"
}

// resultText is a tool_result's content, which is a string or a list of text
// blocks.
func resultText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []contentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func patchSize(hunks []patchHunk) string {
	var added, deleted int
	for _, h := range hunks {
		for _, l := range h.Lines {
			switch {
			case strings.HasPrefix(l, "+"):
				added++
			case strings.HasPrefix(l, "-"):
				deleted++
			}
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

// lineChanges writes a change's size as the app's size chips do: "+12 −3",
// with a U+2212 minus, leaving out a side that is zero.
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

func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return grouped(n) + " " + many
}

// grouped writes a count with comma thousands separators, as the web app's
// en-US numbers read.
func grouped(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
