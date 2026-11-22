package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stepObjectMax caps a step's object in bytes; a long command or prompt keeps
// its start.
const stepObjectMax = 160

// toolInput is the union of the input fields Claude Code's tools carry that a
// step names.
type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Command      string `json:"command"`
	Description  string `json:"description"`
	Pattern      string `json:"pattern"`
	URL          string `json:"url"`
	Query        string `json:"query"`
	Skill        string `json:"skill"`
}

// stepVerbs names each Claude Code tool for a person, and which input field is
// its object.
var stepVerbs = map[string]struct {
	verb   string
	object func(toolInput, string) string
}{
	"Read":         {"Read", filePath},
	"Edit":         {"Edited", filePath},
	"MultiEdit":    {"Edited", filePath},
	"Write":        {"Wrote", filePath},
	"NotebookEdit": {"Edited", func(in toolInput, dir string) string { return relPath(in.NotebookPath, dir) }},
	"Bash":         {"Ran", func(in toolInput, _ string) string { return firstLine(in.Command) }},
	"Grep":         {"Searched", func(in toolInput, _ string) string { return in.Pattern }},
	"Glob":         {"Listed", func(in toolInput, _ string) string { return in.Pattern }},
	"Task":         {"Ran an agent", func(in toolInput, _ string) string { return in.Description }},
	"Agent":        {"Ran an agent", func(in toolInput, _ string) string { return in.Description }},
	"WebFetch":     {"Fetched", func(in toolInput, _ string) string { return in.URL }},
	"WebSearch":    {"Searched the web for", func(in toolInput, _ string) string { return in.Query }},
	"Skill":        {"Loaded", func(in toolInput, _ string) string { return in.Skill }},
	"TodoWrite":    {"Updated", func(toolInput, string) string { return "the todo list" }},
	"ExitPlanMode": {"Proposed", func(toolInput, string) string { return "a plan" }},
}

// stepFor names one tool call: the verb a person would use and what it acted
// on, with paths relative to the worktree.
func stepFor(name string, raw json.RawMessage, workDir string) *bossanovav1.ChatStep {
	var in toolInput
	_ = json.Unmarshal(raw, &in)
	if known, ok := stepVerbs[name]; ok {
		return &bossanovav1.ChatStep{Verb: known.verb, Object: clip(known.object(in, workDir))}
	}
	// mcp__<server>__<tool>
	if parts := strings.SplitN(name, "__", 3); len(parts) == 3 && parts[0] == "mcp" {
		return &bossanovav1.ChatStep{Verb: "Called", Object: parts[1] + " " + parts[2]}
	}
	return &bossanovav1.ChatStep{Verb: "Used", Object: name}
}

func stepText(step *bossanovav1.ChatStep) string {
	return strings.TrimSpace(step.GetVerb() + " " + step.GetObject())
}

func filePath(in toolInput, workDir string) string {
	return relPath(in.FilePath, workDir)
}

func relPath(path, workDir string) string {
	if workDir == "" || path == "" {
		return path
	}
	if rel, err := filepath.Rel(filepath.Clean(workDir), path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

// clip keeps the start of an over-long object, cut on a rune boundary.
func clip(s string) string {
	if len(s) <= stepObjectMax {
		return s
	}
	cut := stepObjectMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
