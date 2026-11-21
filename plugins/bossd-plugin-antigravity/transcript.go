package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	"github.com/recurser/bossalib/contract/plugin/jsonlscan"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// transcriptStep is one record of agy's brain transcript_full.jsonl: one step
// of the conversation, appended when the step is written.
type transcriptStep struct {
	Source    string `json:"source"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Content   string `json:"content"`
	ToolCalls []struct {
		Name string `json:"name"`
	} `json:"tool_calls"`
}

// stepKind is what a transcript step means for the conversation.
type stepKind int

const (
	stepOther stepKind = iota
	stepUser
	stepAssistantText
	stepToolCall
	stepToolResult
)

func (s transcriptStep) kind() stepKind {
	switch s.Type {
	case "USER_INPUT":
		return stepUser
	case "PLANNER_RESPONSE":
		if len(s.ToolCalls) > 0 {
			return stepToolCall
		}
		if strings.TrimSpace(s.Content) != "" {
			return stepAssistantText
		}
	case "GENERIC":
		return stepToolResult
	}
	return stepOther
}

func (s transcriptStep) createdAt() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s.CreatedAt)
	return t, err == nil
}

// userRequest returns what the user typed. agy wraps the input in
// <USER_REQUEST> and appends metadata blocks (local time, settings changes).
func (s transcriptStep) userRequest() string {
	content := s.Content
	const open, closing = "<USER_REQUEST>", "</USER_REQUEST>"
	if start := strings.Index(content, open); start >= 0 {
		content = content[start+len(open):]
		if end := strings.Index(content, closing); end >= 0 {
			content = content[:end]
		}
	}
	return strings.TrimSpace(content)
}

func readSteps(r io.Reader) ([]transcriptStep, error) {
	var steps []transcriptStep
	scanner := jsonlscan.New(r)
	for scanner.Scan() {
		var step transcriptStep
		if json.Unmarshal(scanner.Bytes(), &step) == nil && step.Type != "" {
			steps = append(steps, step)
		}
	}
	return steps, scanner.Err()
}

func readTranscriptSteps(path string) ([]transcriptStep, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readSteps(f)
}

// transcriptMessages flattens steps into the user and assistant text turns
// bossd shows, keeping the newest maxMessages when maxMessages > 0.
func transcriptMessages(steps []transcriptStep, maxMessages int) (msgs []*bossanovav1.ChatMessage, finalAssistant string) {
	for _, step := range steps {
		switch step.kind() {
		case stepUser:
			msgs = append(msgs, &bossanovav1.ChatMessage{Role: "user", Text: step.userRequest(), Timestamp: step.CreatedAt, Kind: "text"})
		case stepAssistantText:
			text := strings.TrimSpace(step.Content)
			msgs = append(msgs, &bossanovav1.ChatMessage{Role: "assistant", Text: text, Timestamp: step.CreatedAt, Kind: "text"})
			finalAssistant = text
		case stepOther, stepToolCall, stepToolResult:
		}
	}
	if maxMessages > 0 && len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}
	return msgs, finalAssistant
}

// lastTurnIsUser reports whether the newest user-or-assistant turn is the
// user's. Tool steps and system notices do not count as turns.
func lastTurnIsUser(steps []transcriptStep) bool {
	for i := len(steps) - 1; i >= 0; i-- {
		switch steps[i].kind() {
		case stepUser:
			return true
		case stepAssistantText, stepToolCall:
			return false
		case stepOther, stepToolResult:
		}
	}
	return false
}

func firstUserRequest(steps []transcriptStep) string {
	for _, step := range steps {
		if step.kind() == stepUser {
			if text := step.userRequest(); text != "" {
				return text
			}
		}
	}
	return ""
}

// firstUserAt is when the conversation's first user turn was written.
func firstUserAt(steps []transcriptStep) (time.Time, bool) {
	for _, step := range steps {
		if step.kind() == stepUser {
			return step.createdAt()
		}
	}
	return time.Time{}, false
}

func chatTitleFromRequest(request string) string {
	return agentruntime.TruncateSummary(agentruntime.FirstLine(agentruntime.StripXMLTags(request)))
}

const progressTailBytes = 64 * 1024

type progressSnapshot struct {
	lastProgressAt         time.Time
	lastAssistantMessageAt time.Time
	phase                  bossanovav1.AgentProgressPhase
}

// readProgress classifies the newest semantic step in the transcript tail.
//
// created_at is when a step started, at one-second precision, so a tool that
// ran for fifteen minutes leaves a result step stamped with its start. The
// transcript's modification time therefore floors lastProgressAt; without it a
// long build would read as a model request that stalled the moment it ended.
func readProgress(path string) (progressSnapshot, bool) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return progressSnapshot{}, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.IsDir() || info.Size() == 0 {
		return progressSnapshot{}, false
	}
	offset := max(info.Size()-progressTailBytes, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, offset, info.Size()-offset))
	if err != nil {
		return progressSnapshot{}, false
	}
	if offset > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			return progressSnapshot{}, false
		}
		data = data[nl+1:]
	}
	steps, err := readSteps(bytes.NewReader(data))
	if err != nil {
		return progressSnapshot{}, false
	}
	for i := len(steps) - 1; i >= 0; i-- {
		phase, ok := progressPhase(steps[i])
		if !ok {
			continue
		}
		at, ok := steps[i].createdAt()
		if !ok {
			return progressSnapshot{}, false
		}
		snap := progressSnapshot{lastProgressAt: at, phase: phase}
		if mod := info.ModTime(); mod.After(at) {
			snap.lastProgressAt = mod
		}
		if steps[i].kind() == stepAssistantText {
			snap.lastAssistantMessageAt = at
		}
		return snap, true
	}
	return progressSnapshot{}, false
}

func progressPhase(step transcriptStep) (bossanovav1.AgentProgressPhase, bool) {
	switch step.kind() {
	case stepUser:
		return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL, true
	case stepToolCall:
		return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_EXECUTING_TOOL, true
	case stepToolResult:
		if step.Status == "RUNNING" {
			return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_EXECUTING_TOOL, true
		}
		return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL, true
	case stepAssistantText:
		return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_IDLE, true
	case stepOther:
	}
	return bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN, false
}
