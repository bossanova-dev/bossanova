// Package main provides helpers for reading codex CLI session transcripts.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const maxScanLines = 200
const maxSummaryLen = 80

// transcriptTailSize is the trailing byte window scanned for the most recent
// meaningful JSONL entry. 32 KB holds dozens of envelope entries in a typical
// codex transcript.
const transcriptTailSize = 32 * 1024

// codexSessionsDir is the on-disk root for codex rollout transcripts. Per
// Lane 0 spike, codex writes:
//
//	~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ISO-ts>-<UUID>.jsonl
//
// The path is sharded by date — there is no per-worktree key — so we find the
// transcript for a given session UUID by globbing across the date shards.
const codexSessionsDir = ".codex/sessions"

// codexSessionsRoot returns the codex sessions/ directory, honoring CODEX_HOME
// (via codexConfigDir) so per-account homes resolve their own rollouts. With
// CODEX_HOME unset this is byte-identical to ~/.codex/sessions.
func codexSessionsRoot() (string, error) {
	dir, err := codexConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions"), nil
}

// transcriptPath resolves the on-disk codex rollout JSONL file for the
// given agent session UUID by globbing every date shard under the codex
// sessions root (honoring CODEX_HOME; see codexSessionsRoot). The workDir
// argument is unused (codex transcripts are not keyed by working directory)
// but kept in the signature to match the daemon-side host_service contract
// that all agent plugins share.
//
// Returns ("", error) when the sessions root is unresolvable, when no
// rollout file matches the UUID, or when more than one match exists (which
// would indicate a corrupted sessions tree).
func transcriptPath(_ string, agentSessionID string) (string, error) {
	if agentSessionID == "" {
		return "", errors.New("agentSessionID is empty")
	}
	root, err := codexSessionsRoot()
	if err != nil {
		return "", err
	}
	return findRolloutPath(root, agentSessionID)
}

