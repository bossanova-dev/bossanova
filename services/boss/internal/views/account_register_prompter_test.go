package views

import (
	"context"
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
