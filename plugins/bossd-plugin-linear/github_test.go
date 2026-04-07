package main

import (
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestMatchPR_BranchMatch(t *testing.T) {
	issue := linearIssue{
		Identifier: "ENG-123",
		BranchName: "eng-123-fix-login",
	}

	prs := []*bossanovav1.PRSummary{
		{Number: 42, HeadBranch: "eng-123-fix-login", Title: "Fix login"},
		{Number: 43, HeadBranch: "eng-124-dark-mode", Title: "[ENG-124] Add dark mode"},
	}

	prNumber, branch := matchPR(issue, prs)

	if prNumber != 42 {
		t.Errorf("Expected PR number 42, got %d", prNumber)
	}
	if branch != "eng-123-fix-login" {
		t.Errorf("Expected branch 'eng-123-fix-login', got '%s'", branch)
	}
}

func TestMatchPR_TitleMatch(t *testing.T) {
	issue := linearIssue{
		Identifier: "ENG-124",
		BranchName: "some-other-branch",
	}

	prs := []*bossanovav1.PRSummary{
		{Number: 42, HeadBranch: "eng-123-fix-login", Title: "Fix login"},
		{Number: 43, HeadBranch: "eng-124-dark-mode", Title: "[ENG-124] Add dark mode"},
	}

	prNumber, branch := matchPR(issue, prs)

	if prNumber != 43 {
		t.Errorf("Expected PR number 43, got %d", prNumber)
	}
	if branch != "eng-124-dark-mode" {
		t.Errorf("Expected branch 'eng-124-dark-mode', got '%s'", branch)
	}
}

func TestMatchPR_BranchPreferredOverTitle(t *testing.T) {
	issue := linearIssue{
		Identifier: "ENG-123",
		BranchName: "eng-123-fix-login",
	}

	prs := []*bossanovav1.PRSummary{
		{Number: 42, HeadBranch: "eng-123-fix-login", Title: "Fix login"},
		{Number: 43, HeadBranch: "other-branch", Title: "[ENG-123] Alternative fix"},
	}

	prNumber, branch := matchPR(issue, prs)

	// Should match by branch (PR 42), not by title (PR 43)
	if prNumber != 42 {
		t.Errorf("Expected PR number 42 (branch match), got %d", prNumber)
	}
	if branch != "eng-123-fix-login" {
		t.Errorf("Expected branch 'eng-123-fix-login', got '%s'", branch)
	}
}

func TestMatchPR_NoMatch(t *testing.T) {
	issue := linearIssue{
		Identifier: "ENG-999",
		BranchName: "eng-999-nonexistent",
	}

	prs := []*bossanovav1.PRSummary{
		{Number: 42, HeadBranch: "eng-123-fix-login", Title: "Fix login"},
		{Number: 43, HeadBranch: "eng-124-dark-mode", Title: "[ENG-124] Add dark mode"},
	}

	prNumber, branch := matchPR(issue, prs)

	if prNumber != 0 {
		t.Errorf("Expected PR number 0, got %d", prNumber)
	}
	if branch != "" {
		t.Errorf("Expected empty branch, got '%s'", branch)
	}
}

func TestMatchPR_EmptyPRs(t *testing.T) {
	issue := linearIssue{
		Identifier: "ENG-123",
		BranchName: "eng-123-fix-login",
	}

	prs := []*bossanovav1.PRSummary{}

	prNumber, branch := matchPR(issue, prs)

	if prNumber != 0 {
		t.Errorf("Expected PR number 0, got %d", prNumber)
	}
	if branch != "" {
		t.Errorf("Expected empty branch, got '%s'", branch)
	}
}

func TestMatchPR_CaseSensitiveIdentifier(t *testing.T) {
	issue := linearIssue{
		Identifier: "ENG-123",
		BranchName: "some-other-branch",
	}

	prs := []*bossanovav1.PRSummary{
		{Number: 42, HeadBranch: "eng-123-fix-login", Title: "[eng-123] Fix login"},
		{Number: 43, HeadBranch: "eng-124-dark-mode", Title: "[ENG-123] Add dark mode"},
	}

	prNumber, branch := matchPR(issue, prs)

	// Should only match exact case [ENG-123], not [eng-123]
	if prNumber != 43 {
		t.Errorf("Expected PR number 43 (exact case match), got %d", prNumber)
	}
	if branch != "eng-124-dark-mode" {
		t.Errorf("Expected branch 'eng-124-dark-mode', got '%s'", branch)
	}
}