// findRolloutPath globs `<root>/<YYYY>/<MM>/<DD>/rollout-*-<uuid>.jsonl`
// and returns the match. In practice codex emits one rollout per UUID; if
// duplicates ever appear (e.g. clock skew, crashed-and-restarted runs)
// we prefer the most recently modified file rather than failing — the
// freshest transcript is the one the daemon's status path should read.
// Exposed for tests so the sessions root can be redirected to a temp dir.
func findRolloutPath(root, agentSessionID string) (string, error) {
	// agentSessionID is externally supplied and interpolated into the glob
	// pattern below. A codex session id is a single UUID segment; reject any id
	// carrying path separators or ".." so a crafted value cannot collapse the
	// pattern out of the sessions root and read an arbitrary file.
	if strings.ContainsAny(agentSessionID, `/\`) || strings.Contains(agentSessionID, "..") {
		return "", fmt.Errorf("invalid codex session id %q", agentSessionID)
	}
	pattern := filepath.Join(root, "*", "*", "*", fmt.Sprintf("rollout-*-%s.jsonl", agentSessionID))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("glob codex sessions: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no codex rollout for session %s", agentSessionID)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	best := matches[0]
	bestInfo, _ := os.Stat(best)
	for _, m := range matches[1:] {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if bestInfo == nil || info.ModTime().After(bestInfo.ModTime()) {
			best, bestInfo = m, info
		}
	}
	return best, nil
}

// transcriptExists reports whether a codex rollout transcript for the given
// (workDir, agentSessionID) is present on disk and non-empty. Used by the
// daemon to choose between `codex exec resume <UUID>` (transcript present)
// and a fresh start (transcript missing). Errors collapse to false.
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

// codexEnvelope is the shared shape of every line in a codex rollout JSONL
// file: a wrapping `{timestamp, type, payload}` envelope around the inner
// event. Inner payloads vary by type; we decode them lazily.
type codexEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMetaPayload struct {
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	CWD        string `json:"cwd"`
	Originator string `json:"originator"`
	CLIVersion string `json:"cli_version"`
}

type interactiveSessionCandidate struct {
	ID   string
	Path string
	Time time.Time
}

func sameWorkDir(a, b string) bool {
	aa, errA := filepath.EvalSymlinks(a)
	if errA != nil {
		aa, _ = filepath.Abs(a)
	}
	bb, errB := filepath.EvalSymlinks(b)
	if errB != nil {
		bb, _ = filepath.Abs(b)
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

func scanInteractiveSessionCandidates(root, workDir string, launchedAfter time.Time) []interactiveSessionCandidate {
	return scanInteractiveSessionCandidatesInWindow(root, workDir, launchedAfter.Add(-2*time.Second), time.Time{})
}

func scanInteractiveSessionCandidatesInWindow(root, workDir string, notBefore, notAfter time.Time) []interactiveSessionCandidate {
	var candidates []interactiveSessionCandidate
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if matched, _ := filepath.Match("rollout-*.jsonl", filepath.Base(path)); !matched {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		meta, ok := readSessionMeta(path)
		if !ok {
			return nil
		}
		if meta.ID == "" || meta.Originator != "codex-tui" || !sameWorkDir(meta.CWD, workDir) {
			return nil
		}
		sessionTime := info.ModTime()
		if meta.Timestamp != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
				sessionTime = parsed
			}
		}
		if sessionTime.Before(notBefore) {
			return nil
		}
		if !notAfter.IsZero() && sessionTime.After(notAfter) {
			return nil
		}
		candidates = append(candidates, interactiveSessionCandidate{
			ID:   meta.ID,
			Path: path,
			Time: sessionTime,
		})
		return nil
	})
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Time.Equal(candidates[j].Time) {
			return candidates[i].Path > candidates[j].Path
		}
		return candidates[i].Time.After(candidates[j].Time)
	})
	return candidates
}

func readSessionMeta(path string) (codexSessionMetaPayload, bool) {
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return codexSessionMetaPayload{}, false
	}
	defer func() { _ = f.Close() }()

	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return codexSessionMetaPayload{}, false
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return codexSessionMetaPayload{}, false
	}
	var env codexEnvelope
	if err := json.Unmarshal(line, &env); err != nil || env.Type != "session_meta" {
		return codexSessionMetaPayload{}, false
	}
	var meta codexSessionMetaPayload
	if err := json.Unmarshal(env.Payload, &meta); err != nil {
		return codexSessionMetaPayload{}, false
	}
	if meta.Timestamp == "" {
		meta.Timestamp = env.Timestamp
	}
	return meta, true
}

func resolveInteractiveSessionID(workDir string, launchedAfter time.Time) (id, transcriptPath string, ambiguous bool, reason string) {
	root, err := codexSessionsRoot()
	if err != nil {
		return "", "", false, "no matching codex-tui rollout found"
	}
	return resolveInteractiveSessionIDAt(root, workDir, launchedAfter)
}

func resolveInteractiveSessionIDAt(root, workDir string, launchedAfter time.Time) (id, transcriptPath string, ambiguous bool, reason string) {
	candidates := scanInteractiveSessionCandidates(root, workDir, launchedAfter)
	if len(candidates) == 0 {
		return "", "", false, "no matching codex-tui rollout found"
	}
	newest := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.ID != newest.ID {
			return "", "", true, "multiple matching codex-tui rollouts found"
		}
	}
	return newest.ID, newest.Path, false, ""
}

func resolveLegacyInteractiveSessionID(workDir string, chatCreatedAt time.Time) (id, transcriptPath string, ambiguous bool, reason string) {
	root, err := codexSessionsRoot()
	if err != nil {
		return "", "", false, "no matching codex-tui rollout found"
	}
	return resolveLegacyInteractiveSessionIDAt(root, workDir, chatCreatedAt, time.Now())
}

func resolveLegacyInteractiveSessionIDAt(root, workDir string, chatCreatedAt, now time.Time) (id, transcriptPath string, ambiguous bool, reason string) {
	if chatCreatedAt.IsZero() {
		return "", "", false, "no matching codex-tui rollout found"
	}
	if now.IsZero() {
		now = time.Now()
	}
	notAfter := now
	createdWindowEnd := chatCreatedAt.Add(2 * time.Minute)
	if now.After(createdWindowEnd) {
		notAfter = createdWindowEnd
	}
	candidates := scanInteractiveSessionCandidatesInWindow(root, workDir, chatCreatedAt.Add(-5*time.Minute), notAfter)
	if len(candidates) == 0 {
		return "", "", false, "no matching codex-tui rollout found"
	}
	if len(candidates) > 1 {
		return "", "", true, "multiple matching codex-tui rollouts found"
	}
	return candidates[0].ID, candidates[0].Path, false, ""
}

// codexEventMsgPayload is the payload shape for type:"event_msg" entries.
// Codex distinguishes user/agent/tool turns by the inner `type` field on
// the payload (user_message, agent_message, function_call_output, etc.).
type codexEventMsgPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// codexResponseItemPayload is the payload shape for type:"response_item".
// We only care about message-role turns and function_call_output entries.
type codexResponseItemPayload struct {
	Type string `json:"type"`
	Role string `json:"role"`
}

// lastTurnIsUser reports whether the most recent meaningful entry in a codex
// rollout JSONL transcript is a real user turn — i.e. an event_msg with
// inner type "user_message", or a response_item message with role:"user"
// that carries actual input_text content.
//
// We walk the tail backward, skipping bookkeeping events (token_count,
// turn_context, task_started, function_call, function_call_output, reasoning).
// We return false the moment we see an agent turn (event_msg/agent_message
// or response_item message role:"assistant"), and we never treat a function
// call output as a user turn even though codex sometimes encodes those as
// user-facing entries.
//
// Errors and empty tails return false — callers treat that as "don't suppress
// the question state".
func lastTurnIsUser(path string) bool {
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
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
		var env codexEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		switch env.Type {
		case "event_msg":
			var p codexEventMsgPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				continue
			}
			switch p.Type {
			case "user_message":
				if strings.TrimSpace(p.Message) != "" {
					return true
				}
			case "agent_message":
				return false
			case "task_complete":
				// Codex emits task_complete when the agent finishes a turn,
				// even when the agent's response was all tool calls and no
				// final text (so no agent_message was emitted). Treating
				// task_complete as agent-last keeps `user_message →
				// task_complete` transcripts from being misclassified as
				// user-last, which would otherwise suppress legitimate
				// question-state detection downstream.
				return false
				// task_started, token_count, etc. — ignore.
			}
		case "response_item":
			var p codexResponseItemPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				continue
			}
			// function_call_output is bookkeeping protocol plumbing; never
			// counts as a user turn.
			if p.Type == "function_call_output" {
				continue
			}
			if p.Type == "message" {
				switch p.Role {
				case "assistant":
					return false
				case "user":
					// Real user input — but codex also emits an
					// `<environment_context>` synthetic user message at the
					// top of every session. The event_msg/user_message path
					// above is the authoritative signal; if we get here, the
					// user just hasn't reached an event_msg envelope yet, so
					// don't mark it "user". Keep walking.
					continue
				}
			}
		}
	}
	return false
}

