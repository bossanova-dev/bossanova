package termnorm

import (
	"testing"
)

func TestEffectiveKeepsResolvableTerm(t *testing.T) {
	restore := probe
	probe = func(term string) bool { return term == "xterm-ghostty" || term == "xterm-256color" }
	defer func() { probe = restore }()

	if got := Effective("xterm-ghostty"); got != "xterm-ghostty" {
		t.Fatalf("Effective(resolvable) = %q, want xterm-ghostty", got)
	}
}

func TestEffectiveFallsBackWhenTermMissing(t *testing.T) {
	restore := probe
	probe = func(term string) bool { return term == "xterm-256color" } // ghostty NOT present
	defer func() { probe = restore }()

	if got := Effective("xterm-ghostty"); got != "xterm-256color" {
		t.Fatalf("Effective(missing) = %q, want xterm-256color", got)
	}
}

func TestNormalizeRewritesTermInPlaceOfCopy(t *testing.T) {
	restore := probe
	probe = func(term string) bool { return term == "xterm-256color" }
	defer func() { probe = restore }()

	in := []string{"PATH=/bin", "TERM=xterm-ghostty", "HOME=/root"}
	out := Normalize(in)

	if in[1] != "TERM=xterm-ghostty" {
		t.Fatalf("Normalize mutated caller slice: %q", in[1])
	}
	if out[1] != "TERM=xterm-256color" {
		t.Fatalf("Normalize out = %q, want TERM=xterm-256color", out[1])
	}
}

func TestNormalizeAppendsWhenTermAbsent(t *testing.T) {
	restore := probe
	probe = func(term string) bool { return true }
	defer func() { probe = restore }()

	out := Normalize([]string{"PATH=/bin"})
	found := ""
	for _, e := range out {
		if len(e) >= 5 && e[:5] == "TERM=" {
			found = e
		}
	}
	if found != "TERM=xterm-256color" {
		t.Fatalf("Normalize appended %q, want TERM=xterm-256color", found)
	}
}
