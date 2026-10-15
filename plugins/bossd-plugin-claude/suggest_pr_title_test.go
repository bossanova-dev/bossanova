package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestSanitizeSuggestedTitle(t *testing.T) {
	long := strings.Repeat("a", suggestedTitleMaxLen+25)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"trims whitespace", "  Hello world  \n", "Hello world"},
		{"first non-empty line", "\n\nFirst line\nSecond line", "First line"},
		{"strips double quotes", "\"Quoted title\"", "Quoted title"},
		{"strips backticks", "`backtick title`", "backtick title"},
		{"strips single quotes", "'single'", "single"},
		{"empty stays empty", "   \n  ", ""},
		{"caps length", long, strings.Repeat("a", suggestedTitleMaxLen)},
		{"rejects current-title commentary", "The current title accurately and concisely describes the whole change.", ""},
		{"rejects the-title commentary", "The title already fits the change.", ""},
		{"rejects describes-the-change commentary", "It accurately describes the change made here.", ""},
		{"keeps real title mentioning describe", "Describe build flags in the README", "Describe build flags in the README"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeSuggestedTitle(tc.in); got != tc.want {
				t.Fatalf("sanitizeSuggestedTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildPRTitlePrompt(t *testing.T) {
	p := buildPRTitlePrompt(&bossanovav1.SuggestPRTitleRequest{
		CurrentTitle: "[WON-1] Keep me",
		GitLog:       "feat: a\nfix: b",
		BaseBranch:   "dev",
	})
	for _, want := range []string{"[WON-1] Keep me", "feat: a\nfix: b", "dev", "repeat it back exactly"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, p)
		}
	}
	// The prompt must forbid narrating the keep/replace decision as the title.
	if !strings.Contains(p, "NO sentence that describes") {
		t.Errorf("prompt should forbid commentary:\n%s", p)
	}
	// An empty PR body renders as "(none)" rather than a blank section.
	if !strings.Contains(p, "(none)") {
		t.Errorf("empty body should render as (none):\n%s", p)
	}
}

func TestSuggestPRTitle(t *testing.T) {
	ctx := context.Background()
	req := &bossanovav1.SuggestPRTitleRequest{WorkDir: "/wt", CurrentTitle: "X"}

	t.Run("success returns sanitized title", func(t *testing.T) {
		s := &Server{logger: zerolog.Nop()}
		s.oneShot = func(context.Context, string, string, map[string]string, bool) (string, error) {
			return "  \"Better title\"\nignored second line\n", nil
		}
		resp, err := s.SuggestPRTitle(ctx, req)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !resp.GetSupported() || resp.GetTitle() != "Better title" {
			t.Fatalf("supported=%v title=%q", resp.GetSupported(), resp.GetTitle())
		}
	})

	t.Run("exec error -> not supported (no error returned)", func(t *testing.T) {
		s := &Server{logger: zerolog.Nop()}
		s.oneShot = func(context.Context, string, string, map[string]string, bool) (string, error) {
			return "", errors.New("claude failed")
		}
		resp, err := s.SuggestPRTitle(ctx, req)
		if err != nil {
			t.Fatalf("SuggestPRTitle must not return an error on exec failure: %v", err)
		}
		if resp.GetSupported() {
			t.Fatal("supported should be false on exec failure")
		}
	})

	t.Run("empty output -> not supported", func(t *testing.T) {
		s := &Server{logger: zerolog.Nop()}
		s.oneShot = func(context.Context, string, string, map[string]string, bool) (string, error) {
			return "  \n ", nil
		}
		resp, err := s.SuggestPRTitle(ctx, req)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if resp.GetSupported() {
			t.Fatal("supported should be false on empty output")
		}
	})
}

