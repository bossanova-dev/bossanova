package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// agyStore reads agy's on-disk state under ~/.gemini/antigravity-cli. Every
// read is best effort: a missing or malformed file answers "unknown".
type agyStore struct {
	root string
}

func newAgyStore() agyStore {
	home, err := os.UserHomeDir()
	if err != nil {
		return agyStore{}
	}
	return agyStore{root: filepath.Join(home, ".gemini", "antigravity-cli")}
}

// canonicalConversationID accepts only agy's lowercase UUID form, so an id can
// never steer a path outside the store.
func canonicalConversationID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func (s agyStore) transcriptPath(id string) (string, bool) {
	if s.root == "" || !canonicalConversationID(id) {
		return "", false
	}
	return filepath.Join(s.root, "brain", id, ".system_generated", "logs", "transcript_full.jsonl"), true
}

func (s agyStore) conversationsDir() string { return filepath.Join(s.root, "conversations") }

// lastConversation returns agy's most recent conversation for a working
// directory. agy keys cache/last_conversations.json by the cwd exactly as its
// process saw it, which may be either spelling of a symlinked path.
func (s agyStore) lastConversation(workDir string) string {
	if s.root == "" || workDir == "" {
		return ""
	}
	var byDir map[string]string
	if !readJSON(filepath.Join(s.root, "cache", "last_conversations.json"), &byDir) {
		return ""
	}
	if id := byDir[workDir]; canonicalConversationID(id) {
		return id
	}
	if real, err := filepath.EvalSymlinks(workDir); err == nil {
		if id := byDir[real]; canonicalConversationID(id) {
			return id
		}
	}
	return ""
}

// conversationTitle returns the title agy stored for a conversation.
func (s agyStore) conversationTitle(id string) string {
	if s.root == "" {
		return ""
	}
	var meta struct {
		Conversations map[string]struct {
			Summary struct {
				Title string `json:"Title"`
			} `json:"summary"`
		} `json:"conversations"`
	}
	if !readJSON(filepath.Join(s.root, "cache", "conversation_metadata.json"), &meta) {
		return ""
	}
	return meta.Conversations[id].Summary.Title
}

func readJSON(path string, v any) bool {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}
