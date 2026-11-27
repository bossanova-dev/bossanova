package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestLiveUnattendedPolicy proves P-settings and P-anchor against the real
// installed Claude CLI with --setting-sources user, --strict-mcp-config,
// --settings <generated>, and --permission-mode acceptEdits.
//
// Opt-in (CLAUDE_LIVE=1) and self-skipping when claude is absent or when the
// operator's own ~/.claude/settings.json already bypasses permissions or allows
// unrestricted Bash.
func TestLiveUnattendedPolicy(t *testing.T) {
	if os.Getenv("CLAUDE_LIVE") != "1" {
		t.Skip("CLAUDE_LIVE!=1 — skipping live claude unattended policy validation (opt-in)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH — skipping live validation")
	}
	skipIfUserSettingsVacuous(t)

	worktree := t.TempDir()
	logDir := t.TempDir()
	outsideDir := t.TempDir()
	binDir := t.TempDir()

	// Initialize a real temp git repo as the worktree with commit signing disabled.
	notePath := filepath.Join(worktree, "note.txt")
	if err := os.WriteFile(notePath, []byte("before-edit\n"), 0o600); err != nil {
		t.Fatalf("write note.txt: %v", err)
	}
	runGitFixture(t, worktree, "init")
	runGitFixture(t, worktree, "config", "commit.gpgsign", "false")
	runGitFixture(t, worktree, "config", "user.name", "Bossanova Live Test")
	runGitFixture(t, worktree, "config", "user.email", "live-test@bossanova.invalid")
	runGitFixture(t, worktree, "add", "note.txt")
	runGitFixture(t, worktree, "commit", "-m", "initial")

	// Create a uniquely named executable probe script in binDir that cannot
	// match any pre-existing user permission rule.
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		t.Fatalf("random probe suffix: %v", err)
	}
	probeName := "bossprobe-" + hex.EncodeToString(randBytes[:])
	probeMarker := "PROBE_OK_" + probeName
	probeScript := fmt.Sprintf("#!/bin/sh\necho %s\n", probeMarker)
	if err := os.WriteFile(filepath.Join(binDir, probeName), []byte(probeScript), 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}

	policy := &bossanovav1.UnattendedPermissionPolicy{
		WorktreePath:           worktree,
		AllowedCommandPrefixes: []string{"git", probeName},
		IsNetworkAllowed:       true,
	}
	logPath := filepath.Join(logDir, "claude-live.log")
	written, err := writeUnattendedPolicySettings(policy, worktree, logPath, "live-unattended")
	if err != nil {
		t.Fatalf("writeUnattendedPolicySettings: %v", err)
	}
	defer written.cleanup()
	settingsPath := written.path

	rawSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read rendered settings: %v", err)
	}

	// Render the exact untrusted headless argv through Runner.buildArgv.
	model := strings.TrimSpace(os.Getenv("CLAUDE_LIVE_MODEL"))
	if model == "" {
		model = "haiku"
	}
	runner := NewRunner(zerolog.Nop(), WithEffort("low"))
	argv := runner.buildArgv(agentruntime.BuildArgvInput{
		ShouldTrustProjectConfig: false,
		WorkDir:                  worktree,
		LogPath:                  logPath,
		Options: map[string]string{
			unattendedPolicySettingsOpt: settingsPath,
			"model":                     model,
		},
	})

	outsideFile := filepath.Join(outsideDir, "denied-outside.txt")
	prompt := fmt.Sprintf(
		"Execute these four tool calls directly and in order without asking questions:\n"+
			"1. Use Bash to run: %s\n"+
			"2. Use Bash to run: git status\n"+
			"3. Use Edit on %s to replace the exact string before-edit with after-edit.\n"+
			"4. Attempt to use Write to create %s with content denied-outside (try the Write tool call even if permissions might block it).\n"+
			"After all four calls, reply with a single short line summarizing the four results.\n",
		probeName, notePath, outsideFile,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = worktree
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = prependPathEnv(os.Environ(), binDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 30 * time.Second

	runErr := cmd.Run()
	if runErr != nil {
		t.Fatalf("live claude failed: %v\nstderr:\n%s\nstdout:\n%s", runErr, stderr.String(), stdout.String())
	}

	redactor := makePathRedactor(worktree, outsideDir, probeName)
	t.Logf("rendered settings (redacted):\n%s", redactor(string(rawSettings)))

	obs := parseClaudeStreamJSON(t, stdout.Bytes())

	// Case (a): probe script ran via Bash and emitted its unique marker.
	probeCall, ok := obs.findToolCall("Bash", func(in map[string]any) bool {
		cmdStr, _ := in["command"].(string)
		return strings.Contains(cmdStr, probeName)
	})
	if !ok {
		t.Fatalf("case (a): no Bash tool call for %s found in stream-json\nstdout:\n%s", probeName, stdout.String())
	}
	if probeCall.isError || !strings.Contains(probeCall.resultText, probeMarker) {
		t.Fatalf("case (a): probe Bash call failed or missed marker: is_error=%v result=%q", probeCall.isError, probeCall.resultText)
	}
	t.Logf("case (a) probe: tool=%s input=%s is_error=%v result=%q",
		probeCall.name, redactor(probeCall.inputJSON), probeCall.isError, redactor(truncateLog(probeCall.resultText)))

	// Case (b): git status ran via Bash without error.
	gitCall, ok := obs.findToolCall("Bash", func(in map[string]any) bool {
		cmdStr, _ := in["command"].(string)
		return strings.Contains(cmdStr, "git status")
	})
	if !ok {
		t.Fatalf("case (b): no Bash tool call for git status found in stream-json")
	}
	if gitCall.isError {
		t.Fatalf("case (b): git status Bash call failed: result=%q", gitCall.resultText)
	}
	t.Logf("case (b) git status: tool=%s input=%s is_error=%v result=%q",
		gitCall.name, redactor(gitCall.inputJSON), gitCall.isError, redactor(truncateLog(gitCall.resultText)))

	// Case (c): Edit inside the worktree succeeded in stream-json AND modified note.txt on disk.
	editCall, ok := obs.findToolCall("Edit", func(in map[string]any) bool {
		fp, _ := in["file_path"].(string)
		return strings.HasSuffix(fp, "note.txt")
	})
	if !ok {
		t.Fatalf("case (c): no Edit tool call for note.txt found in stream-json")
	}
	if editCall.isError {
		t.Fatalf("case (c): Edit inside worktree was rejected: result=%q", editCall.resultText)
	}
	editedBytes, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatalf("case (c): read note.txt: %v", err)
	}
	if !strings.Contains(string(editedBytes), "after-edit") || strings.Contains(string(editedBytes), "before-edit") {
		t.Fatalf("case (c): note.txt on disk = %q, want after-edit", string(editedBytes))
	}
	t.Logf("case (c) edit inside worktree: tool=%s input=%s is_error=%v result=%q disk=%q",
		editCall.name, redactor(editCall.inputJSON), editCall.isError, redactor(truncateLog(editCall.resultText)), strings.TrimSpace(string(editedBytes)))

	// Case (d): Write outside the worktree was attempted AND denied, and the file does not exist on disk.
	writeCall, ok := obs.findToolCall("Write", func(in map[string]any) bool {
		fp, _ := in["file_path"].(string)
		return strings.Contains(fp, "denied-outside.txt")
	})
	if !ok {
		t.Fatalf("case (d): no Write tool call targeting %s found in stream-json\nstdout:\n%s", outsideFile, stdout.String())
	}
	if _, statErr := os.Stat(outsideFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("case (d): outside file %s must not exist on disk (statErr=%v)", outsideFile, statErr)
	}
	if !writeCall.isError && !obs.hasPermissionDenial("Write", writeCall.id) {
		t.Fatalf("case (d): Write outside worktree was not denied: is_error=%v result=%q denials=%v",
			writeCall.isError, writeCall.resultText, obs.permissionDenials)
	}
	t.Logf("case (d) write outside worktree: tool=%s input=%s is_error=%v result=%q denied_on_disk=true",
		writeCall.name, redactor(writeCall.inputJSON), writeCall.isError, redactor(truncateLog(writeCall.resultText)))
}

