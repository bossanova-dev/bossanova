package account

import (
	"errors"
	"strings"
	"testing"
)

// The classification is the whole point of this type: BOS-881's lesson is that a
// fail-closed guard which cannot tell "the credential is unusable" from "I could
// not evaluate the credential" reports the second as the first. These tests pin
// that the two outcomes stay distinguishable through wrapping, and that nothing
// has to parse Error() text to tell them apart.

func TestInjectionErrorMessageNamesAccountAndCause(t *testing.T) {
	err := invalidInjection("acct-1", "codex", "materialize credentials", errors.New("boom"))
	got := err.Error()
	if !strings.Contains(got, `account "acct-1"`) {
		t.Errorf("Error() = %q, want it to name the account", got)
	}
	if !strings.Contains(got, "materialize credentials") {
		t.Errorf("Error() = %q, want it to carry the reason", got)
	}
	if !strings.Contains(got, "boom") {
		t.Errorf("Error() = %q, want it to carry the cause", got)
	}
}

func TestInjectionErrorFallsBackWhenReasonAndAccountAreEmpty(t *testing.T) {
	err := &InjectionError{Outcome: InjectionOutcomeUndetermined}
	if got := err.Error(); got != "credential injection failed" {
		t.Errorf("Error() = %q, want the fallback message", got)
	}
}

func TestInjectionErrorUnwrapsToCauseThroughWrapping(t *testing.T) {
	// Downstream context-cancellation checks and errors.Is comparisons must see
	// through both the typed refusal and any fmt.Errorf wrapping the callers add.
	sentinel := errors.New("sentinel")
	err := error(invalidInjection("acct-1", "codex", "materialize credentials", sentinel))
	if !errors.Is(err, sentinel) {
		t.Fatal("errors.Is: want the cause to remain reachable")
	}
	wrapped := errors.Join(errors.New("outer"), err)
	if !errors.Is(wrapped, sentinel) {
		t.Fatal("errors.Is through a wrapper: want the cause to remain reachable")
	}
	if _, ok := AsInjectionError(wrapped); !ok {
		t.Fatal("AsInjectionError: want the typed refusal to survive wrapping")
	}
}

func TestInjectionOutcomeReadersAreMutuallyExclusive(t *testing.T) {
	invalid := error(invalidInjection("acct-1", "codex", "not found", nil))
	undetermined := error(undeterminedInjection("acct-1", "codex", "registry unavailable", nil))

	if !IsInjectionInvalid(invalid) || IsInjectionUndetermined(invalid) {
		t.Error("an invalid refusal must not also read as undetermined")
	}
	if !IsInjectionUndetermined(undetermined) || IsInjectionInvalid(undetermined) {
		t.Error("an undetermined refusal must not also read as invalid")
	}
	if got := InjectionOutcomeOf(invalid); got != InjectionOutcomeInvalid {
		t.Errorf("InjectionOutcomeOf(invalid) = %q", got)
	}
	if got := InjectionOutcomeOf(undetermined); got != InjectionOutcomeUndetermined {
		t.Errorf("InjectionOutcomeOf(undetermined) = %q", got)
	}
}

func TestInjectionOutcomeOfUntypedErrorIsEmpty(t *testing.T) {
	// A plain error carries no classification. Returning the empty outcome
	// rather than guessing "invalid" is what stops an unrelated failure from
	// being reported to an operator as a bad credential.
	if got := InjectionOutcomeOf(errors.New("unrelated")); got != "" {
		t.Errorf("InjectionOutcomeOf(plain) = %q, want empty", got)
	}
	if IsInjectionInvalid(errors.New("unrelated")) {
		t.Error("a plain error must not read as an invalid credential")
	}
	if InjectionOutcomeOf(nil) != "" {
		t.Error("InjectionOutcomeOf(nil) must be empty")
	}
}

func TestRedactedMasksSecretShapedCauseWhileErrorStaysRaw(t *testing.T) {
	// The materialize error can embed a provider response body. Error() keeps it
	// for the daemon log and errors.Is; Redacted() is what may reach a screen.
	const secret = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	err := invalidInjection("acct-1", "codex", "materialize credentials",
		errors.New("provider rejected api_key="+secret))

	if !strings.Contains(err.Error(), secret) {
		t.Fatal("Error() must stay raw so the daemon log and errors.Is keep the cause")
	}
	redacted := err.Redacted()
	if strings.Contains(redacted, secret) {
		t.Fatalf("Redacted() leaked the credential: %q", redacted)
	}
	if !strings.Contains(redacted, "materialize credentials") {
		t.Errorf("Redacted() = %q, want the operator-facing reason preserved", redacted)
	}
}

func TestRedactedMessageHandlesPlainAndNilErrors(t *testing.T) {
	if got := RedactedMessage(nil); got != "" {
		t.Errorf("RedactedMessage(nil) = %q, want empty", got)
	}
	const secret = "sk-ant-api03-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	got := RedactedMessage(errors.New("token=" + secret))
	if strings.Contains(got, secret) {
		t.Fatalf("RedactedMessage leaked the credential: %q", got)
	}
	if got == "" {
		t.Error("RedactedMessage must still describe the failure")
	}
}
