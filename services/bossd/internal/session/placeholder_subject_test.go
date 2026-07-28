package session

import "testing"

// The classification itself (gitpkg.IsDraftPRPlaceholderSubject) is unit-tested
// in package git, which owns both the placeholder literal and the helper. The
// tests below pin realCommitSubjects — the finalize-side consumer whose
// exact-string comparison the BOS-591 regression defeated.

// TestRealCommitSubjects_TagInjectedPlaceholder is the regression anchor: on
// the parent commit (before the fix), the PR-tag-injected placeholder subject
// still exact-string-matched draftPRPlaceholderCommitSubject as unequal and
// was counted as real work, defeating finalize's empty-run guard.
func TestRealCommitSubjects_TagInjectedPlaceholder(t *testing.T) {
	got := realCommitSubjects([]string{"chore: [#1689] [skip ci] create pull request"})
	if len(got) != 0 {
		t.Fatalf("realCommitSubjects(tag-injected placeholder) = %v, want empty slice", got)
	}
}

// TestRealCommitSubjects_StackedTagPlaceholder pins the stacking case. The
// boss-finalize skill's add-pr-numbers.sh still tags the placeholder
// unconditionally and skips only when the CURRENT run's number is already
// present, so two runs for different PR numbers leave two stacked tags. A
// single-tag-tolerant classifier would count that as real work and reopen the
// guard this ticket closes.
func TestRealCommitSubjects_StackedTagPlaceholder(t *testing.T) {
	for _, subject := range []string{
		"chore: [#7] [#1689] [skip ci] create pull request",
		"chore: [#7] [#42] [#1689] [skip ci] create pull request",
	} {
		if got := realCommitSubjects([]string{subject}); len(got) != 0 {
			t.Errorf("realCommitSubjects(%q) = %v, want empty slice", subject, got)
		}
	}
}

// TestRealCommitSubjects_StackedTagRealCommitStillReal is the safety half: the
// repeat-capable strip must not turn genuine tagged work into a placeholder.
func TestRealCommitSubjects_StackedTagRealCommitStillReal(t *testing.T) {
	in := "feat(boss): [#7] [#1689] add X"
	got := realCommitSubjects([]string{in})
	if len(got) != 1 || got[0] != in {
		t.Fatalf("realCommitSubjects(%q) = %v, want the subject kept byte-identical", in, got)
	}
}

func TestRealCommitSubjects_UntaggedPlaceholderStillEmpty(t *testing.T) {
	got := realCommitSubjects([]string{draftPRPlaceholderCommitSubject})
	if len(got) != 0 {
		t.Fatalf("realCommitSubjects(untagged placeholder) = %v, want empty slice", got)
	}
}

func TestRealCommitSubjects_RealCommitUnmutated(t *testing.T) {
	in := "feat(boss): [#1689] add X"
	got := realCommitSubjects([]string{in})
	if len(got) != 1 {
		t.Fatalf("realCommitSubjects(real commit) = %v, want one subject", got)
	}
	// Byte-identical check: the placeholder-detection normalization must
	// never leak into the subjects realCommitSubjects returns.
	if got[0] != in {
		t.Fatalf("realCommitSubjects mutated the subject: got %q, want %q (byte-identical to input)", got[0], in)
	}
}

func TestRealCommitSubjects_MixedSliceKeepsOnlyRealCommit(t *testing.T) {
	in := []string{
		draftPRPlaceholderCommitSubject,
		"chore: [#1689] [skip ci] create pull request",
		"feat(boss): [#1689] add X",
	}
	got := realCommitSubjects(in)
	if len(got) != 1 || got[0] != "feat(boss): [#1689] add X" {
		t.Fatalf("realCommitSubjects(mixed) = %v, want only the real commit", got)
	}
}
