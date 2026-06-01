package sessionreason

import (
	"errors"
	"testing"
)

func TestDraftPRCreationFailure(t *testing.T) {
	reason := DraftPRCreationFailure(errors.New("create draft PR: authentication required"))
	if reason == "" {
		t.Fatal("DraftPRCreationFailure() = empty string")
	}
	if !IsDraftPRCreationFailure(&reason) {
		t.Fatalf("IsDraftPRCreationFailure(%q) = false, want true", reason)
	}
}

func TestIsDraftPRCreationFailure(t *testing.T) {
	reason := "CI failed"
	if IsDraftPRCreationFailure(&reason) {
		t.Fatalf("IsDraftPRCreationFailure(%q) = true, want false", reason)
	}
	if IsDraftPRCreationFailure(nil) {
		t.Fatal("IsDraftPRCreationFailure(nil) = true, want false")
	}
}
