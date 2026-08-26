package agenttelemetry

import (
	"bufio"
	"context"
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
)

var claudeProjectKeyRe = regexp.MustCompile(`[^A-Za-z0-9]`)

type Counts struct {
	ParentModelCallCount int64
	ChildModelCallCount  int64
	ToolCallCount        int64
	SubagentCount        int64
	DirectSubagentCount  int64
	OutputTokenCount     *int64
	ReasoningTokenCount  *int64
	Children             []ChildSpan
}

type ChildSpan struct {
	AgentSessionID      string
	ParentAgentID       string
	SpawnDepth          int32
	StartedAt           time.Time
	StoppedAt           *time.Time
	ModelCallCount      int64
	ToolCallCount       int64
	OutputTokenCount    *int64
	ReasoningTokenCount *int64
}

type CodexSessionMeta struct {
	CWD string `json:"cwd"`
}

func SameWorkDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil {
		a = ra
	}
	if errB == nil {
		b = rb
	}
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA == nil {
		a = aa
	}
	if errB == nil {
		b = bb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func TallyCodex(r io.Reader) (Counts, error) {
	return TallyCodexSince(r, time.Time{})
}

func TallyCodexSince(r io.Reader, since time.Time) (Counts, error) {
	return TallyCodexSinceContext(context.Background(), r, since)
}

func TallyCodexSinceContext(ctx context.Context, r io.Reader, since time.Time) (Counts, error) {
	counts, _, err := tallyCodexSinceContextWithBaseline(ctx, r, since, time.Time{}, nil)
	return counts, err
}

func tallyCodexSinceContextWithBaseline(ctx context.Context, r io.Reader, since, until time.Time, baseline *codexTokenCount) (Counts, *codexTokenCount, error) {
	return tallyJSONL(ctx, r, "codex", since, until, baseline)
}

func CodexTranscriptPath(sessionID string) (string, error) {
	paths, err := CodexTranscriptPaths(sessionID)
	if err != nil {
		return "", err
	}
	return paths[len(paths)-1], nil
}

func CodexTranscriptPaths(sessionID string) ([]string, error) {
	if sessionID == "" {
		return nil, errors.New("sessionID is empty")
	}
	if strings.ContainsAny(sessionID, `/\`) || strings.Contains(sessionID, "..") {
		return nil, fmt.Errorf("invalid codex session id %q", sessionID)
	}
	root, err := codexSessionsRoot()
	if err != nil {
		return nil, err
	}
	pattern := filepath.Join(root, "*", "*", "*", fmt.Sprintf("rollout-*-%s.jsonl", sessionID))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob codex sessions: %w", err)
	}
	if len(matches) == 0 {
		return nil, os.ErrNotExist
	}
	sort.Slice(matches, func(i, j int) bool {
		left, leftErr := os.Stat(matches[i])
		right, rightErr := os.Stat(matches[j])
		if leftErr == nil && rightErr == nil && !left.ModTime().Equal(right.ModTime()) {
			return left.ModTime().Before(right.ModTime())
		}
		if leftErr == nil && rightErr != nil {
			return true
		}
		if leftErr != nil && rightErr == nil {
			return false
		}
		return matches[i] < matches[j]
	})
	return matches, nil
}

func TallyCodexPaths(paths []string) (Counts, error) {
	return TallyCodexPathsSince(paths, time.Time{})
}

func TallyCodexPathsSince(paths []string, since time.Time) (Counts, error) {
	return TallyCodexPathsSinceContext(context.Background(), paths, since)
}

func TallyCodexPathsSinceContext(ctx context.Context, paths []string, since time.Time) (Counts, error) {
	return TallyCodexPathsWindowContext(ctx, paths, since, time.Time{})
}

func TallyCodexPathsWindowContext(ctx context.Context, paths []string, since, until time.Time) (Counts, error) {
	if len(paths) == 0 {
		return Counts{}, os.ErrNotExist
	}
	var out Counts
	var tallied bool
	var baseline *codexTokenCount
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return Counts{}, err
		}
		f, err := os.Open(filepath.Clean(path))
		if err != nil {
			continue
		}
		counts, nextBaseline, tallyErr := tallyCodexSinceContextWithBaseline(ctx, f, since, until, baseline)
		_ = f.Close()
		if tallyErr != nil {
			return Counts{}, tallyErr
		}
		if !since.IsZero() {
			baseline = nextBaseline
		}
		tallied = true
		out.ParentModelCallCount += counts.ParentModelCallCount
		out.ChildModelCallCount += counts.ChildModelCallCount
		out.ToolCallCount += counts.ToolCallCount
		out.SubagentCount += counts.SubagentCount
		out.DirectSubagentCount += counts.DirectSubagentCount
		if counts.OutputTokenCount != nil {
			addPtr(&out.OutputTokenCount, *counts.OutputTokenCount)
		}
		if counts.ReasoningTokenCount != nil {
			addPtr(&out.ReasoningTokenCount, *counts.ReasoningTokenCount)
		}
		out.Children = append(out.Children, counts.Children...)
	}
	if !tallied {
		return Counts{}, os.ErrNotExist
	}
	return out, nil
}

func codexSessionsRoot() (string, error) {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

func TallyClaude(r io.Reader) (Counts, error) {
	return TallyClaudeSince(r, time.Time{})
}

func TallyClaudeSince(r io.Reader, since time.Time) (Counts, error) {
	return TallyClaudeSinceContext(context.Background(), r, since)
}

func TallyClaudeSinceContext(ctx context.Context, r io.Reader, since time.Time) (Counts, error) {
	counts, _, err := tallyJSONL(ctx, r, "claude", since, time.Time{}, nil)
	return counts, err
}

func TallyClaudePath(path string) (Counts, error) {
	return TallyClaudePathForSession(path, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
}

func ClaudeTranscriptPath(worktreePath, sessionID string) (string, error) {
	if strings.ContainsAny(sessionID, `/\`) || strings.Contains(sessionID, "..") {
		return "", fmt.Errorf("invalid claude session id %q", sessionID)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	projectKey := claudeProjectKeyRe.ReplaceAllString(filepath.Clean(worktreePath), "-")
	return filepath.Join(home, ".claude", "projects", projectKey, sessionID+".jsonl"), nil
}

func TallyClaudePathForSession(path, sessionID string) (Counts, error) {
	return TallyClaudePathWithChildren(path, filepath.Dir(path), sessionID)
}

func TallyClaudePathForSessionContext(ctx context.Context, path, sessionID string) (Counts, error) {
	return TallyClaudePathWithChildrenSinceContext(ctx, path, filepath.Dir(path), sessionID, time.Time{})
}

func TallyClaudePathWithChildren(path, childParentDir, sessionID string) (Counts, error) {
	return TallyClaudePathWithChildrenSince(path, childParentDir, sessionID, time.Time{})
}

func TallyClaudePathWithChildrenSince(path, childParentDir, sessionID string, since time.Time) (Counts, error) {
	return TallyClaudePathWithChildrenSinceContext(context.Background(), path, childParentDir, sessionID, since)
}

func TallyClaudePathWithChildrenSinceContext(ctx context.Context, path, childParentDir, sessionID string, since time.Time) (Counts, error) {
	return TallyClaudePathWithChildrenWindowContext(ctx, path, childParentDir, sessionID, since, time.Time{})
}

func TallyClaudePathWithChildrenWindowContext(ctx context.Context, path, childParentDir, sessionID string, since, until time.Time) (Counts, error) {
	if err := ctx.Err(); err != nil {
		return Counts{}, err
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return Counts{}, err
	}
	defer func() { _ = f.Close() }()
	counts, _, err := tallyJSONL(ctx, f, "claude", since, until, nil)
	if err != nil {
		return Counts{}, err
	}
	children, err := ClaudeChildSpansContext(ctx, childParentDir, sessionID+".jsonl")
	if err != nil {
		return Counts{}, err
	}
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return Counts{}, err
		}
		if !since.IsZero() && child.StartedAt.Before(since) {
			continue
		}
		if !until.IsZero() && !child.StartedAt.Before(until) {
			continue
		}
		counts.Children = append(counts.Children, child)
		counts.ChildModelCallCount += child.ModelCallCount
		counts.ToolCallCount += child.ToolCallCount
		if child.OutputTokenCount != nil {
			addPtr(&counts.OutputTokenCount, *child.OutputTokenCount)
		}
		if child.ReasoningTokenCount != nil {
			addPtr(&counts.ReasoningTokenCount, *child.ReasoningTokenCount)
		}
	}
	counts.SubagentCount = int64(len(counts.Children))
	for _, child := range counts.Children {
		if child.SpawnDepth == 1 {
			counts.DirectSubagentCount++
		}
	}
	return counts, nil
}

func ChildSpanFromJSONL(path string) (ChildSpan, error) {
	return ChildSpanFromJSONLContext(context.Background(), path)
}

func ChildSpanFromJSONLContext(ctx context.Context, path string) (ChildSpan, error) {
	if err := ctx.Err(); err != nil {
		return ChildSpan{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return ChildSpan{}, err
	}
	defer func() { _ = f.Close() }()
	counts, _, err := tallyJSONL(ctx, f, "claude", time.Time{}, time.Time{}, nil)
	if err != nil {
		return ChildSpan{}, err
	}
	if err := ctx.Err(); err != nil {
		return ChildSpan{}, err
	}
	start, stop, err := timestamps(path)
	if err != nil {
		return ChildSpan{}, err
	}
	agentID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return ChildSpan{
		AgentSessionID:      agentID,
		StartedAt:           start,
		StoppedAt:           &stop,
		ModelCallCount:      counts.ParentModelCallCount,
		ToolCallCount:       counts.ToolCallCount,
		OutputTokenCount:    counts.OutputTokenCount,
		ReasoningTokenCount: counts.ReasoningTokenCount,
	}, nil
}

func ClaudeChildSpans(parentDir, parentFile string) ([]ChildSpan, error) {
	return ClaudeChildSpansContext(context.Background(), parentDir, parentFile)
}

func ClaudeChildSpansContext(ctx context.Context, parentDir, parentFile string) ([]ChildSpan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessionID := strings.TrimSuffix(parentFile, filepath.Ext(parentFile))
	dir := filepath.Join(parentDir, sessionID, "subagents")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ChildSpan
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		child, err := ChildSpanFromJSONLContext(ctx, path)
		if err != nil {
			continue
		}
		applyClaudeChildMeta(&child, strings.TrimSuffix(path, ".jsonl")+".meta.json")
		out = append(out, child)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out, nil
}

func applyClaudeChildMeta(child *ChildSpan, path string) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return
	}
	var meta struct {
		ParentAgentID string `json:"parentAgentId"`
		SpawnDepth    int32  `json:"spawnDepth"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return
	}
	child.ParentAgentID = meta.ParentAgentID
	child.SpawnDepth = meta.SpawnDepth
}

type genericLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Message   json.RawMessage `json:"message"`
	Payload   json.RawMessage `json:"payload"`
	Item      json.RawMessage `json:"item"`
	Usage     *usage          `json:"usage"`
	Text      string          `json:"text"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Usage   *usage          `json:"usage"`
	Content json.RawMessage `json:"content"`
}

type codexEventPayload struct {
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	Role    string          `json:"role"`
	Message json.RawMessage `json:"message"`
	Info    json.RawMessage `json:"info"`
}

type codexExecItem struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type codexExecTurnCompleted struct {
	Usage   codexTokenCount `json:"usage"`
	Payload struct {
		Usage codexTokenCount `json:"usage"`
	} `json:"payload"`
}

type usage struct {
	OutputTokens        int64               `json:"output_tokens"`
	OutputTokensDetails *outputTokenDetails `json:"output_tokens_details"`
}

type outputTokenDetails struct {
	ThinkingTokens int64 `json:"thinking_tokens"`
}

type codexTokenCount struct {
	InputTokens           int64           `json:"input_tokens"`
	CachedInputTokens     int64           `json:"cached_input_tokens"`
	OutputTokens          int64           `json:"output_tokens"`
	ReasoningTokens       int64           `json:"reasoning_tokens"`
	ReasoningOutputTokens int64           `json:"reasoning_output_tokens"`
	TotalTokens           int64           `json:"total_tokens"`
	Info                  *codexTokenInfo `json:"info"`
}

type codexTokenInfo struct {
	LastTokenUsage  *codexTokenCount `json:"last_token_usage"`
	TotalTokenUsage *codexTokenCount `json:"total_token_usage"`
}

func tallyJSONL(ctx context.Context, r io.Reader, provider string, since, until time.Time, codexTokenBaseline *codexTokenCount) (Counts, *codexTokenCount, error) {
	var out Counts
	var codexAssistantMessages int64
	var codexTokenCountMessages int64
	var codexModelInvocations int64
	var codexToolInvocationOpen bool
	var finalOutputTokenCount *int64
	var finalReasoningTokenCount *int64
	subtractCodexTokenBaseline := codexTokenBaseline
	var finalCodexUsageCumulative bool
	scanner := newJSONLScanner(r)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return Counts{}, codexTokenBaseline, err
		}
		line := scanner.Bytes()
		var msg genericLine
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Type == "" && msg.Text != "" {
			text := msg.Text
			var inner genericLine
			if err := json.Unmarshal([]byte(text), &inner); err == nil {
				msg = inner
				line = []byte(text)
			}
		}
		if provider == "codex" && (msg.Type == "event_msg" || msg.Type == "response_item") && len(msg.Payload) > 0 {
			var payload codexEventPayload
			if err := json.Unmarshal(msg.Payload, &payload); err == nil {
				msg.Type = payload.Type
				msg.Name = payload.Name
				msg.Message = payload.Message
				if len(msg.Message) == 0 {
					msg.Message = msg.Payload
				}
				if msg.Type == "message" && payload.Role == "assistant" {
					msg.Type = "assistant_message"
				}
			}
		}
		var beforeSince, afterUntil bool
		if !since.IsZero() && msg.Timestamp != "" {
			if ts, err := time.Parse(time.RFC3339Nano, msg.Timestamp); err == nil && ts.Before(since) {
				beforeSince = true
			}
		}
		if !until.IsZero() && msg.Timestamp != "" {
			if ts, err := time.Parse(time.RFC3339Nano, msg.Timestamp); err == nil && !ts.Before(until) {
				afterUntil = true
			}
		}
		if beforeSince {
			if provider == "codex" && msg.Type == "token_count" {
				var tc codexTokenCount
				if err := json.Unmarshal(msg.Message, &tc); err == nil {
					if usage, ok := tc.cumulativeUsage(); ok {
						codexTokenBaseline = &usage
						subtractCodexTokenBaseline = &usage
					}
				}
			}
			continue
		}
		if afterUntil {
			continue
		}
		switch msg.Type {
		case "assistant":
			var cm claudeMessage
			if len(msg.Message) > 0 && json.Unmarshal(msg.Message, &cm) == nil && cm.Usage != nil {
				msg.Usage = cm.Usage
			}
			if provider == "claude" && len(msg.Message) > 0 && cm.Role != "assistant" {
				continue
			}
			out.ParentModelCallCount++
			if provider == "claude" {
				out.ToolCallCount += countClaudeToolUseBlocks(cm.Content)
			}
			if msg.Usage != nil {
				addPtr(&out.OutputTokenCount, msg.Usage.OutputTokens)
				if msg.Usage.OutputTokensDetails != nil {
					addPtr(&out.ReasoningTokenCount, msg.Usage.OutputTokensDetails.ThinkingTokens)
				}
			}
		case "assistant_message":
			if provider == "codex" {
				codexAssistantMessages++
				codexModelInvocations++
				codexToolInvocationOpen = false
			} else {
				out.ParentModelCallCount++
			}
			if msg.Usage != nil {
				addPtr(&out.OutputTokenCount, msg.Usage.OutputTokens)
				if msg.Usage.OutputTokensDetails != nil {
					addPtr(&out.ReasoningTokenCount, msg.Usage.OutputTokensDetails.ThinkingTokens)
				}
			}
		case "tool_use", "function_call", "tool_search_call", "function_call_item", "custom_tool_call":
			out.ToolCallCount++
			if provider == "codex" {
				if msg.Type == "custom_tool_call" && isCodexSubagentToolName(msg.Name) {
					out.SubagentCount++
					out.DirectSubagentCount++
				}
				if !codexToolInvocationOpen {
					codexModelInvocations++
					codexToolInvocationOpen = true
				}
			}
		case "function_call_output", "custom_tool_call_output":
			if provider == "codex" {
				codexToolInvocationOpen = false
			}
		case "item.completed":
			var item codexExecItem
			if provider == "codex" && len(msg.Item) > 0 && json.Unmarshal(msg.Item, &item) == nil {
				switch item.Type {
				case "agent_message", "assistant_message":
					codexAssistantMessages++
					codexModelInvocations++
					codexToolInvocationOpen = false
				case "function_call", "tool_call", "custom_tool_call":
					out.ToolCallCount++
					if item.Type == "custom_tool_call" && isCodexSubagentToolName(item.Name) {
						out.SubagentCount++
						out.DirectSubagentCount++
					}
					if !codexToolInvocationOpen {
						codexModelInvocations++
						codexToolInvocationOpen = true
					}
				case "function_call_output", "custom_tool_call_output":
					codexToolInvocationOpen = false
				}
			}
		case "turn.completed":
			if provider == "codex" {
				var turn codexExecTurnCompleted
				if err := json.Unmarshal(line, &turn); err == nil {
					usage := turn.Usage
					if !usage.hasUsage() {
						usage = turn.Payload.Usage
					}
					if usage.hasUsage() {
						codexTokenCountMessages++
						finalOutputTokenCount = int64Ptr(usage.OutputTokens)
						finalReasoningTokenCount = int64Ptr(usage.reasoningTokens())
					}
				}
			}
		case "token_count":
			var tc codexTokenCount
			if err := json.Unmarshal(msg.Message, &tc); err == nil {
				usage, cumulative := tc.bestUsage()
				if usage.hasUsage() {
					codexTokenCountMessages++
					finalOutputTokenCount = int64Ptr(usage.OutputTokens)
					finalReasoningTokenCount = int64Ptr(usage.reasoningTokens())
					finalCodexUsageCumulative = cumulative
					if cumulative {
						codexTokenBaseline = &usage
					}
				}
			}
		}
	}
	if provider == "codex" {
		if codexModelInvocations > 0 {
			out.ParentModelCallCount = codexModelInvocations
		} else if codexTokenCountMessages > 0 {
			out.ParentModelCallCount = codexTokenCountMessages
		} else {
			out.ParentModelCallCount = codexAssistantMessages
		}
	}
	if provider == "codex" && finalCodexUsageCumulative && subtractCodexTokenBaseline != nil {
		if finalOutputTokenCount != nil {
			finalOutputTokenCount = int64Ptr(nonNegative(*finalOutputTokenCount - subtractCodexTokenBaseline.OutputTokens))
		}
		if finalReasoningTokenCount != nil {
			finalReasoningTokenCount = int64Ptr(nonNegative(*finalReasoningTokenCount - subtractCodexTokenBaseline.reasoningTokens()))
		}
	}
	if finalOutputTokenCount != nil {
		out.OutputTokenCount = finalOutputTokenCount
	}
	if finalReasoningTokenCount != nil {
		out.ReasoningTokenCount = finalReasoningTokenCount
	}
	if err := ctx.Err(); err != nil {
		return Counts{}, codexTokenBaseline, err
	}
	return out, codexTokenBaseline, scanner.Err()
}

func isCodexSubagentToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "spawn_agent", "dispatch_subagent", "subagent", "subagent_dispatch":
		return true
	default:
		return false
	}
}

func (tc codexTokenCount) bestUsage() (codexTokenCount, bool) {
	if usage, ok := tc.cumulativeUsage(); ok {
		return usage, true
	}
	if tc.Info != nil && tc.Info.LastTokenUsage != nil {
		return *tc.Info.LastTokenUsage, false
	}
	return tc, false
}

func (tc codexTokenCount) cumulativeUsage() (codexTokenCount, bool) {
	if tc.Info != nil && tc.Info.TotalTokenUsage != nil {
		return *tc.Info.TotalTokenUsage, true
	}
	return codexTokenCount{}, false
}

func (tc codexTokenCount) hasUsage() bool {
	return tc.InputTokens != 0 ||
		tc.CachedInputTokens != 0 ||
		tc.OutputTokens != 0 ||
		tc.ReasoningTokens != 0 ||
		tc.ReasoningOutputTokens != 0 ||
		tc.TotalTokens != 0
}

func (tc codexTokenCount) reasoningTokens() int64 {
	if tc.ReasoningOutputTokens != 0 {
		return tc.ReasoningOutputTokens
	}
	return tc.ReasoningTokens
}

func timestamps(path string) (time.Time, time.Time, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	scanner := newJSONLScanner(f)
	var first, last time.Time
	for scanner.Scan() {
		var line genericLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || line.Timestamp == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, line.Timestamp)
		if err != nil {
			continue
		}
		if first.IsZero() {
			first = ts
		}
		last = ts
	}
	if err := scanner.Err(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if first.IsZero() || last.IsZero() {
		return time.Time{}, time.Time{}, errors.New("no parseable timestamps")
	}
	return first, last, nil
}

func newJSONLScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	return scanner
}

func addPtr(dst **int64, value int64) {
	if *dst == nil {
		v := int64(0)
		*dst = &v
	}
	**dst += value
}

func int64Ptr(value int64) *int64 {
	v := value
	return &v
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func countClaudeToolUseBlocks(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return 0
	}
	var count int64
	for _, block := range blocks {
		if block.Type == "tool_use" {
			count++
		}
	}
	return count
}

func RedactedLineError(path string, line int, err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d: %T", path, line, err)
}
