// Package claude provides helpers for interacting with Claude Code project data.
package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Chat represents a previous Claude Code conversation.
type Chat struct {
	UUID       string    // Session UUID (filename stem)
	Slug       string    // Human-readable slug (e.g. "velvet-singing-hickey")
	Summary    string    // First user message text, truncated
	ModifiedAt time.Time // File mtime
}

// maxChats is the maximum number of chats to return.
const maxChats = 20

// maxScanLines is how many JSONL lines to scan for metadata per file.
const maxScanLines = 50

// maxSummaryLen is the maximum length of the summary text.
const maxSummaryLen = 80

// DiscoverChats scans the Claude Code project directory for previous conversations
// associated with the given worktree path. Returns chats sorted by modification
// time (most recent first), capped at maxChats. Returns nil, nil if the project
// directory does not exist.
func DiscoverChats(worktreePath string) ([]Chat, error) {
	projectKey := pathToProjectKey(worktreePath)
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	projectDir := filepath.Join(homeDir, ".claude", "projects", projectKey)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var chats []Chat
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}

		uuid := strings.TrimSuffix(entry.Name(), ".jsonl")
		fullPath := filepath.Join(projectDir, entry.Name())

		info, err := entry.Info()
		if err != nil {
			continue
		}

		slug, summary := parseSessionMeta(fullPath)
		if summary == "" {
			continue // Skip files with no user messages
		}

		chats = append(chats, Chat{
			UUID:       uuid,
			Slug:       slug,
			Summary:    summary,
			ModifiedAt: info.ModTime(),
		})
	}

	// Sort by modification time, most recent first.
	sort.Slice(chats, func(i, j int) bool {
		return chats[i].ModifiedAt.After(chats[j].ModifiedAt)
	})

	if len(chats) > maxChats {
		chats = chats[:maxChats]
	}

	return chats, nil
}

// DiscoverChatsInDir is like DiscoverChats but scans a specific directory
// instead of deriving it from the worktree path. Useful for testing.
func DiscoverChatsInDir(projectDir string) ([]Chat, error) {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var chats []Chat
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}

		uuid := strings.TrimSuffix(entry.Name(), ".jsonl")
		fullPath := filepath.Join(projectDir, entry.Name())

		info, err := entry.Info()
		if err != nil {
			continue
		}

		slug, summary := parseSessionMeta(fullPath)
		if summary == "" {
			continue
		}

		chats = append(chats, Chat{
			UUID:       uuid,
			Slug:       slug,
			Summary:    summary,
			ModifiedAt: info.ModTime(),
		})
	}

	sort.Slice(chats, func(i, j int) bool {
		return chats[i].ModifiedAt.After(chats[j].ModifiedAt)
	})

	if len(chats) > maxChats {
		chats = chats[:maxChats]
	}

	return chats, nil
}

// pathToProjectKey converts a filesystem path to a Claude Code project key.
// e.g. "/Users/dave/foo" → "-Users-dave-foo"
func pathToProjectKey(path string) string {
	return strings.ReplaceAll(path, "/", "-")
}

// jsonlLine is a minimal representation of a JSONL line for parsing.
type jsonlLine struct {
	Type    string      `json:"type"`
	Slug    string      `json:"slug"`
	Message jsonlMsg    `json:"message"`
}

type jsonlMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseSessionMeta scans the first maxScanLines of a JSONL file to extract the
// slug and first user message text.
func parseSessionMeta(path string) (slug, summary string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for i := 0; i < maxScanLines && scanner.Scan(); i++ {
		var line jsonlLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}

		if line.Slug != "" && slug == "" {
			slug = line.Slug
		}

		if line.Type == "user" && line.Message.Role == "user" && summary == "" {
			summary = extractText(line.Message.Content)
		}

		if slug != "" && summary != "" {
			break
		}
	}

	return slug, summary
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
		return truncateSummary(firstLine(s))
	}

	// Try as array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return truncateSummary(firstLine(b.Text))
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

func truncateSummary(s string) string {
	if len(s) <= maxSummaryLen {
		return s
	}
	return s[:maxSummaryLen-3] + "..."
}
