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

	t.Run("duplicate_email_does_not_force_distinct_label", func(t *testing.T) {
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok}}
		cl := &fakeAccountClient{listResult: []*pb.Account{{Id: "a1", Provider: "claude", Label: "existing", Email: "dup@example.com"}}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, Label: "claude-1", Email: "dup@example.com",
		}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 || cl.addReqs[0].GetLabel() != "claude-1" || cl.addReqs[0].GetEmail() != "dup@example.com" {
			t.Fatalf("AddAccount = %+v, want duplicate email accepted with supplied label", cl.addReqs)
		}
		if strings.Contains(pr.transcript(), "already registered") {
			t.Fatalf("duplicate email warning should not be shown:\n%s", pr.transcript())
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

	t.Run("preseeded_label_email_skip_prompts", func(t *testing.T) {
		// --token-stdin --label --email should be fully non-interactive: only the
		// token is read, and no email/label prompt is shown (a real piped stdin
		// would EOF on any further Ask).
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok}}
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, Label: "my-label", Email: "me@example.com", Priority: 7,
		}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
		req := cl.addReqs[0]
		if req.GetLabel() != "my-label" || req.GetEmail() != "me@example.com" {
			t.Fatalf("label/email = %q/%q, want provided values", req.GetLabel(), req.GetEmail())
		}
		if req.GetPriority() != 7 {
			t.Fatalf("priority = %d, want 7 (--priority must reach AddAccount)", req.GetPriority())
		}
		for _, asked := range pr.asked {
			if strings.HasPrefix(asked, "Email for this") || asked == "Label for this account" {
				t.Fatalf("interactive prompt %q shown despite --label/--email", asked)
			}
		}
	})

	t.Run("token_stdin_no_email_headless", func(t *testing.T) {
		// Regression: `boss account add claude --token-stdin --label piped` piped
		// from a stdin that carries ONLY the token (no email). This uses the real
		// ioPrompter so a second read genuinely hits io.EOF, exactly like a closed
		// pipe. Before the fix promptIdentity still called Ask for the (empty)
		// email, which EOF'd and failed the registration; with PasteMode the email
		// must resolve to its empty default without prompting.
		tok := claudeToken()
		pr := NewIOPrompter(strings.NewReader(tok+"\n"), io.Discard)
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, Label: "piped",
		}); err != nil {
			t.Fatalf("headless token-stdin registration failed: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
		if req := cl.addReqs[0]; req.GetEmail() != "" || req.GetLabel() != "piped" {
			t.Fatalf("email/label = %q/%q, want \"\"/\"piped\"", req.GetEmail(), req.GetLabel())
		}
	})

	t.Run("token_stdin_no_label_no_email_uses_defaults", func(t *testing.T) {
		// Fully bare headless registration: only the token on stdin, neither
		// --label nor --email. Both identity values must resolve to their defaults
		// (empty email, "claude-1" label) without prompting the exhausted stdin.
		tok := claudeToken()
		pr := NewIOPrompter(strings.NewReader(tok+"\n"), io.Discard)
		cl := &fakeAccountClient{}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true,
		}); err != nil {
			t.Fatalf("bare headless token-stdin registration failed: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
		if req := cl.addReqs[0]; req.GetEmail() != "" || req.GetLabel() != "claude-1" {
			t.Fatalf("email/label = %q/%q, want \"\"/\"claude-1\"", req.GetEmail(), req.GetLabel())
		}
	})

	t.Run("token_stdin_duplicate_email_uses_default_label", func(t *testing.T) {
		tok := claudeToken()
		pr := NewIOPrompter(strings.NewReader(tok+"\n"), io.Discard)
		cl := &fakeAccountClient{listResult: []*pb.Account{
			{Id: "a1", Provider: "claude", Label: "claude-1", Email: "dup@example.com"},
		}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true, Email: "dup@example.com",
		}); err != nil {
			t.Fatalf("RunClaudeAdd: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want AddAccount, got %d", len(cl.addReqs))
		}
		if req := cl.addReqs[0]; req.GetEmail() != "dup@example.com" || req.GetLabel() != "claude-2" {
			t.Fatalf("email/label = %q/%q, want duplicate email with default label claude-2", req.GetEmail(), req.GetLabel())
		}
	})

	t.Run("token_stdin_sparse_labels_use_first_available_default", func(t *testing.T) {
		tok := claudeToken()
		pr := NewIOPrompter(strings.NewReader(tok+"\n"), io.Discard)
		cl := &fakeAccountClient{listResult: []*pb.Account{
			{Id: "a1", Provider: "claude", Label: "claude-2"},
		}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{
			Prompter: pr, Client: cl, PasteMode: true,
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

	t.Run("livesmoke_unavailable_prompts_keep_or_remove", func(t *testing.T) {
		// If the daemon cannot run provider verification, the account is not verified. The
		// CLI must not present this as a successful deferred registration.
		tok := claudeToken()
		pr := &fakePrompter{answers: []string{tok, "", ""}, confirms: []bool{false}}
		cl := &fakeAccountClient{testResult: &pb.TestAccountResponse{
			Account:      &pb.Account{Id: "acc-new", LastTestError: "provider verification unavailable"},
			LiveSmokeRan: false,
		}}
		if err := RunClaudeAdd(context.Background(), ClaudeOptions{Prompter: pr, Client: cl, PasteMode: true}); err == nil {
			t.Fatalf("want error when unavailable verification is rejected")
		}
		if len(cl.removedIDs) != 1 {
			t.Fatalf("account should be removed when unavailable verification is rejected: %v", cl.removedIDs)
		}
		if strings.Contains(pr.transcript(), "deferred") ||
			strings.Contains(pr.transcript(), "credential materialization pending") ||
			strings.Contains(pr.transcript(), "Rotation will run the live test") {
			t.Fatalf("stale deferred copy leaked into transcript:\n%s", pr.transcript())
		}
		if !strings.Contains(pr.transcript(), "Account verification failed") {
			t.Fatalf("keep/remove prompt must be shown:\n%s", pr.transcript())
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
