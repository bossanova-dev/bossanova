package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/contract/plugin/userturn"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// activityLine is a Claude Code transcript line as the structured read needs it.
type activityLine struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	PRNumber      int             `json:"prNumber"`
	PRURL         string          `json:"prUrl"`
	PRRepository  string          `json:"prRepository"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Attachment    struct {
		Type   string `json:"type"`
		Prompt string `json:"prompt"`
		Origin struct {
			Kind string `json:"kind"`
		} `json:"origin"`
	} `json:"attachment"`
}

// contentBlock is one block of a message's content array.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// activity builds the structured conversation one transcript line at a time.
// Tool results arrive lines after their call, so it keeps each open call by
// id and settles it when the result comes. Claude Code records the session's
// PR again on nearly every turn, so it keeps the PR it last showed as linked
// (repo#number) and shows a link only when the PR changes.
type activity struct {
	workDir  string
	messages []*bossanovav1.ChatMessage
	calls    map[string]openCall
	linked   string
}

// openCall is a tool call still waiting on its result: its message, and the
// tool whose record the result carries.
type openCall struct {
	msg  *bossanovav1.ChatMessage
	tool string
}

// readActivity reads the transcript at path as the conversation a person
// reads: what was said, each tool call as a step, notices as events, a link
// each time the session's PR changes, and a question still waiting on its
// answer. Harness wrappers never appear as
// user text. maxMessages and the final assistant text follow readTranscript.
func readActivity(path, workDir string, maxMessages int32) (messages []*bossanovav1.ChatMessage, finalAssistant string, exists bool, err error) {
	a := &activity{workDir: workDir, calls: map[string]openCall{}}
	exists, err = scanTranscript(path, a.add)
	if !exists || err != nil {
		return nil, "", exists, err
	}
	messages, finalAssistant = tailTranscript(a.messages, maxMessages)
	return messages, finalAssistant, true, nil
}

func (a *activity) add(raw []byte) {
	var line activityLine
	if json.Unmarshal(raw, &line) != nil || line.IsSidechain || line.IsMeta {
		return
	}
	switch line.Type {
	case "user":
		if line.Message.Role == "user" {
			a.addUser(line)
		}
	case "assistant":
		a.addAssistant(line)
	case "attachment":
		a.addQueued(line)
	case "pr-link":
		a.prLink(line)
	}
}

// prLink shows a pr-link record as a GitHub event when it names a PR other
// than the one last shown. A record with no PR number shows nothing and
// changes nothing. The web Chats view folds repeats of this "Linked " text the
// same way for older plugins (Folder.event in
// services/web/src/features/chat/lib/transcript.ts).
func (a *activity) prLink(line activityLine) {
	if line.PRNumber <= 0 {
		return
	}
	pr := fmt.Sprintf("%s#%d", line.PRRepository, line.PRNumber)
	if pr == a.linked {
		return
	}
	a.linked = pr
	a.event(line.Timestamp, userturn.Notice{
		Source: userturn.SourceGitHub,
		Text:   "Linked " + pr,
		URL:    line.PRURL,
	})
}

func (a *activity) addUser(line activityLine) {
	if line.IsCompactSummary {
		a.event(line.Timestamp, userturn.Notice{Source: userturn.SourceAgent, Text: "Conversation compacted"})
		return
	}
	var text string
	if json.Unmarshal(line.Message.Content, &text) != nil {
		var blocks []contentBlock
		if json.Unmarshal(line.Message.Content, &blocks) != nil {
			return
		}
		record := soleResultRecord(line.ToolUseResult, blocks)
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				a.settle(b, record)
			case "text":
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, "\n")
	}
	a.userTurn(text, line.Timestamp, true)
}

// addQueued reads what arrived while the agent was mid-turn. Claude Code
// records it as a queued_command attachment rather than a user line: a
// person's message with origin "human", or a background task's notice.
func (a *activity) addQueued(line activityLine) {
	if line.Attachment.Type != "queued_command" {
		return
	}
	a.userTurn(line.Attachment.Prompt, line.Timestamp, line.Attachment.Origin.Kind == "human")
}

// userTurn places text the agent received: a person's message or command, or
// a notice as an event. Text not from a person is never shown as theirs.
func (a *activity) userTurn(text, timestamp string, fromPerson bool) {
	if strings.TrimSpace(text) == "" {
		return
	}
	turn := userturn.Classify(text)
	switch turn.Kind {
	case userturn.Message:
		if fromPerson {
			a.push(&bossanovav1.ChatMessage{Role: "user", Kind: "text", Text: turn.Text, Timestamp: timestamp})
		}
	case userturn.Command:
		if fromPerson {
			a.push(&bossanovav1.ChatMessage{Role: "user", Kind: "command", Text: turn.Text, Timestamp: timestamp})
		}
	case userturn.Event:
		a.event(timestamp, turn.Event)
	case userturn.Harness:
	}
}

func (a *activity) addAssistant(line activityLine) {
	var blocks []contentBlock
	if json.Unmarshal(line.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if text := strings.TrimSpace(b.Text); text != "" {
				a.push(&bossanovav1.ChatMessage{Role: "assistant", Kind: "text", Text: text, Timestamp: line.Timestamp})
			}
		case "tool_use":
			msg := a.call(b, line.Timestamp)
			a.calls[b.ID] = openCall{msg: msg, tool: b.Name}
			a.push(msg)
		}
	}
}

// call is a tool call's message: a pending question for the agent's question
// tool, a step for everything else.
func (a *activity) call(b contentBlock, timestamp string) *bossanovav1.ChatMessage {
	if q := askedQuestion(b); q != nil {
		return &bossanovav1.ChatMessage{Role: "assistant", Kind: "question", Text: q.GetText(), Timestamp: timestamp, Question: q}
	}
	step := stepFor(b.Name, b.Input, a.workDir)
	return &bossanovav1.ChatMessage{Role: "assistant", Kind: "step", Text: stepText(step), Timestamp: timestamp, Step: step}
}

// settle records a call's result: a failure marks its step, the result's
// record summarizes it, and an answer turns a question into the step that
// asked it.
func (a *activity) settle(result contentBlock, record json.RawMessage) {
	open, ok := a.calls[result.ToolUseID]
	if !ok {
		return
	}
	delete(a.calls, result.ToolUseID)
	msg := open.msg
	if msg.Kind == "question" {
		msg.Kind = "step"
		msg.Step = &bossanovav1.ChatStep{Verb: "Asked", Object: msg.GetQuestion().GetText()}
		msg.Text = stepText(msg.Step)
		msg.Question = nil
	}
	if result.IsError {
		msg.Step.IsError = true
	}
	msg.Step.Result = resultSummary(open.tool, record, result)
}

func (a *activity) event(timestamp string, n userturn.Notice) {
	a.push(&bossanovav1.ChatMessage{
		Role: "system", Kind: "event", Text: n.Text, Timestamp: timestamp,
		Event: &bossanovav1.ChatEvent{Source: n.Source, Text: n.Text, Url: n.URL},
	})
}

func (a *activity) push(msg *bossanovav1.ChatMessage) {
	a.messages = append(a.messages, msg)
}

// askedQuestion reads the agent's question tool call: its first question, the
// labels of the answers it offers, the id its menu goes by (questionID), and
// whether the menu takes an answer from Bossanova.
func askedQuestion(b contentBlock) *bossanovav1.ChatQuestion {
	if b.Name != "AskUserQuestion" {
		return nil
	}
	input, ok := parseQuestionInput(b.Input)
	if !ok {
		return nil
	}
	first := input.Questions[0]
	q := &bossanovav1.ChatQuestion{Kind: questionKindQuestion, Text: strings.TrimSpace(first.Question)}
	for _, o := range first.Options {
		if label := strings.TrimSpace(o.Label); label != "" {
			q.Options = append(q.Options, label)
		}
	}
	q.Id = questionID(first.Header, q.GetText(), q.GetOptions())
	q.IsAnswerable = input.answerable()
	return q
}

// questionInput is the AskUserQuestion tool call's input.
type questionInput struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label string `json:"label"`
		} `json:"options"`
	} `json:"questions"`
}

func parseQuestionInput(raw json.RawMessage) (questionInput, bool) {
	var input questionInput
	if json.Unmarshal(raw, &input) != nil || len(input.Questions) == 0 {
		return questionInput{}, false
	}
	return input, true
}

// answerable reports whether the menu takes an answer as one key, or as its
// free-text row plus one line: a single question with a single choice, every
// option labelled, and few enough options that each, and the free-text row
// after them, is a digit key. Several questions page through tabs and several
// choices toggle boxes; those are answered in the terminal.
func (in questionInput) answerable() bool {
	if len(in.Questions) != 1 || in.Questions[0].MultiSelect {
		return false
	}
	opts := in.Questions[0].Options
	if len(opts) == 0 || len(opts)+1 > maxDigitMenuRow {
		return false
	}
	for _, o := range opts {
		if strings.TrimSpace(o.Label) == "" {
			return false
		}
	}
	return true
}
