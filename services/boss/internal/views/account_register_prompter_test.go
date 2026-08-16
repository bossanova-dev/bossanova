package views

import (
	"context"
	"strings"
	"testing"
	"time"
)

type askResult struct {
	text string
	ok   bool
	err  error
}

func TestTUIPrompterAskRoundTrip(t *testing.T) {
	p := newTUIPrompter(context.Background())
	res := make(chan askResult, 1)
	go func() {
		s, err := p.Ask("Label for this account", "claude-1")
		res <- askResult{text: s, err: err}
	}()

	req := <-p.requests
	if req.kind != promptKindAsk || req.text != "Label for this account" || req.def != "claude-1" {
		t.Fatalf("unexpected Ask request: %+v", req)
	}
	req.reply <- promptResponse{text: "prod"}

	got := <-res
	if got.err != nil || got.text != "prod" {
		t.Fatalf("Ask round trip = %+v, want text=prod", got)
	}
}

func TestTUIPrompterAskSecretRoundTrip(t *testing.T) {
	p := newTUIPrompter(context.Background())
	res := make(chan askResult, 1)
	go func() {
		s, err := p.AskSecret("Paste your token")
		res <- askResult{text: s, err: err}
	}()

	req := <-p.requests
	if req.kind != promptKindSecret {
		t.Fatalf("kind = %d, want secret", req.kind)
	}
	req.reply <- promptResponse{text: "sekret"}

	got := <-res
	if got.err != nil || got.text != "sekret" {
		t.Fatalf("AskSecret round trip = %+v", got)
	}
}

func TestTUIPrompterConfirmRoundTrip(t *testing.T) {
	p := newTUIPrompter(context.Background())
	res := make(chan askResult, 1)
	go func() {
		ok, err := p.Confirm("Keep?", false)
		res <- askResult{ok: ok, err: err}
	}()

	req := <-p.requests
	if req.kind != promptKindConfirm || req.defBool {
		t.Fatalf("unexpected Confirm request: %+v", req)
	}
	req.reply <- promptResponse{ok: true}

	got := <-res
	if got.err != nil || !got.ok {
		t.Fatalf("Confirm round trip = %+v, want ok=true", got)
	}
}

func TestTUIPrompterSayDelivered(t *testing.T) {
	p := newTUIPrompter(context.Background())
	p.Say("hello %s", "world")
	select {
	case line := <-p.progress:
		if line != "hello world" {
			t.Fatalf("Say line = %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("Say line was not delivered")
	}
}

// TestTUIPrompterSubstitutesForwardedSocketDial covers the one text the flow
// composes from a raw RPC error and hands straight to the screen (BOS-847).
//
// Under --host the flow's client is the tunnelled one, so a dropped tunnel
// fails TestAccount with a LOCAL socket path the operator cannot act on, and
// accountflow folds it into "Account verification failed (…). Keep the account
// anyway?" — a decision with nothing to decide on. Not parallel: hostDestination
// is a package global.
func TestTUIPrompterSubstitutesForwardedSocketDial(t *testing.T) {
	const dialErr = "rpc error: code = Unavailable desc = connection error: " +
		"desc = \"transport: Error while dialing dial unix /var/folders/x9/T/bossd.sock: " +
		"connect: no such file or directory\""

	t.Run("remote confirm prompt is rewritten", func(t *testing.T) {
		withHostDestination(t, "deploy@build-box.invalid")
		p := newTUIPrompter(context.Background())
		res := make(chan askResult, 1)
		go func() {
			ok, err := p.Confirm("Account verification failed ("+dialErr+"). Keep the account anyway?", false)
			res <- askResult{ok: ok, err: err}
		}()

		req := <-p.requests
		if strings.Contains(req.text, "dial unix") {
			t.Fatalf("prompt still shows a local socket path:\n%s", req.text)
		}
		if !strings.Contains(req.text, "deploy@build-box.invalid") {
			t.Fatalf("prompt must name the machine that stopped answering:\n%s", req.text)
		}
		// The sentence around the substituted reason must survive, or the operator
		// no longer knows what they are being asked to keep.
		if !strings.Contains(req.text, "Account verification failed") ||
			!strings.Contains(req.text, "Keep the account anyway?") {
			t.Fatalf("substitution ate the surrounding prompt:\n%s", req.text)
		}
		req.reply <- promptResponse{ok: false}
		<-res
	})

	t.Run("remote Say line is rewritten", func(t *testing.T) {
		withHostDestination(t, "deploy@build-box.invalid")
		p := newTUIPrompter(context.Background())
		p.Say("Account %q registered. Verification couldn't run right now (%s); it will run later.", "claude-1", dialErr)
		line := <-p.progress
		if strings.Contains(line, "dial unix") {
			t.Fatalf("progress line still shows a local socket path:\n%s", line)
		}
		if !strings.Contains(line, "registered") {
			t.Fatalf("substitution ate the surrounding line:\n%s", line)
		}
	})

	// keepOrRemove's stranded-credential warning has no parentheses to bound the
	// failure, and it is the one line where losing the sentence costs the
	// operator something they cannot recover: which account was left behind,
	// unverified, on the remote daemon.
	t.Run("an unparenthesised warning keeps the sentence around the failure", func(t *testing.T) {
		withHostDestination(t, "deploy@build-box.invalid")
		p := newTUIPrompter(context.Background())
		p.Say("warning: could not remove unverified account %s: %s", "acct-7", dialErr)
		line := <-p.progress
		if strings.Contains(line, "dial unix") {
			t.Fatalf("progress line still shows a local socket path:\n%s", line)
		}
		if !strings.Contains(line, "could not remove unverified account acct-7") {
			t.Fatalf("substitution ate the stranded-credential warning:\n%s", line)
		}
		if !strings.Contains(line, "deploy@build-box.invalid") {
			t.Fatalf("line must name the machine that stopped answering:\n%s", line)
		}
	})

	t.Run("local text is passed through verbatim", func(t *testing.T) {
		withHostDestination(t, "")
		p := newTUIPrompter(context.Background())
		p.Say("%s", dialErr)
		line := <-p.progress
		if line != dialErr {
			t.Fatalf("local text must not be rewritten:\n%s", line)
		}
	})

	t.Run("an answer from the remote daemon is left alone", func(t *testing.T) {
		withHostDestination(t, "deploy@build-box.invalid")
		p := newTUIPrompter(context.Background())
		// The daemon answered; that answer is the most useful thing on screen and
		// must not be replaced by a reconnecting message that is not true.
		const answered = "Account verification failed (credential verification failed: 401). Keep the account anyway?"
		p.Say("%s", answered)
		if line := <-p.progress; line != answered {
			t.Fatalf("a real daemon answer was rewritten:\n%s", line)
		}
	})
}

func TestTUIPrompterCtxCancelUnblocksPendingAsk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newTUIPrompter(ctx)
	res := make(chan askResult, 1)
	go func() {
		s, err := p.Ask("Label", "def")
		res <- askResult{text: s, err: err}
	}()

	// Ensure the Ask is pending (blocked on its reply) before cancelling.
	<-p.requests
	cancel()

	select {
	case got := <-res:
		if got.err == nil {
			t.Fatalf("cancelled Ask should return an error, got text=%q", got.text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel did not unblock the pending Ask (goroutine leak)")
	}
}
