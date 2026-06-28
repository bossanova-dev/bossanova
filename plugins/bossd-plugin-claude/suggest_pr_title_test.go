package main

import (
	"context"
	"errors"
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
		s.oneShot = func(context.Context, string, string) (string, error) {
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
		s.oneShot = func(context.Context, string, string) (string, error) {
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
		s.oneShot = func(context.Context, string, string) (string, error) {
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
