package accountflow

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/agentcred"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func claudeToken() string { return "sk-ant-oat01-" + strings.Repeat("a", 40) }

func countSaid(lines []string, want string) int {
	count := 0
	for _, line := range lines {
		if line == want {
			count++
		}
	}
	return count
}

func TestRunClaudeAdd(t *testing.T) {
	t.Run("walkthrough_happy", func(t *testing.T) {
		tok := claudeToken()
		ex := &fakeExec{proc: newScriptedProc([]string{"Open https://claude.com/oauth and approve", tok}, nil)}
		pr := &fakePrompter{confirms: []bool{true}, answers: []string{"", ""}}
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Exec: ex, Prompter: pr, Client: cl}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
		req := cl.addReqs[0]
		if req.GetProvider() != "claude" {
			t.Fatalf("provider = %q", req.GetProvider())
		}
		if string(req.GetCredential()) != tok {
			t.Fatalf("credential blob = %q, want token", req.GetCredential())
		}
		if len(ex.extraEnv) == 0 || !strings.HasPrefix(ex.extraEnv[0], "CLAUDE_CONFIG_DIR=") {
			t.Fatalf("extraEnv = %v, want CLAUDE_CONFIG_DIR= prefix", ex.extraEnv)
		}
		if strings.Contains(pr.transcript(), tok) {
			t.Fatalf("raw token leaked into transcript:\n%s", pr.transcript())
		}
	})

	t.Run("walkthrough_no_token_in_output", func(t *testing.T) {
		ex := &fakeExec{proc: newScriptedProc([]string{"Visit https://claude.com/login and approve"}, nil)}
		pr := &fakePrompter{confirms: []bool{true}}
		cl := &fakeAccountClient{}
		err := RunClaudeAdd(context.Background(), ClaudeOptions{Exec: ex, Prompter: pr, Client: cl})
		if err == nil || !strings.Contains(err.Error(), "no setup token found") {
			t.Fatalf("err = %v, want 'no setup token found'", err)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("walkthrough_nonzero_exit", func(t *testing.T) {
		ex := &fakeExec{proc: newScriptedProc([]string{"step 1", "OAuth error: access_denied"}, errors.New("exit status 1"))}
		pr := &fakePrompter{confirms: []bool{true}}
		cl := &fakeAccountClient{}
		err := RunClaudeAdd(context.Background(), ClaudeOptions{Exec: ex, Prompter: pr, Client: cl})
		if err == nil || !strings.Contains(err.Error(), "access_denied") {
			t.Fatalf("err = %v, want access_denied", err)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("walkthrough_timeout", func(t *testing.T) {
		proc := newBlockingProc([]string{"waiting for approval"})
		ex := &fakeExec{proc: proc}
		pr := &fakePrompter{confirms: []bool{true}}
		cl := &fakeAccountClient{}
		err := RunClaudeAdd(context.Background(), ClaudeOptions{Exec: ex, Prompter: pr, Client: cl, Timeout: 50 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want timed out", err)
		}
		if !proc.wasKilled() {
			t.Fatalf("proc was not killed on timeout")
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("walkthrough_dedupes_claude_noise_and_preserves_unknown", func(t *testing.T) {
		tok := claudeToken()
		ex := &fakeExec{proc: newScriptedProc([]string{
			"\x1b[2KWelcome to Claude Code v1.2.3\r",
			"Welcome to Claude Code v1.2.3",
			"Opening browser to sign in...",
			"Opening browser to sign in...",
			"Error opening browser: permission denied",
			"Opening browser failed: permission denied",
			"Non-token warning: browser launch failed once",
			"Non-token warning: browser launch failed once",
			tok,
			tok,
		}, nil)}
		pr := &fakePrompter{confirms: []bool{true}, answers: []string{"", ""}}
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Exec: ex, Prompter: pr, Client: cl}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		transcript := pr.transcript()
		if strings.Contains(transcript, tok) {
			t.Fatalf("raw token leaked into transcript:\n%s", transcript)
		}
		if got := strings.Count(strings.Join(pr.said, "\n"), "Welcome to Claude Code"); got > 1 {
			t.Fatalf("welcome banner emitted %d times, want at most once:\n%v", got, pr.said)
		}
		if got := countSaid(pr.said, "Opening browser for Claude sign-in..."); got > 1 {
			t.Fatalf("opening-browser notice emitted %d times, want at most once:\n%v", got, pr.said)
		}
		if !strings.Contains(transcript, "Error opening browser: permission denied") {
			t.Fatalf("opening-browser error was swallowed:\n%s", transcript)
		}
		if !strings.Contains(transcript, "Opening browser failed: permission denied") {
			t.Fatalf("opening-browser failure was swallowed:\n%s", transcript)
		}
		if !strings.Contains(transcript, "Non-token warning: browser launch failed once") {
			t.Fatalf("unknown warning was swallowed:\n%s", transcript)
		}
		for i := 1; i < len(pr.said); i++ {
			if pr.said[i] == pr.said[i-1] {
				t.Fatalf("consecutive duplicate emitted at %d: %q\n%v", i, pr.said[i], pr.said)
			}
		}
	})

	t.Run("walkthrough_collapses_only_consecutive_unknown_duplicates", func(t *testing.T) {
		tok := claudeToken()
		line := "Non-token warning: proxy retry"
		ex := &fakeExec{proc: newScriptedProc([]string{
			line,
			line,
			"different status line",
			line,
			tok,
		}, nil)}
		pr := &fakePrompter{confirms: []bool{true}, answers: []string{"", ""}}
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Exec: ex, Prompter: pr, Client: cl}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if got := strings.Count(strings.Join(pr.said, "\n"), line); got != 2 {
			t.Fatalf("unknown line emitted %d times, want consecutive-only dedupe to leave 2:\n%v", got, pr.said)
		}
	})

	t.Run("paste_happy", func(t *testing.T) {
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok, "", ""}}
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 || string(cl.addReqs[0].GetCredential()) != tok {
			t.Fatalf("AddAccount not called with token: %+v", cl.addReqs)
		}
		if strings.Contains(pr.transcript(), tok) {
			t.Fatalf("token leaked into transcript")
		}
	})

	t.Run("paste_invalid_then_valid", func(t *testing.T) {
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{"oops", tok, "", ""}}
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount after one re-prompt, got %d", len(cl.addReqs))
		}
	})

	t.Run("paste_invalid_twice", func(t *testing.T) {
		pr := &fakePrompter{answers: []string{"oops", "still-bad"}}
		cl := &fakeAccountClient{}
		err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true})
		if !errors.Is(err, agentcred.ErrInvalidClaudeToken) {
			t.Fatalf("err = %v, want ErrInvalidClaudeToken", err)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("supplied_label_is_used_verbatim", func(t *testing.T) {
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok}}
		cl := &fakeAccountClient{listResult: []*pb.Account{{Id: "a1", Provider: "claude", Label: "existing"}}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, Label: "claude-1",
		}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 || cl.addReqs[0].GetLabel() != "claude-1" {
			t.Fatalf("AddAccount = %+v, want supplied label", cl.addReqs)
		}
	})

	t.Run("keyring_error_hint", func(t *testing.T) {
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok, "", ""}}
		cl := &fakeAccountClient{addErr: errors.New("bossd could not open the system keyring")}
		err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true})
		if err == nil || !strings.Contains(err.Error(), "--allow-insecure-keyring") {
			t.Fatalf("err = %v, want keyring hint", err)
		}
	})

	t.Run("livetest_fail_remove", func(t *testing.T) {
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok, "", ""}, confirms: []bool{false}}
		testErr := errors.New("credential verification failed: 401 unauthorized")
		cl := &fakeAccountClient{testErr: testErr}
		err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true})
		if err == nil {
			t.Fatalf("want error from rejected verification")
		}
		if len(cl.removedIDs) != 1 {
			t.Fatalf("account was not removed: %v", cl.removedIDs)
		}
	})

	t.Run("preseeded_label_skips_prompt", func(t *testing.T) {
		// --token-stdin --label should be fully non-interactive: only the token is
		// read, and no label prompt is shown (a real piped stdin would EOF on any
		// further Ask).
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok}}
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, Label: "my-label", Priority: 7,
		}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
		req := cl.addReqs[0]
		if req.GetLabel() != "my-label" {
			t.Fatalf("label = %q, want provided value", req.GetLabel())
		}
		if req.GetPriority() != 7 {
			t.Fatalf("priority = %d, want 7 (--priority must reach AddAccount)", req.GetPriority())
		}
		for _, asked := range pr.asked {
			if asked == "Label for this account" {
				t.Fatalf("interactive prompt %q shown despite --label", asked)
			}
		}
	})

	t.Run("token_stdin_label_headless", func(t *testing.T) {
		// Regression: `boss account add claude --token-stdin --label piped` piped
		// from a stdin that carries ONLY the token. This uses the real ioPrompter
		// so a second read genuinely hits io.EOF, exactly like a closed pipe.
		tok := claudeToken()
		pr := NewIOPrompter(strings.NewReader(tok+"\n"), io.Discard)
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, StdinUnavailable: true, Label: "piped",
		}); err != nil {
			t.Fatalf("headless token-stdin registration failed: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
		if req := cl.addReqs[0]; req.GetLabel() != "piped" {
			t.Fatalf("label = %q, want piped", req.GetLabel())
		}
	})

	t.Run("token_stdin_no_label_uses_default", func(t *testing.T) {
		// Fully bare headless registration: only the token on stdin and no
		// --label. Label must resolve to its default without prompting exhausted
		// stdin.
		tok := claudeToken()
		pr := NewIOPrompter(strings.NewReader(tok+"\n"), io.Discard)
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, StdinUnavailable: true,
		}); err != nil {
			t.Fatalf("bare headless token-stdin registration failed: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
		if req := cl.addReqs[0]; req.GetLabel() != "claude-1" {
			t.Fatalf("label = %q, want claude-1", req.GetLabel())
		}
	})

	t.Run("token_stdin_existing_label_uses_next_default", func(t *testing.T) {
		tok := claudeToken()
		pr := NewIOPrompter(strings.NewReader(tok+"\n"), io.Discard)
		cl := &fakeAccountClient{listResult: []*pb.Account{
			{Id: "a1", Provider: "claude", Label: "claude-1"},
		}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, StdinUnavailable: true,
		}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want AddAccount, got %d", len(cl.addReqs))
		}
		if req := cl.addReqs[0]; req.GetLabel() != "claude-2" {
			t.Fatalf("label = %q, want default label claude-2", req.GetLabel())
		}
	})

	t.Run("token_stdin_sparse_labels_use_first_available_default", func(t *testing.T) {
		tok := claudeToken()
		pr := NewIOPrompter(strings.NewReader(tok+"\n"), io.Discard)
		cl := &fakeAccountClient{listResult: []*pb.Account{
			{Id: "a1", Provider: "claude", Label: "claude-2"},
		}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, StdinUnavailable: true,
		}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want AddAccount, got %d", len(cl.addReqs))
		}
		if req := cl.addReqs[0]; req.GetLabel() != "claude-1" {
			t.Fatalf("label = %q, want first available default claude-1", req.GetLabel())
		}
	})

	t.Run("livetest_fail_keep", func(t *testing.T) {
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok, "", ""}, confirms: []bool{true}}
		cl := &fakeAccountClient{testErr: errors.New("credential verification failed: 401")}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true}); err != nil {
			t.Fatalf("keep-anyway should succeed: %v", err)
		}
		if len(cl.removedIDs) != 0 {
			t.Fatalf("account must not be removed when kept")
		}
		if !strings.Contains(pr.transcript(), "without verification") {
			t.Fatalf("no verification notice in transcript:\n%s", pr.transcript())
		}
	})

	t.Run("livesmoke_unavailable_keeps_silently", func(t *testing.T) {
		// When provider verification simply couldn't run (no agent plugin loaded),
		// the daemon reports live_smoke_ran=false with the "unavailable" sentinel
		// detail. The credential is fine, so the CLI keeps it silently — no
		// keep/remove prompt, no removal — and prints a calm note.
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok, "", ""}} // no confirms — no keep/remove prompt expected
		cl := &fakeAccountClient{testResult: &pb.TestAccountResponse{
			Account:      &pb.Account{Id: "acc-new", LastTestError: "provider verification unavailable"},
			LiveSmokeRan: false,
			Detail:       "provider verification unavailable",
		}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true}); err != nil {
			t.Fatalf("unavailable verification must keep the account, got err: %v", err)
		}
		if len(cl.removedIDs) != 0 {
			t.Fatalf("account must not be removed when verification is merely unavailable: %v", cl.removedIDs)
		}
		if strings.Contains(pr.transcript(), "Account verification failed") {
			t.Fatalf("must not show the failure/keep-remove prompt:\n%s", pr.transcript())
		}
	})

	t.Run("livesmoke_ran_fail_still_prompts", func(t *testing.T) {
		// A provider check that actually ran (live_smoke_ran=true) and failed is a real
		// failure: the keep/remove prompt applies and declining removes the account.
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok, "", ""}, confirms: []bool{false}}
		cl := &fakeAccountClient{testResult: &pb.TestAccountResponse{
			Account:      &pb.Account{Id: "acc-new", LastTestError: "credential verification failed: 401 unauthorized"},
			LiveSmokeRan: true,
		}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true}); err == nil {
			t.Fatalf("want error from rejected verification failure")
		}
		if len(cl.removedIDs) != 1 {
			t.Fatalf("account should be removed when a failed verification is declined: %v", cl.removedIDs)
		}
	})

	t.Run("livesmoke_false_but_validation_error_still_prompts", func(t *testing.T) {
		// The daemon also returns live_smoke_ran=false for genuine credential
		// validation failures (malformed/missing blob), NOT only the deferred
		// nil-runner case. Those must still offer keep/remove — declining removes
		// the bad account — rather than being silently treated as deferred.
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok, "", ""}, confirms: []bool{false}}
		cl := &fakeAccountClient{testResult: &pb.TestAccountResponse{
			Account:      &pb.Account{Id: "acc-new", LastTestError: `codex credential missing "access" field`},
			LiveSmokeRan: false,
		}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true}); err == nil {
			t.Fatalf("want error from rejected credential-validation failure")
		}
		if len(cl.removedIDs) != 1 {
			t.Fatalf("bad credential should be removed when declined, not deferred: %v", cl.removedIDs)
		}
		if !strings.Contains(pr.transcript(), "Account verification failed") {
			t.Fatalf("keep/remove prompt must be shown for a validation failure:\n%s", pr.transcript())
		}
	})
}

