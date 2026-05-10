// Package main provides helpers for reading Claude Code project data.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxScanLines = 50
const maxSummaryLen = 80

// transcriptTailSize is the trailing byte window scanned for the most recent
// meaningful JSONL entry. 32 KB holds ~60-200 turns in a typical transcript.
const transcriptTailSize = 32 * 1024

// transcriptPath resolves ~/.claude/projects/<key>/<agentSessionID>.jsonl.
func transcriptPath(worktreePath, agentSessionID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects", pathToProjectKey(worktreePath), agentSessionID+".jsonl"), nil
}

// lastTurnIsUser reports whether the most recent meaningful entry in the
// Claude Code JSONL transcript is a real user text turn. Tool_result entries
// (logged as type:"user" but protocol plumbing) are skipped. Any error or
// an empty tail returns false — callers treat this as "don't suppress the
// question state", preserving pre-change behavior.
func lastTurnIsUser(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}

	var offset int64
	if info.Size() > transcriptTailSize {
		offset = info.Size() - transcriptTailSize
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}

	// When we seeked into the middle of the file the first line is almost
	// certainly partial — drop up to the first newline.
	if offset > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			return false
		}
		data = data[nl+1:]
	}

	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		switch entry.Type {
		case "assistant":
			return false
		case "user":
			if entry.Message.Role != "user" {
				continue
			}
			if hasUserTextBlock(entry.Message.Content) {
				return true
			}
			// Otherwise a tool_result-only user entry — keep walking.
		}
	}
	return false
}

// transcriptExists reports whether the Claude Code JSONL transcript for the
// given (worktreePath, agentSessionID) is present on disk and non-empty. Used
// by wake-up logic to decide between `claude --resume` (transcript present)
// and `claude --session-id` (transcript missing — fresh fallback). Errors are
// not surfaced — callers treat "can't tell" as "transcript missing", which is
// the safe default (fresh start over silently lying about a resume).
func transcriptExists(workDir, agentSessionID string) bool {
	path, err := transcriptPath(workDir, agentSessionID)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Size() > 0
}

// hasUserTextBlock reports whether the content of a user JSONL entry carries
// real text (string content, or an array with at least one {type:"text"}
// block). Array content composed solely of tool_result blocks returns false.
func hasUserTextBlock(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
			return true
		}
	case '[':
		var blocks []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &blocks); err == nil {
			for _, b := range blocks {
				if b.Type == "text" {
					return true
				}
			}
		}
	}
	return false
}

// chatTitle reads the JSONL file for the given Claude session and returns
// the first user message as a title. Returns "" if the file doesn't exist
// or no user message is found.
func chatTitle(worktreePath, claudeID string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	projectDir := filepath.Join(homeDir, ".claude", "projects", pathToProjectKey(worktreePath))
	return chatTitleInDir(projectDir, claudeID)
}

// chatTitleInDir reads the JSONL file for a Claude session from a specific
// directory. Used by chatTitle and useful for testing.
func chatTitleInDir(projectDir, claudeID string) string {
	path := filepath.Join(projectDir, claudeID+".jsonl")
	_, summary := parseSessionMeta(path)
	return summary
}

// pathToProjectKey converts a filesystem path to a Claude Code project key.
// Claude Code replaces both "/" and "." with "-".
// e.g. "/Users/dave/Code/.worktrees/foo" → "-Users-dave-Code--worktrees-foo"
func pathToProjectKey(path string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(path)
}

// jsonlLine is a minimal representation of a JSONL line for parsing.
type jsonlLine struct {
	Type    string   `json:"type"`
	Message jsonlMsg `json:"message"`
}

type jsonlMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseSessionMeta scans the first maxScanLines of a JSONL file to extract
// the slug and first user message text.
func parseSessionMeta(path string) (slug, summary string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for i := 0; i < maxScanLines && scanner.Scan(); i++ {
		var line jsonlLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}

		if line.Type == "user" && line.Message.Role == "user" && summary == "" {
			summary = extractText(line.Message.Content)
		}

		if summary != "" {
			break
		}
	}

	return "", summary
}

// extractText pulls the first text content from a user message.
// Content can be a string or an array of content blocks.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try as a plain string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return truncate(firstLine(stripXMLTags(s)))
	}

	// Try as array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				if t := truncate(firstLine(stripXMLTags(b.Text))); t != "" {
					return t
				}
			}
		}
	}

	return ""
}

var xmlTagRe = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

func stripXMLTags(s string) string {
	return strings.TrimSpace(xmlTagRe.ReplaceAllString(s, ""))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func truncate(s string) string {
	if len(s) <= maxSummaryLen {
		return s
	}
	return s[:maxSummaryLen-3] + "..."
}
