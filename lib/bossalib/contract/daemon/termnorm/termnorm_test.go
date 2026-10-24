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

func TestNormalizeWithResolvableTermKeepsRealTERM(t *testing.T) {
	p := func(term string) bool { return term == "xterm-ghostty" }

	in := []string{"PATH=/bin", "TERM=xterm-ghostty", "HOME=/root"}
	out := NormalizeWith(in, p)

	if in[1] != "TERM=xterm-ghostty" {
		t.Fatalf("NormalizeWith mutated caller slice: %q", in[1])
	}
	if out[1] != "TERM=xterm-ghostty" {
		t.Fatalf("NormalizeWith out = %q, want TERM=xterm-ghostty", out[1])
	}
}

func TestNormalizeWithUnresolvableTermFallsBack(t *testing.T) {
	p := func(term string) bool { return false }

	in := []string{"PATH=/bin", "TERM=xterm-ghostty", "HOME=/root"}
	out := NormalizeWith(in, p)

	if out[1] != "TERM="+FallbackTERM {
		t.Fatalf("NormalizeWith out = %q, want TERM=%s", out[1], FallbackTERM)
	}
}

func TestNormalizeWithAbsentTermAppendsFallbackWithoutConsultingProber(t *testing.T) {
	var consulted []string
	p := func(term string) bool {
		consulted = append(consulted, term)
		return true
	}

	out := NormalizeWith([]string{"PATH=/bin"}, p)
	found := ""
	for _, e := range out {
		if len(e) >= 5 && e[:5] == "TERM=" {
			found = e
		}
	}
	if found != "TERM="+FallbackTERM {
		t.Fatalf("NormalizeWith appended %q, want TERM=%s", found, FallbackTERM)
	}
	if len(consulted) != 0 {
		t.Fatalf("NormalizeWith consulted prober for terms %v, want none", consulted)
	}
}

func TestEffectiveWithResolvableAndUnresolvableTerm(t *testing.T) {
	tests := []struct {
		name string
		p    Prober
		want string
	}{
		{"resolvable", func(term string) bool { return true }, "xterm-ghostty"},
		{"unresolvable", func(term string) bool { return false }, FallbackTERM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveWith("xterm-ghostty", tt.p); got != tt.want {
				t.Fatalf("EffectiveWith(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestNormalizeRoutesThroughPackageProbeAtCallTime(t *testing.T) {
	restore := probe
	var consulted []string
	probe = func(term string) bool {
		consulted = append(consulted, term)
		return true
	}
	defer func() { probe = restore }()

	Normalize([]string{"TERM=xterm-ghostty"})

	if len(consulted) != 1 || consulted[0] != "xterm-ghostty" {
		t.Fatalf("Normalize did not consult package var probe at call time: %v", consulted)
	}
}