func skipIfUserSettingsVacuous(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return
	}
	var cfg struct {
		Permissions struct {
			DefaultMode string   `json:"defaultMode"`
			Allow       []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return
	}
	if cfg.Permissions.DefaultMode == "bypassPermissions" {
		t.Skip("user ~/.claude/settings.json sets permissions.defaultMode=bypassPermissions; live policy test would be vacuous")
	}
	for _, rule := range cfg.Permissions.Allow {
		r := strings.TrimSpace(rule)
		if r == "Bash" || r == "Bash(*)" || r == "Bash(:*)" || r == "*" {
			t.Skipf("user ~/.claude/settings.json allows %q; live policy test would be vacuous", r)
		}
	}
}

func runGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out.String())
	}
}

func prependPathEnv(environ []string, binDir string) []string {
	out := make([]string, 0, len(environ)+1)
	replaced := false
	for _, kv := range environ {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+binDir+string(os.PathListSeparator)+strings.TrimPrefix(kv, "PATH="))
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "PATH="+binDir)
	}
	return out
}

type observedToolCall struct {
	id         string
	name       string
	input      map[string]any
	inputJSON  string
	isError    bool
	resultText string
	hasResult  bool
}

type streamJSONObservations struct {
	calls             []*observedToolCall
	byID              map[string]*observedToolCall
	permissionDenials []string
}