// chatTitle reads codex's session index for a display thread name, falling
// back to the rollout JSONL's first user-typed message.
func chatTitle(workDir, agentSessionID string) string {
	if name := sessionIndexThreadName(agentSessionID); name != "" {
		return name
	}
	path, err := transcriptPath(workDir, agentSessionID)
	if err != nil {
		return ""
	}
	return chatTitleAtPath(path)
}

type codexSessionIndexEntry struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
}

// sessionIndexThreadName returns the latest thread_name recorded for a codex
// session id in CODEX_HOME/session_index.jsonl.
func sessionIndexThreadName(agentSessionID string) string {
	if agentSessionID == "" {
		return ""
	}
	configDir, err := codexConfigDir()
	if err != nil {
		return ""
	}
	indexPath := filepath.Clean(filepath.Join(configDir, "session_index.jsonl"))
	f, err := os.Open(indexPath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var title string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		var entry codexSessionIndexEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.ID != agentSessionID {
			continue
		}
		// Keep the last non-empty thread_name for this id. Codex upserts one
		// entry per session id (keyed by id, carrying updated_at), so in
		// practice there is a single match and "latest wins" is trivial; the
		// full scan just makes us robust if codex ever switches to append.
		// An empty thread_name never overwrites a real name and leaves title
		// "", so chatTitle falls back to the non-explicit rollout title.
		if t := truncate(firstLine(stripXMLTags(entry.ThreadName))); t != "" {
			title = t
		}
	}
	return title
}

