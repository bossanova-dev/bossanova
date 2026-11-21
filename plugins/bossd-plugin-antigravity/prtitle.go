package main

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// suggestPRTitleTimeout stays under bossd's 30-second plugin RPC budget. agy
// spends 5 to 8 seconds starting before the model sees the prompt.
const suggestPRTitleTimeout = 28 * time.Second

const suggestedTitleMaxLen = 80

// suggestPRTitle asks agy for a title in plan mode, which makes no edits. The
// prompt rides stdin as a stream-json turn, like a headless run.
func (r *Runner) suggestPRTitle(ctx context.Context, req *bossanovav1.SuggestPRTitleRequest) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, suggestPRTitleTimeout)
	defer cancel()
	input, err := encodeStreamTurn(buildPRTitlePrompt(req))
	if err != nil {
		return "", err
	}
	plan := launchConfig{model: r.model, mode: "plan"}
	args := append([]string{"--input-format", "stream-json", "--output-format", "stream-json"}, plan.sessionFlags("", "", req.GetWorkDir())...)
	out, err := r.runAgy(ctx, req.GetWorkDir(), args, input, req.GetExtraEnv())
	if err != nil {
		return "", err
	}
	result, ok := lastResult(out)
	if !ok || result.Result.Status != "SUCCESS" {
		return "", errors.New("agy returned no successful result")
	}
	return sanitizeSuggestedTitle(result.Result.Response), nil
}

func buildPRTitlePrompt(req *bossanovav1.SuggestPRTitleRequest) string {
	orNone := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "(none)"
		}
		return s
	}
	var b strings.Builder
	b.WriteString("You are titling a pull request. Output ONLY the pull request title on a single line: the title text itself and nothing else. ")
	b.WriteString("No quotes, no markdown, no preamble, and no sentence that comments on the title. Do not use any tools.\n\n")
	b.WriteString("When the CURRENT TITLE below already describes the whole change well, repeat it back exactly. ")
	b.WriteString("Otherwise write a new concise title (at most 70 characters) that summarizes the whole change, not just the last commit.\n\n")
	b.WriteString("CURRENT TITLE:\n" + orNone(req.GetCurrentTitle()) + "\n\n")
	b.WriteString("BASE BRANCH: " + orNone(req.GetBaseBranch()) + "\n\n")
	b.WriteString("PR DESCRIPTION:\n" + orNone(req.GetPrBody()) + "\n\n")
	b.WriteString("COMMITS (oldest to newest):\n" + orNone(req.GetGitLog()) + "\n")
	return b.String()
}

// sanitizeSuggestedTitle keeps the first non-empty line, strips quoting, and
// rejects commentary about the title instead of a title.
func sanitizeSuggestedTitle(out string) string {
	var line string
	for l := range strings.SplitSeq(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	line = strings.TrimSpace(strings.Trim(line, "\"'`*#"))
	lower := strings.ToLower(line)
	for _, prefix := range []string{"the current title", "the title", "current title", "this title"} {
		if strings.HasPrefix(lower, prefix) {
			return ""
		}
	}
	for _, commentary := range []string{"already describes", "accurately describes", "already fits", "already accurate"} {
		if strings.Contains(lower, commentary) {
			return ""
		}
	}
	if utf8.RuneCountInString(line) > suggestedTitleMaxLen {
		line = strings.TrimSpace(string([]rune(line)[:suggestedTitleMaxLen]))
	}
	return line
}
