package vcs

import (
	"errors"
	"strings"
	"testing"
)

func TestActionableErrorWrapsCause(t *testing.T) {
	cause := errors.New("gh api failed")
	err := &ActionableError{
		Code:    ErrorCodeGitHubWorkflowScopeRequired,
		Summary: "GitHub token lacks workflow scope",
		Err:     cause,
	}

	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(err, cause) = false, want true")
	}

	got := err.Error()
	if !strings.Contains(got, "GitHub token lacks workflow scope") {
		t.Errorf("Error() = %q, want summary text", got)
	}
	if !strings.Contains(got, "gh api failed") {
		t.Errorf("Error() = %q, want cause text", got)
	}
}

func TestActionableDetails(t *testing.T) {
	err := errors.Join(&ActionableError{
		Code:    ErrorCodeGitHubWorkflowScopeRequired,
		Summary: "GitHub token lacks workflow scope",
		Detail:  "Provider detail",
		Command: "gh auth refresh -h github.com -s workflow",
		Err:     errors.New("gh api failed"),
	})

	got, ok := ActionableDetails(err)
	if !ok {
		t.Fatalf("ActionableDetails() ok = false, want true")
	}

	for _, want := range []string{
		"GitHub token lacks workflow scope",
		"Provider detail",
		"Fix: gh auth refresh -h github.com -s workflow",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ActionableDetails() = %q, want %q", got, want)
		}
	}
}

func TestActionableDetailsReturnsFalseForPlainError(t *testing.T) {
	got, ok := ActionableDetails(errors.New("plain error"))
	if ok {
		t.Fatalf("ActionableDetails() = (%q, true), want false", got)
	}
	if got != "" {
		t.Fatalf("ActionableDetails() detail = %q, want empty", got)
	}
}

func TestActionableDetailsReturnsFalseForTypedNil(t *testing.T) {
	var actionable *ActionableError
	err := errors.Join(actionable)

	got, ok := ActionableDetails(err)
	if ok {
		t.Fatalf("ActionableDetails() = (%q, true), want false", got)
	}
	if got != "" {
		t.Fatalf("ActionableDetails() detail = %q, want empty", got)
	}
}
