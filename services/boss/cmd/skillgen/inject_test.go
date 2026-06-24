package skillgen

import (
	"strings"
	"testing"
)

func TestInjectReplacesBetweenMarkers(t *testing.T) {
	skill := "# Title\n\nintro\n\n" + BeginMarker + "\nOLD\n" + EndMarker + "\n\n## Hand-authored\n\ntail\n"
	got, err := Inject(skill, "## New\n\nbody\n")
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if strings.Contains(got, "OLD") {
		t.Error("old generated content not removed")
	}
	if !strings.Contains(got, "## New") || !strings.Contains(got, "## Hand-authored") {
		t.Error("must replace generated region and keep hand-authored tail")
	}
	if !strings.Contains(got, BeginMarker) || !strings.Contains(got, EndMarker) {
		t.Error("markers must be preserved")
	}
	// Idempotent: injecting the same body again is a no-op.
	again, err := Inject(got, "## New\n\nbody\n")
	if err != nil {
		t.Fatalf("Inject (2nd): %v", err)
	}
	if again != got {
		t.Error("Inject is not idempotent")
	}
}

func TestInjectErrorsOnMissingMarkers(t *testing.T) {
	if _, err := Inject("no markers here", "x"); err == nil {
		t.Error("expected error when markers absent")
	}
	dup := BeginMarker + "\n" + BeginMarker + "\n" + EndMarker
	if _, err := Inject(dup, "x"); err == nil {
		t.Error("expected error on duplicate BEGIN marker")
	}
	dupEnd := BeginMarker + "\n" + EndMarker + "\n" + EndMarker
	if _, err := Inject(dupEnd, "x"); err == nil {
		t.Error("expected error on duplicate END marker")
	}
}

func TestInjectErrorsWhenEndPrecedesBegin(t *testing.T) {
	reversed := EndMarker + "\nstuff\n" + BeginMarker
	if _, err := Inject(reversed, "x"); err == nil {
		t.Error("expected error when END precedes BEGIN")
	}
}
