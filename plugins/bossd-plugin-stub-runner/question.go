package main

import (
	"bytes"
	"os"
	"path/filepath"
)

// The stub asks the operator a question when its worktree carries a
// .stub-question file. The interactive pane prints the file behind a fixed
// prefix and then stays open, and HasQuestionPrompt recognises that prefix in
// the captured pane, so bossd's tmux poller reports the chat as QUESTION
// through the same pane-grammar path a real runner takes. A worktree without
// the file gets the plain `true` pane, which exits at once.
const (
	questionFileName = ".stub-question"
	// questionPrefix has no trailing space: tmux strips trailing blanks from a
	// captured line, so an empty question would otherwise never match.
	questionPrefix = "stub asks:"
)

// questionScript prints the question file (its path is $1) behind the prefix
// and keeps the pane open for a day; archiving or stopping the chat kills the
// pane before then.
const questionScript = "printf '" + questionPrefix + " '; cat \"$1\"; echo; exec sleep 86400"

func interactiveArgv(worktreePath string) []string {
	if argv := interactiveFixtureArgv(); argv != nil {
		return argv
	}
	if worktreePath == "" {
		return []string{"true"}
	}
	path := filepath.Join(worktreePath, questionFileName)
	if _, err := os.Stat(path); err != nil {
		return []string{"true"}
	}
	return []string{"sh", "-c", questionScript, "sh", path}
}

func hasQuestion(pane []byte) bool {
	for _, line := range bytes.Split(pane, []byte("\n")) {
		if bytes.HasPrefix(line, []byte(questionPrefix)) {
			return true
		}
	}
	return false
}