// TestClaudePasteModeDoesNotSuppressTheLabelPrompt pins the split between the
// two things --token-stdin used to mean at once (BOS-847).
//
// PasteMode says only "obtain the token by pasting instead of by running the
// CLI". StdinUnavailable says "there is no interactive input left to read".
// They coincide for `boss account add claude --token-stdin` and for nothing
// else: the TUI pastes a token from a prompter that is perfectly able to answer
// one more question, and the --host TUI flow pastes because it cannot spawn a
// local claude at all. Feeding PasteMode to the identity prompt made those
// callers silently skip the label question based on a fact about a different
// process's stdin.
func TestClaudePasteModeDoesNotSuppressTheLabelPrompt(t *testing.T) {
	const labelPrompt = "Label for this account"

	asked := func(pr *fakePrompter) bool {
		for _, q := range pr.asked {
			if q == labelPrompt {
				return true
			}
		}
		return false
	}

	t.Run("paste_mode_alone_still_asks_and_uses_the_answer", func(t *testing.T) {
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok, "chosen-by-operator"}}
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true,
		}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if !asked(pr) {
			t.Fatalf("PasteMode alone must not suppress %q; asked: %v", labelPrompt, pr.asked)
		}
		if len(cl.addReqs) != 1 || cl.addReqs[0].GetLabel() != "chosen-by-operator" {
			t.Fatalf("label = %+v, want the operator's answer", cl.addReqs)
		}
	})

	t.Run("stdin_unavailable_takes_the_default_without_asking", func(t *testing.T) {
		tok := claudeToken()
		// A non-empty second answer that must NOT be consumed: if the label were
		// still prompted, the account would be labelled "never-read".
		pr := &fakePrompter{answers: []string{tok, "never-read"}}
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, StdinUnavailable: true,
		}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if asked(pr) {
			t.Fatalf("StdinUnavailable must suppress %q; asked: %v", labelPrompt, pr.asked)
		}
		if len(cl.addReqs) != 1 || cl.addReqs[0].GetLabel() != "claude-1" {
			t.Fatalf("label = %+v, want the computed default claude-1", cl.addReqs)
		}
	})

	t.Run("explicit_label_wins_under_either_flag", func(t *testing.T) {
		for _, tc := range []struct {
			name             string
			stdinUnavailable bool
		}{
			{"stdin available", false},
			{"stdin unavailable", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tok := claudeToken()
				pr := &fakePrompter{answers: []string{tok}}
				cl := &fakeAccountClient{}
				if err := RunClaudeAdd(context.Background(), ClaudeOptions{
					Prompter: pr, Client: cl, PasteMode: true,
					StdinUnavailable: tc.stdinUnavailable, Label: "explicit",
				}); err != nil {
					t.Fatalf("RunClaudeAdd: %v", err)
				}
				if asked(pr) {
					t.Fatalf("--label must not be re-asked; asked: %v", pr.asked)
				}
				if len(cl.addReqs) != 1 || cl.addReqs[0].GetLabel() != "explicit" {
					t.Fatalf("label = %+v, want explicit", cl.addReqs)
				}
			})
		}
	})
}

