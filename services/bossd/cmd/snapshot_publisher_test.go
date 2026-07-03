package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossd/internal/upstream"
	"github.com/rs/zerolog"
)

// fakePublishClient stubs only PublishDaemonSnapshot; every other RPC on the
// embedded interface is nil and would panic if the publisher ever called it
// (it must not). It records the bearer token seen on each call and can fail
// the first attempt to exercise the re-register path.
type fakePublishClient struct {
	bossanovav1connect.OrchestratorServiceClient
	failFirstWith error
	acceptToken   string
	tokens        []string
}

func (f *fakePublishClient) PublishDaemonSnapshot(
	_ context.Context,
	req *connect.Request[bossanovav1.PublishDaemonSnapshotRequest],
) (*connect.Response[bossanovav1.PublishDaemonSnapshotResponse], error) {
	token := strings.TrimPrefix(req.Header().Get("Authorization"), "Bearer ")
	f.tokens = append(f.tokens, token)
	if len(f.tokens) == 1 && f.failFirstWith != nil {
		return nil, f.failFirstWith
	}
	if f.acceptToken != "" && token != f.acceptToken {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
	}
	return connect.NewResponse(&bossanovav1.PublishDaemonSnapshotResponse{}), nil
}

// cancelledCtx returns a context that is already done, so runSnapshotPublisher
// runs its initial publish() exactly once and then the loop returns on
// ctx.Done — a deterministic single iteration with no timing dependence.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// On CodeUnauthenticated ("invalid credentials"), the publisher must
// re-register, rotate the shared token holder, and retry once with the fresh
// token — this is the self-heal that gets a wedged daemon back onto the web.
func TestRunSnapshotPublisher_SelfHealsOnAuthRejection(t *testing.T) {
	holder := upstream.NewSessionTokenHolder("stale")
	reReg := func(context.Context) (string, error) { return "fresh", nil }
	client := &fakePublishClient{
		failFirstWith: connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials")),
	}

	runSnapshotPublisher(cancelledCtx(), client, holder, upstream.StreamStores{},
		"daemon-1", "host-1", reReg, nil, time.Hour, zerolog.Nop())

	if len(client.tokens) != 2 {
		t.Fatalf("expected 2 publish attempts (fail + retry), got %d: %v", len(client.tokens), client.tokens)
	}
	if client.tokens[0] != "stale" || client.tokens[1] != "fresh" {
		t.Fatalf("expected tokens [stale fresh], got %v", client.tokens)
	}
	if got := holder.Get(); got != "fresh" {
		t.Fatalf("expected holder rotated to fresh, got %q", got)
	}
}

// A healthy publish must not re-register or retry.
func TestRunSnapshotPublisher_NoReRegisterOnSuccess(t *testing.T) {
	holder := upstream.NewSessionTokenHolder("good")
	reRegCalled := false
	reReg := func(context.Context) (string, error) { reRegCalled = true; return "x", nil }
	client := &fakePublishClient{}

	runSnapshotPublisher(cancelledCtx(), client, holder, upstream.StreamStores{},
		"daemon-1", "host-1", reReg, nil, time.Hour, zerolog.Nop())

	if reRegCalled {
		t.Error("reRegister must not be called on a successful publish")
	}
	if len(client.tokens) != 1 {
		t.Fatalf("expected exactly 1 publish attempt, got %d", len(client.tokens))
	}
	if holder.Get() != "good" {
		t.Errorf("holder token must be unchanged, got %q", holder.Get())
	}
}

// A non-auth error (e.g. CodeUnavailable) must NOT trigger re-register — only
// CodeUnauthenticated means the token itself is bad.
func TestRunSnapshotPublisher_NoReRegisterOnNonAuthError(t *testing.T) {
	holder := upstream.NewSessionTokenHolder("tok")
	reRegCalled := false
	reReg := func(context.Context) (string, error) { reRegCalled = true; return "x", nil }
	client := &fakePublishClient{
		failFirstWith: connect.NewError(connect.CodeUnavailable, errors.New("backend down")),
	}

	runSnapshotPublisher(cancelledCtx(), client, holder, upstream.StreamStores{},
		"daemon-1", "host-1", reReg, nil, time.Hour, zerolog.Nop())

	if reRegCalled {
		t.Error("reRegister must not be called on a non-auth error")
	}
	if len(client.tokens) != 1 {
		t.Fatalf("expected exactly 1 publish attempt (no retry), got %d", len(client.tokens))
	}
}

// If another recovery path rotates the shared holder while the publisher is
// re-registering, the publisher must not write its now-stale return value over
// the newer token. Bosso keeps one current token per daemon_id, so the later
// registration wins.
func TestRunSnapshotPublisher_DoesNotOverwriteConcurrentTokenRotation(t *testing.T) {
	holder := upstream.NewSessionTokenHolder("stale")
	reReg := func(context.Context) (string, error) {
		holder.Set("fresh-from-stream")
		return "fresh-from-publisher", nil
	}
	client := &fakePublishClient{
		failFirstWith: connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials")),
		acceptToken:   "fresh-from-stream",
	}

	runSnapshotPublisher(cancelledCtx(), client, holder, upstream.StreamStores{},
		"daemon-1", "host-1", reReg, nil, time.Hour, zerolog.Nop())

	if len(client.tokens) != 2 {
		t.Fatalf("expected 2 publish attempts (fail + retry), got %d: %v", len(client.tokens), client.tokens)
	}
	if client.tokens[0] != "stale" || client.tokens[1] != "fresh-from-stream" {
		t.Fatalf("expected tokens [stale fresh-from-stream], got %v", client.tokens)
	}
	if got := holder.Get(); got != "fresh-from-stream" {
		t.Fatalf("expected holder to keep concurrent token, got %q", got)
	}
}
