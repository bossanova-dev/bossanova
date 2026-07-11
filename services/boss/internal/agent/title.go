// Package claude provides helpers for reading Claude Code project data.
package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxScanLines = 50
const maxSummaryLen = 80

// ChatTitle reads the JSONL file for the given Claude session and returns
// the first user message as a title. Returns "" if the file doesn't exist
// or no user message is found.
func ChatTitle(worktreePath, agentSessionID string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	projectDir := filepath.Join(homeDir, ".claude", "projects", PathToProjectKey(worktreePath))
	return chatTitleInDir(projectDir, agentSessionID)
}

// chatTitleInDir reads the JSONL file for a Claude session from a specific
// directory. Used by ChatTitle and useful for testing.
func chatTitleInDir(projectDir, agentSessionID string) string {
	path := filepath.Join(projectDir, agentSessionID+".jsonl")
	_, summary := parseSessionMeta(path)
	return summary
}

// TranscriptAbsentOrEmpty reports whether a chat's Claude transcript is safe to
// treat as a never-used orphan — i.e. whether auto-deleting the chat row would
// destroy no history. It returns true ONLY when the transcript path resolves
// and the file is affirmatively absent or a zero-length regular file.
//
// Any ambiguity returns false so a transient failure can never trigger a
// destructive delete: an unresolvable home dir, a stat error other than
// not-exist (permissions, an unreadable parent dir), a directory at the path,
// or a non-empty file all mean "keep the chat". This is deliberately stricter
// than "ChatTitle returned empty": a mis-encoded project key (e.g. a stored
// path with a trailing slash) makes a rich transcript look title-less, and
// keying deletion off that lost real chats.
func TranscriptAbsentOrEmpty(worktreePath, agentSessionID string) bool {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	projectDir := filepath.Join(homeDir, ".claude", "projects", PathToProjectKey(worktreePath))
	return transcriptAbsentOrEmptyInDir(projectDir, agentSessionID)
}

// transcriptAbsentOrEmptyInDir is the testable core of TranscriptAbsentOrEmpty.
func transcriptAbsentOrEmptyInDir(projectDir, agentSessionID string) bool {
	path := filepath.Join(projectDir, agentSessionID+".jsonl")
	info, err := os.Stat(path)
	if err != nil {
		// The only safe-to-delete failure is a confirmed absent file.
		return errors.Is(err, fs.ErrNotExist)
	}
	return !info.IsDir() && info.Size() == 0
}

// PathToProjectKey converts a filesystem path to a Claude Code project key.
// Claude Code replaces path separators and "." with "-".
// e.g. "/Users/dave/Code/.worktrees/foo" → "-Users-dave-Code--worktrees-foo"
func PathToProjectKey(path string) string {
	// Claude Code derives the key from the process working directory (getcwd),
	// which is always normalized, so Clean first. Without this a stored path
	// with a trailing slash (e.g. a repo registered as ".../bossanova/")
	// encodes to "...-bossanova-" and never matches Claude's "...-bossanova"
	// key, silently defeating --resume and forcing a --session-id fresh start.
	return strings.NewReplacer("/", "-", "\\", "-", ".", "-", ":", "-").Replace(filepath.Clean(path))
}

// jsonlLine is a minimal representation of a JSONL line for parsing.
//
// NOTE: this struct and parseSessionMeta are duplicated in
// plugins/bossd-plugin-claude/transcript.go — keep the two copies in sync
// (same drift caveat as PathToProjectKey / pathToProjectKey).
type jsonlLine struct {
	Type    string   `json:"type"`
	Message jsonlMsg `json:"message"`
	// IsMeta flags Claude Code's synthetic messages (e.g. the
	// <local-command-caveat> line injected when a session starts via a local
	// command). Meta lines must not be chosen as the first-user-message title.
	IsMeta bool `json:"isMeta"`
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

		if line.Type == "user" && line.Message.Role == "user" && !line.IsMeta && summary == "" {
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