// TestRunClaudeOneShotAppliesExtraEnvOverAmbient pins BOS-1562 for the PR-title
// one-shot: it runs claude in the session worktree, so the daemon's repo pins
// (empty proof, Linear and Sentry keys for an untrusted repo) must override the
// plugin's ambient values in the child, exactly as a session spawn does.
func TestRunClaudeOneShotAppliesExtraEnvOverAmbient(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\nprintf 'linear=[%s] proof=[%s] keep=[%s]\\n' \"$LINEAR_API_KEY\" \"$PROOF_ANTHROPIC_API_KEY\" \"$BOSS_TEST_KEEP\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LINEAR_API_KEY", "ambient-linear")
	t.Setenv("PROOF_ANTHROPIC_API_KEY", "ambient-proof")
	t.Setenv("BOSS_TEST_KEEP", "inherited")

	s := &Server{logger: zerolog.Nop()}
	out, err := s.runClaudeOneShot(context.Background(), t.TempDir(), "prompt", map[string]string{
		"LINEAR_API_KEY":          "",
		"PROOF_ANTHROPIC_API_KEY": "",
	}, true)
	if err != nil {
		t.Fatalf("runClaudeOneShot: %v", err)
	}
	if want := "linear=[] proof=[] keep=[inherited]"; strings.TrimSpace(out) != want {
		t.Fatalf("child env = %q, want %q (pins must override ambient; approved parent variables survive)", strings.TrimSpace(out), want)
	}
}

// TestSuggestPRTitleForwardsExtraEnv proves the request's overlay reaches the
// one-shot rather than being dropped on the way.
func TestSuggestPRTitleForwardsExtraEnv(t *testing.T) {
	var got map[string]string
	s := &Server{logger: zerolog.Nop()}
	s.oneShot = func(_ context.Context, _, _ string, env map[string]string, _ bool) (string, error) {
		got = env
		return "Title", nil
	}
	if _, err := s.SuggestPRTitle(context.Background(), &bossanovav1.SuggestPRTitleRequest{
		WorkDir:  "/wt",
		ExtraEnv: map[string]string{"LINEAR_API_KEY": ""},
	}); err != nil {
		t.Fatal(err)
	}
	if v, ok := got["LINEAR_API_KEY"]; !ok || v != "" {
		t.Fatalf("one-shot env = %v, want LINEAR_API_KEY pinned empty", got)
	}
}

func TestRunClaudeOneShotUntrustedDisablesProjectConfiguration(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = --help ]; then printf '%s' '--strict-mcp-config --setting-sources'; exit; fi\ncat >/dev/null\nprintf '%s' \"$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := newServer(nil, zerolog.Nop())
	got, err := s.runClaudeOneShot(context.Background(), t.TempDir(), "prompt", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--strict-mcp-config", "--setting-sources user"} {
		if !strings.Contains(got, flag) {
			t.Errorf("one-shot argv %q missing %q", got, flag)
		}
	}
}

func TestSuggestPRTitleProjectConfigurationTrust(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted=%v", trusted), func(t *testing.T) {
			var launchArgs []string
			helpCalls := 0
			factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
				if slices.Contains(args, "--help") {
					helpCalls++
					return exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s' '--strict-mcp-config --setting-sources'")
				}
				launchArgs = slices.Clone(args)
				return exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null; printf '%s' 'Title'")
			}
			srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
			resp, err := srv.SuggestPRTitle(context.Background(), &bossanovav1.SuggestPRTitleRequest{WorkDir: t.TempDir(), ShouldTrustProjectConfig: trusted})
			if err != nil || !resp.GetSupported() {
				t.Fatalf("suggestion: %+v %v", resp, err)
			}
			want := []string{"-p", "--permission-mode", "plan"}
			wantHelpCalls := 0
			if !trusted {
				want = append(want, "--strict-mcp-config", "--setting-sources", "user")
				wantHelpCalls = 1
			}
			if !slices.Equal(launchArgs, want) || helpCalls != wantHelpCalls {
				t.Fatalf("args %v (help %d), want %v (help %d)", launchArgs, helpCalls, want, wantHelpCalls)
			}
		})
	}
}

func TestRunClaudeOneShotMissingFlagNeverLaunches(t *testing.T) {
	for _, missing := range []string{"--strict-mcp-config", "--setting-sources"} {
		t.Run(missing, func(t *testing.T) {
			launches := 0
			supported := "--strict-mcp-config"
			if supported == missing {
				supported = "--setting-sources"
			}
			factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
				if !slices.Contains(args, "--help") {
					launches++
				}
				return exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s' "+supported)
			}
			srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
			_, err := srv.runClaudeOneShot(context.Background(), t.TempDir(), "prompt", nil, false)
			if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "boss repo update <id> --trusted") {
				t.Fatalf("error: %v", err)
			}
			if launches != 0 {
				t.Fatalf("unsafe launches = %d", launches)
			}
		})
	}
}
