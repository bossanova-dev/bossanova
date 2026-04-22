package tuidriver_test

import (
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuidriver"
)

func TestDriver_SimpleCommand(t *testing.T) {
	// Spawn "echo hello" and verify the output appears on screen.
	d, err := tuidriver.New(tuidriver.Options{
		Command: "echo",
		Args:    []string{"hello-from-tuidriver"},
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	err = d.WaitForText(5*time.Second, "hello-from-tuidriver")
	if err != nil {
		t.Fatal(err)
	}
}

func TestDriver_InteractiveCommand(t *testing.T) {
	// Spawn "cat" (interactive), write to it, and verify echo.
	d, err := tuidriver.New(tuidriver.Options{
		Command: "cat",
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	if err := d.SendString("test-input\n"); err != nil {
		t.Fatalf("SendString: %v", err)
	}

	err = d.WaitForText(5*time.Second, "test-input")
	if err != nil {
		t.Fatal(err)
	}
}

func TestDriver_ScreenContains(t *testing.T) {
	d, err := tuidriver.New(tuidriver.Options{
		Command: "echo",
		Args:    []string{"unique-marker-xyz"},
		Width:   80,
		Height:  24,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	if err := d.WaitForText(5*time.Second, "unique-marker-xyz"); err != nil {
		t.Fatal(err)
	}

	if !d.ScreenContains("unique-marker-xyz") {
		t.Fatalf("ScreenContains returned false; screen:\n%s", d.Screen())
	}
	if d.ScreenContains("nonexistent-text") {
		t.Fatal("ScreenContains returned true for absent text")
	}
}

// TestDriver_CloseRaceRegression exercises the vt.Close vs responseLoop
// race by creating and closing drivers back-to-back. Pre-fix this tripped
// -race on the emulator's `closed` bool; post-fix the pipe writer is
// closed directly and -race stays quiet.
func TestDriver_CloseRaceRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race-stress regression in -short mode")
	}
	for i := range 30 {
		d, err := tuidriver.New(tuidriver.Options{
			Command: "true",
			Width:   80,
			Height:  24,
		})
		if err != nil {
			t.Fatalf("iter %d: New: %v", i, err)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("iter %d: Close: %v", i, err)
		}
	}
}