// TestClassifyTest exercises the pure classifier directly, covering every
// outcome branch. The load-bearing subtlety is that live_smoke_ran=false is NOT
// on its own enough to mean "unavailable": only the exact sentinel detail routes
// to testUnavailable, and a genuine credential-validation failure (also
// live_smoke_ran=false) must still route to testFailed.
func TestClassifyTest(t *testing.T) {
	tests := []struct {
		name       string
		resp       *pb.TestAccountResponse
		err        error
		wantOut    testOutcome
		wantReason string
	}{
		{
			name:       "transport_error_is_failure",
			err:        errors.New("connection refused"),
			wantOut:    testFailed,
			wantReason: "connection refused",
		},
		{
			name: "unavailable_sentinel_keeps_silently",
			// The real daemon shape: live_smoke_ran=false, the sentinel echoed in
			// BOTH Detail and last_test_error. The sentinel check must win over the
			// last_test_error branch.
			resp: &pb.TestAccountResponse{
				Account:      &pb.Account{Id: "a", LastTestError: liveSmokeUnavailableDetail},
				LiveSmokeRan: false,
				Detail:       liveSmokeUnavailableDetail,
			},
			wantOut:    testUnavailable,
			wantReason: liveSmokeUnavailableDetail,
		},
		{
			name: "validation_failure_smoke_not_run_is_failure",
			// live_smoke_ran=false with a NON-sentinel detail (malformed credential)
			// must NOT be mistaken for unavailable.
			resp: &pb.TestAccountResponse{
				Account:      &pb.Account{Id: "a", LastTestError: `codex credential missing "access" field`},
				LiveSmokeRan: false,
				Detail:       `codex credential missing "access" field`,
			},
			wantOut:    testFailed,
			wantReason: `codex credential missing "access" field`,
		},
		{
			name: "smoke_ran_and_failed_is_failure",
			resp: &pb.TestAccountResponse{
				Account:      &pb.Account{Id: "a", LastTestError: "credential verification failed: 401"},
				LiveSmokeRan: true,
				Detail:       "credential verification failed: 401",
			},
			wantOut:    testFailed,
			wantReason: "credential verification failed: 401",
		},
		{
			name: "verified_when_no_error",
			resp: &pb.TestAccountResponse{
				Account:      &pb.Account{Id: "a"},
				LiveSmokeRan: true,
			},
			wantOut:    testVerified,
			wantReason: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOut, gotReason := classifyTest(tc.resp, tc.err)
			if gotOut != tc.wantOut {
				t.Fatalf("outcome = %d, want %d", gotOut, tc.wantOut)
			}
			if gotReason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

// TestLiveSmokeUnavailableDetailContract pins the exact sentinel bytes. This
// literal is duplicated (by module-boundary convention) in bossd at
// services/bossd/internal/server/account.go; the "keep silently" behavior works
// only while the two copies stay byte-identical. If you change this string you
// MUST change the bossd copy (and its matching contract test) in lockstep, or
// the CLI silently reverts to the keep/remove prompt for the unavailable case.
func TestLiveSmokeUnavailableDetailContract(t *testing.T) {
	const want = "provider verification unavailable"
	if liveSmokeUnavailableDetail != want {
		t.Fatalf("liveSmokeUnavailableDetail = %q, want %q (must match the bossd sentinel)", liveSmokeUnavailableDetail, want)
	}
}