func (o *streamJSONObservations) findToolCall(name string, match func(map[string]any) bool) (*observedToolCall, bool) {
	for _, c := range o.calls {
		if c.name == name && match(c.input) {
			return c, true
		}
	}
	return nil, false
}

func (o *streamJSONObservations) hasPermissionDenial(toolName, toolUseID string) bool {
	for _, d := range o.permissionDenials {
		if d == toolUseID || d == toolName {
			return true
		}
	}
	return false
}

func parseClaudeStreamJSON(t *testing.T, raw []byte) *streamJSONObservations {
	t.Helper()
	obs := &streamJSONObservations{byID: make(map[string]*observedToolCall)}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var env struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
					ToolUseID string          `json:"tool_use_id"`
					IsError   bool            `json:"is_error"`
					Content   json.RawMessage `json:"content"`
				} `json:"content"`
			} `json:"message"`
			PermissionDenials []struct {
				ToolName  string `json:"tool_name"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"permission_denials"`
		}
		if err := json.Unmarshal(trimmed, &env); err != nil {
			continue
		}
		for _, block := range env.Message.Content {
			switch block.Type {
			case "tool_use":
				inMap := make(map[string]any)
				_ = json.Unmarshal(block.Input, &inMap)
				call := &observedToolCall{
					id:        block.ID,
					name:      block.Name,
					input:     inMap,
					inputJSON: string(block.Input),
				}
				obs.calls = append(obs.calls, call)
				obs.byID[block.ID] = call
			case "tool_result":
				if call, ok := obs.byID[block.ToolUseID]; ok {
					call.isError = block.IsError
					call.resultText = extractToolResultText(block.Content)
					call.hasResult = true
				}
			}
		}
		for _, denial := range env.PermissionDenials {
			if denial.ToolUseID != "" {
				obs.permissionDenials = append(obs.permissionDenials, denial.ToolUseID)
			}
			if denial.ToolName != "" {
				obs.permissionDenials = append(obs.permissionDenials, denial.ToolName)
			}
		}
	}
	return obs
}

func extractToolResultText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil && len(parts) > 0 {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

func makePathRedactor(worktree, outsideDir, probeName string) func(string) string {
	var repl []string
	wtRoots := policyWorktreeRoots(worktree)
	for i, root := range wtRoots {
		placeholder := "<WORKTREE>"
		if i > 0 {
			placeholder = "<WORKTREE_REALPATH>"
		}
		trimmed := strings.TrimPrefix(filepath.ToSlash(root), "/")
		repl = append(repl, "//"+trimmed, "//"+placeholder, root, placeholder)
	}
	for _, root := range policyWorktreeRoots(outsideDir) {
		repl = append(repl, root, "<OUTSIDE>")
	}
	repl = append(repl, probeName, "bossprobe-<rand>")
	r := strings.NewReplacer(repl...)
	return r.Replace
}

const maxLiveLogSnippet = 200

func truncateLog(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " | "))
	if len(s) <= maxLiveLogSnippet {
		return s
	}
	return s[:maxLiveLogSnippet] + "…"
}
