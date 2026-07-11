package main

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestHandleSigtermDrainsThenExits pins the incident fix: after SIGTERM the
// handler must drain (bounded by shutdownTimeout) and then EXIT the process.
// The old handler drained but never exited, leaving an alive-but-CANCELLED
// zombie the host health loop considered healthy forever.
func TestHandleSigtermDrainsThenExits(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	var order []string
	var gotTimeout time.Duration
	var gotCode int
	done := make(chan struct{})

	shutdown := func(d time.Duration) {
		order = append(order, "shutdown")
		gotTimeout = d
	}
	exit := func(code int) {
		order = append(order, "exit")
		gotCode = code
		close(done)
	}

	go handleSigterm(sigCh, zerolog.Nop(), shutdown, exit)
	sigCh <- syscall.SIGTERM

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit within 2s of SIGTERM")
	}
	if len(order) != 2 || order[0] != "shutdown" || order[1] != "exit" {
		t.Fatalf("call order = %v, want [shutdown exit]", order)
	}
	if gotTimeout != shutdownTimeout {
		t.Fatalf("shutdown timeout = %v, want %v", gotTimeout, shutdownTimeout)
	}
	if gotCode != 0 {
		t.Fatalf("exit code = %d, want 0", gotCode)
	}
}
