package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SeedAPIKeyApproval idempotently ensures ~/.claude.json's
// customApiKeyResponses.approved contains suffix (the sentinel key's last 20
// chars, from SentinelApprovalSuffix), so the interactive Claude REPL never
// prompts to approve the injected sentinel ANTHROPIC_API_KEY (BOS-326). It MERGES
// into the existing document, preserving every other key — ~/.claude.json holds
// a lot of unrelated REPL state. Fail-soft: a missing file is created; an
// unreadable or parse-broken file returns an error the caller logs and ignores
// (the approval just won't be pre-seeded and the first session may prompt once).
func SeedAPIKeyApproval(claudeJSONPath, suffix string) error {
	doc := map[string]any{}
	// #nosec G304 -- reads ~/.claude.json to merge API-key approval into Claude REPL state; internal $HOME path (secret-adjacent)
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	if b, err := os.ReadFile(claudeJSONPath); err == nil {
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", claudeJSONPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", claudeJSONPath, err)
	}

	resp, _ := doc["customApiKeyResponses"].(map[string]any)
	if resp == nil {
		resp = map[string]any{}
	}
	approved, _ := resp["approved"].([]any)
	for _, v := range approved {
		if s, _ := v.(string); s == suffix {
			return nil // already approved — idempotent no-op
		}
	}
	resp["approved"] = append(approved, suffix)
	doc["customApiKeyResponses"] = resp

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", claudeJSONPath, err)
	}
	// Atomic write (temp file + rename): ~/.claude.json holds a lot of unrelated
	// REPL state, so a crash mid-write must never truncate it. Rename is atomic
	// within the same directory.
	dir := filepath.Dir(claudeJSONPath)
	tmp, err := os.CreateTemp(dir, ".claude.json.seed-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", claudeJSONPath, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp for %s: %w", claudeJSONPath, err)
	}
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %s: %w", claudeJSONPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", claudeJSONPath, err)
	}
	if err := os.Rename(tmpName, claudeJSONPath); err != nil {
		return fmt.Errorf("rename temp into %s: %w", claudeJSONPath, err)
	}
	return nil
}
