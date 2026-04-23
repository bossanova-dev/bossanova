package status

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// transcriptTailSize is the trailing byte window scanned for the most recent
// meaningful JSONL entry. 32 KB holds ~60-200 turns in a typical transcript.
const transcriptTailSize = 32 * 1024

// pathToProjectKey mirrors Claude Code's project-directory encoding: both "/"
// and "." become "-". Duplicated from services/boss/internal/claude/title.go
// to avoid a cross-service internal import.
func pathToProjectKey(path string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(path)
}

// transcriptPath resolves ~/.claude/projects/<key>/<claudeID>.jsonl.
func transcriptPath(worktreePath, claudeID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects", pathToProjectKey(worktreePath), claudeID+".jsonl"), nil
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
