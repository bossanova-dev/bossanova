package vcs

import (
	"errors"
	"strings"
)

const ErrorCodeGitHubWorkflowScopeRequired = "github_workflow_scope_required"

type ActionableError struct {
	Code    string
	Summary string
	Detail  string
	Command string
	Err     error
}

func (e *ActionableError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Summary
	}
	if e.Summary == "" {
		return e.Err.Error()
	}
	return e.Summary + ": " + e.Err.Error()
}

func (e *ActionableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ActionableError) OperatorMessage() string {
	if e == nil {
		return ""
	}

	parts := make([]string, 0, 3)
	if e.Summary != "" {
		parts = append(parts, e.Summary)
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	if e.Command != "" {
		parts = append(parts, "Fix: "+e.Command)
	}
	return strings.Join(parts, "\n\n")
}

func ActionableDetails(err error) (string, bool) {
	var actionable *ActionableError
	if !errors.As(err, &actionable) || actionable == nil {
		return "", false
	}

	detail := actionable.OperatorMessage()
	if detail == "" {
		detail = actionable.Error()
	}
	return detail, true
}
