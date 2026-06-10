package tuitest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/boss/internal/tuitest"
)

type proofStep struct {
	Keys             []string `json:"keys"`
	WaitForReadyText string   `json:"waitForReadyText"`
	WaitForText      string   `json:"waitForText"`
}

type proofRecipe struct {
	ID               string      `json:"id"`
	Title            string      `json:"title"`
	Args             []string    `json:"args"`
	Keys             []string    `json:"keys"`
	WaitForReadyText string      `json:"waitForReadyText"`
	WaitForText      string      `json:"waitForText"`
	Fixture          string      `json:"fixture"`
	Steps            []proofStep `json:"steps"`
	Terminal         struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"terminal"`
}

func TestProofCapture(t *testing.T) {
	recipePath := os.Getenv("BOSS_PROOF_RECIPE")
	outputDir := os.Getenv("BOSS_PROOF_OUTPUT_DIR")
	if recipePath == "" || outputDir == "" {
		t.Skip("BOSS_PROOF_RECIPE and BOSS_PROOF_OUTPUT_DIR are required for proof capture")
	}

	recipePath = resolveProofPath(recipePath)
	outputDir = resolveProofPath(outputDir)

	recipe := readProofRecipe(t, recipePath)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("create proof output dir: %v", err)
	}

	h := tuitest.New(t, fixtureProfile(t, recipe)...)

	if err := h.Driver.WaitForText(10*time.Second, "Bossanova"); err != nil {
		// Onboarding skips straight to the first-run screen and never renders the
		// app shell, so a missing "Bossanova" banner is expected there only.
		if recipe.Fixture != "onboarding" {
			t.Fatalf("wait for app shell: %v", err)
		}
	}

	if recipe.WaitForReadyText != "" {
		if err := h.Driver.WaitForText(10*time.Second, recipe.WaitForReadyText); err != nil {
			t.Fatalf("wait for ready proof text: %v", err)
		}
	}

	if len(recipe.Steps) > 0 {
		for i, step := range recipe.Steps {
			// Gate keys on a data-dependent anchor so async-loaded views (home
			// session list, repo list) have populated before the keypress; the
			// underlying models ignore navigation keys until their data arrives.
			if step.WaitForReadyText != "" {
				if err := h.Driver.WaitForText(10*time.Second, step.WaitForReadyText); err != nil {
					t.Fatalf("step %d wait for ready %q: %v", i, step.WaitForReadyText, err)
				}
			}
			for _, key := range step.Keys {
				sendProofKey(t, h, key)
			}
			if step.WaitForText != "" {
				if err := h.Driver.WaitForText(10*time.Second, step.WaitForText); err != nil {
					t.Fatalf("step %d wait for %q: %v", i, step.WaitForText, err)
				}
			}
		}
	} else {
		// Legacy single-step recipes use a flat keys array (no per-step waits).
		for _, key := range recipe.Keys {
			sendProofKey(t, h, key)
		}
	}

	if recipe.WaitForText != "" {
		if err := h.Driver.WaitForText(10*time.Second, recipe.WaitForText); err != nil {
			t.Fatalf("wait for final proof text: %v", err)
		}
	}

	screenPath := filepath.Join(outputDir, "screen.txt")
	if err := os.WriteFile(screenPath, []byte(h.Driver.Screen()), 0o644); err != nil {
		t.Fatalf("write proof screen: %v", err)
	}
}

func readProofRecipe(t *testing.T, path string) proofRecipe {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read proof recipe: %v", err)
	}
	var recipe proofRecipe
	if err := json.Unmarshal(data, &recipe); err != nil {
		t.Fatalf("parse proof recipe: %v", err)
	}
	if recipe.Terminal.Width == 0 {
		recipe.Terminal.Width = 140
	}
	if recipe.Terminal.Height == 0 {
		recipe.Terminal.Height = 36
	}
	return recipe
}

// fixtureProfile maps a recipe's "fixture" field to harness options. "demo"
// (default) seeds the full demo world logged-in; "login" launches logged-out so
// pressing 'l' reaches the device-code screen; "onboarding" launches the
// first-run gate directly.
func fixtureProfile(t *testing.T, recipe proofRecipe) []tuitest.Option {
	t.Helper()

	base := []tuitest.Option{
		tuitest.WithTerminalSize(recipe.Terminal.Width, recipe.Terminal.Height),
		tuitest.WithArgs(recipe.Args...),
		// Deterministic worktree dir keeps the settings screen free of the
		// per-test temp HOME (a machine-specific, high-entropy path).
		tuitest.WithWorktreeBaseDir("/home/bossanova/worktrees"),
	}

	switch recipe.Fixture {
	case "onboarding":
		return append(base,
			tuitest.WithFirstRunOnboarding(),
			tuitest.WithEnv("PATH="+fakeProofProviderPath(t, "claude", "codex")),
		)
	case "login":
		// Starts LOGGED OUT: no WithLoggedInUser and no pre-seeded auth token, so
		// pressing 'l' actually reaches the login view. WithE2ELogin only arms the
		// device-code client with a deterministic code to complete the flow when
		// initiated; it does not pre-authenticate. Do not add WithLoggedInUser here
		// or the login screen becomes unreachable.
		return append(base, tuitest.WithE2ELogin("proof@example.com"))
	default: // "demo" and "" both mean the full demo world.
		w := fixtures.DemoWorld()
		return append(base,
			tuitest.WithLoggedInUser("proof@example.com"),
			tuitest.WithRepos(w.Repos...),
			tuitest.WithSessions(w.Sessions...),
			tuitest.WithChats(w.Chats...),
			tuitest.WithCronJobs(w.CronJobs...),
		)
	}
}

func fakeProofProviderPath(t *testing.T, commands ...string) string {
	t.Helper()

	dir := t.TempDir()
	for _, command := range commands {
		path := filepath.Join(dir, command)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write fake provider %q: %v", command, err)
		}
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func resolveProofPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	_, filename, _, _ := runtime.Caller(0)
	serviceDir := filepath.Join(filepath.Dir(filename), "..", "..")
	serviceRelative := filepath.Clean(filepath.Join(serviceDir, path))
	if _, err := os.Stat(serviceRelative); err == nil {
		return serviceRelative
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return serviceRelative
}

func sendProofKey(t *testing.T, h *tuitest.Harness, key string) {
	t.Helper()
	if len(key) == 6 && strings.HasPrefix(key, "ctrl+") && key[5] >= 'a' && key[5] <= 'z' {
		// Ctrl+<letter> is the control byte: 'a'->0x01, 'b'->0x02, ...
		if err := h.Driver.SendKey(key[5] - 'a' + 1); err != nil {
			t.Fatalf("send %s: %v", key, err)
		}
		return
	}
	switch key {
	case "enter":
		if err := h.Driver.SendEnter(); err != nil {
			t.Fatalf("send enter: %v", err)
		}
	case "esc":
		if err := h.Driver.SendEscape(); err != nil {
			t.Fatalf("send escape: %v", err)
		}
	default:
		if len(key) != 1 {
			t.Fatalf("unsupported proof key %q", key)
		}
		if err := h.Driver.SendKey(key[0]); err != nil {
			t.Fatalf("send key %q: %v", key, err)
		}
	}
}
