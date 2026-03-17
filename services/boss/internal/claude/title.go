// Package claude provides helpers for reading Claude Code project data.
package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const maxScanLines = 50
const maxSummaryLen = 80

// ChatTitle reads the JSONL file for the given Claude session and returns
// the first user message as a title. Returns "" if the file doesn't exist
// or no user message is found.
func ChatTitle(worktreePath, claudeID string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	projectDir := filepath.Join(homeDir, ".claude", "projects", PathToProjectKey(worktreePath))
	return chatTitleInDir(projectDir, claudeID)
}

// chatTitleInDir reads the JSONL file for a Claude session from a specific
// directory. Used by ChatTitle and useful for testing.
func chatTitleInDir(projectDir, claudeID string) string {
	path := filepath.Join(projectDir, claudeID+".jsonl")
	_, summary := parseSessionMeta(path)
	return summary
}

// PathToProjectKey converts a filesystem path to a Claude Code project key.
// Claude Code replaces both "/" and "." with "-".
// e.g. "/Users/dave/Code/.worktrees/foo" → "-Users-dave-Code--worktrees-foo"
func PathToProjectKey(path string) string {
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
		return truncate(firstLine(s))
	}

	// Try as array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return truncate(firstLine(b.Text))
			}
		}
	}

	return ""
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
