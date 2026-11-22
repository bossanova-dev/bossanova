package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/recurser/bossalib/contract/plugin/jsonlscan"
	"github.com/recurser/bossalib/contract/plugin/userturn"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stepObjectMax caps a step's object in bytes; a long command keeps its start.
const stepObjectMax = 160

// activityItem is an item_completed item as the structured read needs it.
type activityItem struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Command   []string `json:"command"`
	ParsedCmd []struct {
		Type  string `json:"type"`
		Cmd   string `json:"cmd"`
		Path  string `json:"path"`
		Query string `json:"query"`
	} `json:"parsed_cmd"`
	Status    string                `json:"status"`
	ExitCode  *int                  `json:"exit_code"`
	Changes   map[string]fileChange `json:"changes"`
	Server    string                `json:"server"`
	Tool      string                `json:"tool"`
	Kind      string                `json:"kind"`
	AgentPath string                `json:"agent_path"`
}

// parseRolloutActivity reads a rollout as the conversation a person reads:
// what was said, each command, file change and tool call as a step, and
// compactions as events, with harness wrappers removed from user turns. Steps
// come from item_completed items (codex 0.160 onward); an older rollout's
// legacy events carry no tool items, so it answers its turns alone. The other
// returns follow parseRolloutMessages.
func parseRolloutActivity(path, workDir string) ([]*bossanovav1.ChatMessage, string, int, string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, "", 0, "", err
	}
	defer func() { _ = f.Close() }()

	var legacy, items []*bossanovav1.ChatMessage
	var errs turnErrors
	scanner := jsonlscan.New(f)
	for scanner.Scan() {
		var env codexEnvelope
		if json.Unmarshal(scanner.Bytes(), &env) != nil || env.Type != "event_msg" {
			continue
		}
		var p codexEventMsgPayload
		if json.Unmarshal(env.Payload, &p) != nil {
			continue
		}
		typ, message := p.turn()
		errs.observe(p, typ, message)
		if p.Type == "item_completed" {
			var wrapped struct {
				Item activityItem `json:"item"`
			}
			if json.Unmarshal(env.Payload, &wrapped) == nil {
				items = append(items, itemMessages(wrapped.Item, env.Timestamp, workDir)...)
			}
			continue
		}
		switch typ {
		case "user_message":
			legacy = append(legacy, userMessages(message, env.Timestamp)...)
		case "agent_message":
			legacy = append(legacy, assistantText(message, env.Timestamp)...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", scanner.Skipped(), "", err
	}
	msgs := items
	if len(legacy) > 0 {
		msgs = legacy
	}
	return msgs, finalAssistantText(msgs), scanner.Skipped(), errs.last, nil
}

func itemMessages(it activityItem, at, workDir string) []*bossanovav1.ChatMessage {
	switch it.Type {
	case "UserMessage":
		return userMessages(itemText(it), at)
	case "AgentMessage":
		return assistantText(itemText(it), at)
	case "CommandExecution":
		failed := it.Status == "failed" || (it.ExitCode != nil && *it.ExitCode != 0)
		step := commandStep(it, workDir)
		step.Result = exitResult(it.ExitCode)
		return []*bossanovav1.ChatMessage{stepMessage(step, failed, at)}
	case "FileChange":
		return fileChangeSteps(it, at, workDir)
	case "McpToolCall":
		return []*bossanovav1.ChatMessage{stepMessage(&bossanovav1.ChatStep{Verb: "Called", Object: it.Server + " " + it.Tool}, it.Status == "failed", at)}
	case "SubAgentActivity":
		if it.Kind == "started" {
			return []*bossanovav1.ChatMessage{stepMessage(&bossanovav1.ChatStep{Verb: "Started an agent", Object: it.AgentPath}, false, at)}
		}
	case "ContextCompaction":
		return []*bossanovav1.ChatMessage{eventMessage(userturn.Notice{Source: userturn.SourceAgent, Text: "Conversation compacted"}, at)}
	}
	return nil
}

func itemText(it activityItem) string {
	var parts []string
	for _, c := range it.Content {
		if strings.EqualFold(c.Type, "text") && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func userMessages(text, at string) []*bossanovav1.ChatMessage {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	turn := userturn.Classify(text)
	switch turn.Kind {
	case userturn.Message:
		return []*bossanovav1.ChatMessage{{Role: "user", Kind: "text", Text: turn.Text, Timestamp: at}}
	case userturn.Command:
		return []*bossanovav1.ChatMessage{{Role: "user", Kind: "command", Text: turn.Text, Timestamp: at}}
	case userturn.Event:
		return []*bossanovav1.ChatMessage{eventMessage(turn.Event, at)}
	case userturn.Harness:
	}
	return nil
}

func assistantText(text, at string) []*bossanovav1.ChatMessage {
	if text = strings.TrimSpace(text); text == "" {
		return nil
	}
	return []*bossanovav1.ChatMessage{{Role: "assistant", Kind: "text", Text: text, Timestamp: at}}
}

// commandStep names a command by what codex parsed it as: a file read, a
// search or a listing, else the command it ran.
func commandStep(it activityItem, workDir string) *bossanovav1.ChatStep {
	if len(it.ParsedCmd) == 1 {
		pc := it.ParsedCmd[0]
		switch pc.Type {
		case "read":
			return &bossanovav1.ChatStep{Verb: "Read", Object: clip(relPath(pc.Path, workDir))}
		case "search":
			return &bossanovav1.ChatStep{Verb: "Searched", Object: clip(pc.Query)}
		case "list_files":
			return &bossanovav1.ChatStep{Verb: "Listed", Object: clip(pc.Cmd)}
		}
	}
	return &bossanovav1.ChatStep{Verb: "Ran", Object: clip(shellCommand(it.Command))}
}

// shellCommand is the command a `<shell> -lc <command>` argv runs, else the argv.
func shellCommand(argv []string) string {
	if n := len(argv); n == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
		return firstLine(argv[2])
	}
	return firstLine(strings.Join(argv, " "))
}

var changeVerbs = map[string]string{"add": "Added", "delete": "Deleted", "update": "Edited"}

func fileChangeSteps(it activityItem, at, workDir string) []*bossanovav1.ChatMessage {
	paths := make([]string, 0, len(it.Changes))
	for p := range it.Changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]*bossanovav1.ChatMessage, 0, len(paths))
	failed := it.Status == "failed"
	for _, p := range paths {
		change := it.Changes[p]
		verb, ok := changeVerbs[change.Type]
		if !ok {
			verb = "Changed"
		}
		step := &bossanovav1.ChatStep{Verb: verb, Object: clip(relPath(p, workDir))}
		if !failed {
			step.Result = change.size()
		}
		out = append(out, stepMessage(step, failed, at))
	}
	return out
}

func stepMessage(step *bossanovav1.ChatStep, failed bool, at string) *bossanovav1.ChatMessage {
	step.IsError = failed
	text := strings.TrimSpace(step.GetVerb() + " " + step.GetObject())
	return &bossanovav1.ChatMessage{Role: "assistant", Kind: "step", Text: text, Timestamp: at, Step: step}
}

func eventMessage(n userturn.Notice, at string) *bossanovav1.ChatMessage {
	return &bossanovav1.ChatMessage{
		Role: "system", Kind: "event", Text: n.Text, Timestamp: at,
		Event: &bossanovav1.ChatEvent{Source: n.Source, Text: n.Text, Url: n.URL},
	}
}

func finalAssistantText(msgs []*bossanovav1.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].GetRole() == "assistant" && msgs[i].GetKind() == "text" {
			return msgs[i].GetText()
		}
	}
	return ""
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