// chatTitleAtPath scans the first maxScanLines of the rollout JSONL at path
// and returns the first event_msg/user_message text (or, failing that, the
// first response_item message role:"user" input_text). Synthetic
// `<environment_context>` messages are filtered out by the XML-tag stripper.
func chatTitleAtPath(path string) string {
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	// Two passes via a single walk: prefer event_msg/user_message because that
	// is the post-hello, real-user-typed entry. Fall back to the first
	// response_item message:user input_text if no event_msg appears in the
	// scan window.
	var fallback string
	for i := 0; i < maxScanLines && scanner.Scan(); i++ {
		var env codexEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		switch env.Type {
		case "event_msg":
			var p codexEventMsgPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				continue
			}
			if p.Type == "user_message" {
				if t := truncate(firstLine(stripXMLTags(p.Message))); t != "" {
					return t
				}
			}
		case "response_item":
			if fallback != "" {
				continue
			}
			fallback = extractResponseItemUserText(env.Payload)
		}
	}
	return fallback
}

// extractResponseItemUserText pulls the first input_text content from a
// response_item payload that represents a user message. Returns "" for
// non-user response items, synthetic environment_context-only messages, and
// empty content arrays.
func extractResponseItemUserText(raw json.RawMessage) string {
	var p struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if p.Type != "message" || p.Role != "user" {
		return ""
	}
	for _, c := range p.Content {
		if c.Type != "input_text" {
			continue
		}
		if t := truncate(firstLine(stripXMLTags(c.Text))); t != "" {
			return t
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

// parseRolloutMessages reads all chat turns from the codex rollout JSONL at
// path and returns them as ordered []*bossanovav1.ChatMessage. Only
// event_msg/user_message and event_msg/agent_message envelopes are converted;
// protocol noise (session_meta, turn_context, token_count, response_item,
// task_complete, function_call*, etc.) is skipped.
//
// The second return value is the flattened text of the last assistant turn
// seen across the full file (used as FinalAssistantText in
// ReadTranscriptResponse, independent of any MaxMessages tail-cut).
func parseRolloutMessages(path string) ([]*bossanovav1.ChatMessage, string, error) {
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = f.Close() }()

	var msgs []*bossanovav1.ChatMessage
	var lastAssistant string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		var env codexEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		if env.Type != "event_msg" {
			continue
		}
		var p codexEventMsgPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			continue
		}
		var role string
		switch p.Type {
		case "user_message":
			role = "user"
		case "agent_message":
			role = "assistant"
		default:
			continue
		}
		text := strings.TrimSpace(p.Message)
		msgs = append(msgs, &bossanovav1.ChatMessage{
			Role:      role,
			Text:      text,
			Timestamp: env.Timestamp,
			Kind:      "text",
		})
		if role == "assistant" {
			lastAssistant = text
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	return msgs, lastAssistant, nil
}

// readTranscriptAt resolves the rollout JSONL for agentSessionID under root
// and returns parsed chat messages. When no rollout file is found it returns
// {Exists:false} with nil error — callers treat that as transcript-not-yet-
// written rather than a hard failure. Exposed for tests so the sessions root
// can be redirected to a temp dir (mirrors findRolloutPath).
func readTranscriptAt(root, _, agentSessionID string, maxMessages int32) (*bossanovav1.ReadTranscriptResponse, error) {
	path, err := findRolloutPath(root, agentSessionID)
	if err != nil {
		return &bossanovav1.ReadTranscriptResponse{Exists: false}, nil
	}
	msgs, finalAssistant, err := parseRolloutMessages(path)
	if err != nil {
		return nil, err
	}
	if maxMessages > 0 && int(maxMessages) < len(msgs) {
		msgs = msgs[len(msgs)-int(maxMessages):]
	}
	return &bossanovav1.ReadTranscriptResponse{
		Exists:             true,
		Messages:           msgs,
		FinalAssistantText: finalAssistant,
	}, nil
}
