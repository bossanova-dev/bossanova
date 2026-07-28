package broadcast

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseAcceptsEveryKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want Selector
	}{
		{
			name: "single chat term",
			in:   "chat:c1",
			want: Selector{Clauses: []Clause{{ChatIDs: []string{"c1"}}}},
		},
		{
			name: "single session term",
			in:   "session:s1",
			want: Selector{Clauses: []Clause{{SessionIDs: []string{"s1"}}}},
		},
		{
			name: "single repo term",
			in:   "repo:r1",
			want: Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}}}},
		},
		{
			name: "single agent term",
			in:   "agent:claude",
			want: Selector{Clauses: []Clause{{AgentNames: []string{"claude"}}}},
		},
		{
			name: "single account term",
			in:   "account:a1",
			want: Selector{Clauses: []Clause{{AccountIDs: []string{"a1"}}}},
		},
		{
			name: "single daemon term",
			in:   "daemon:d1",
			want: Selector{Clauses: []Clause{{DaemonIDs: []string{"d1"}}}},
		},
		{
			name: "all six keys comma-joined in one clause",
			in:   "chat:c1,session:s1,repo:r1,agent:claude,account:a1,daemon:d1",
			want: Selector{Clauses: []Clause{{
				ChatIDs:    []string{"c1"},
				SessionIDs: []string{"s1"},
				RepoIDs:    []string{"r1"},
				AgentNames: []string{"claude"},
				AccountIDs: []string{"a1"},
				DaemonIDs:  []string{"d1"},
			}}},
		},
		{
			name: "repeated key unions its values",
			in:   "agent:claude,agent:codex",
			want: Selector{Clauses: []Clause{{AgentNames: []string{"claude", "codex"}}}},
		},
		{
			name: "clauses are plus-joined and order is preserved",
			in:   "repo:r1,agent:claude+account:a1",
			want: Selector{Clauses: []Clause{
				{RepoIDs: []string{"r1"}, AgentNames: []string{"claude"}},
				{AccountIDs: []string{"a1"}},
			}},
		},
		{
			name: "surrounding whitespace is trimmed",
			in:   "  repo: r1 , agent:claude  +  account:a1  ",
			want: Selector{Clauses: []Clause{
				{RepoIDs: []string{"r1"}, AgentNames: []string{"claude"}},
				{AccountIDs: []string{"a1"}},
			}},
		},
		{
			// Decision: Parse normalizes each field (dedupe + sort) so that a
			// parsed selector is already in String()'s canonical shape and the
			// Parse -> String -> Parse round trip is an equality, not just a
			// byte-stability, guarantee.
			name: "duplicate values within a field are deduped",
			in:   "agent:claude,agent:claude,agent:codex",
			want: Selector{Clauses: []Clause{{AgentNames: []string{"claude", "codex"}}}},
		},
		{
			name: "values within a field are sorted",
			in:   "agent:codex,agent:claude",
			want: Selector{Clauses: []Clause{{AgentNames: []string{"claude", "codex"}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("Parse(%q).Validate() unexpected error: %v", tt.in, err)
			}
		})
	}
}

func TestParseErrorsNameTheOffendingToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		// wantIn are substrings the message must carry: the offending token,
		// plus (for an unknown key) the full valid-key set the CLI surfaces
		// verbatim.
		wantIn []string
	}{
		{
			name:   "unknown key names the key and lists the valid set",
			in:     "epic:e1",
			wantIn: []string{`"epic"`, "chat", "session", "repo", "agent", "account", "daemon"},
		},
		{
			// "never means everyone" is asserted deliberately rather than a bare
			// "empty": deleting the empty-input guard would fall through to the
			// empty-CLAUSE error, which also says "empty" and would let this —
			// the single most important rule in the contract — pass vacuously.
			name:   "empty input is rejected, never treated as everyone",
			in:     "",
			wantIn: []string{`""`, "is empty", "never means everyone"},
		},
		{
			name:   "whitespace-only input is rejected",
			in:     "   ",
			wantIn: []string{`"   "`, "is empty", "never means everyone"},
		},
		{
			name:   "blank value names the term",
			in:     "agent:",
			wantIn: []string{`"agent:"`},
		},
		{
			name:   "whitespace-only value names the term",
			in:     "repo:r1,agent:   ",
			wantIn: []string{`"agent:"`},
		},
		{
			name:   "term without a colon names the term",
			in:     "claude",
			wantIn: []string{`"claude"`},
		},
		{
			name:   "blank key names the term",
			in:     ":r1",
			wantIn: []string{`":r1"`},
		},
		{
			// "empty clause" / "empty term" are asserted in full so the two
			// adjacent failure modes cannot satisfy each other's assertion — a
			// bare "clause" matches both messages.
			name:   "empty clause names the input",
			in:     "repo:r1++agent:claude",
			wantIn: []string{`"repo:r1++agent:claude"`, "empty clause"},
		},
		{
			name:   "trailing separator leaves an empty clause",
			in:     "repo:r1+",
			wantIn: []string{`"repo:r1+"`, "empty clause"},
		},
		{
			name:   "empty term inside a clause",
			in:     "repo:r1,,agent:claude",
			wantIn: []string{`"repo:r1,,agent:claude"`, "empty term"},
		},
		{
			name: "over-cap clauses names the cap",
			in:   overCapClauses(),
			// The full phrase, not a bare "clause": four different messages in
			// this file mention a clause, so only the phrase discriminates.
			wantIn: []string{fmt.Sprintf("at most %d clauses", MaxClauses)},
		},
		{
			name:   "over-cap values names the key and the cap",
			in:     overCapValues(),
			wantIn: []string{`"agent"`, fmt.Sprintf("%d", MaxValuesPerField)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tt.in)
			if err == nil {
				t.Fatalf("Parse(%q) = %#v, want error", tt.in, got)
			}
			if !reflect.DeepEqual(got, Selector{}) {
				t.Fatalf("Parse(%q) returned %#v alongside its error, want the zero Selector", tt.in, got)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Parse(%q) error %q does not mention %q", tt.in, err, want)
				}
			}
		})
	}
}

// overCapClauses builds an input with MaxClauses+1 clauses.
func overCapClauses() string {
	clauses := make([]string, 0, MaxClauses+1)
	for i := range MaxClauses + 1 {
		clauses = append(clauses, fmt.Sprintf("repo:r%d", i))
	}
	return strings.Join(clauses, "+")
}

// overCapValues builds a single clause carrying MaxValuesPerField+1 distinct
// agent values, so the cap trips on genuinely distinct values rather than on
// duplicates that normalization would have collapsed.
func overCapValues() string {
	terms := make([]string, 0, MaxValuesPerField+1)
	for i := range MaxValuesPerField + 1 {
		terms = append(terms, fmt.Sprintf("agent:a%d", i))
	}
	return strings.Join(terms, ",")
}

// TestParseAtCaps pins that the caps are inclusive: exactly MaxClauses clauses
// and exactly MaxValuesPerField values are accepted, so the over-cap tests
// above are proving an off-by-one-free boundary rather than a blanket refusal.
func TestParseAtCaps(t *testing.T) {
	t.Parallel()

	clauses := make([]string, 0, MaxClauses)
	for i := range MaxClauses {
		clauses = append(clauses, fmt.Sprintf("repo:r%d", i))
	}
	sel, err := Parse(strings.Join(clauses, "+"))
	if err != nil {
		t.Fatalf("Parse at MaxClauses unexpected error: %v", err)
	}
	if len(sel.Clauses) != MaxClauses {
		t.Fatalf("Parse at MaxClauses produced %d clauses, want %d", len(sel.Clauses), MaxClauses)
	}

	terms := make([]string, 0, MaxValuesPerField)
	for i := range MaxValuesPerField {
		terms = append(terms, fmt.Sprintf("agent:a%d", i))
	}
	sel, err = Parse(strings.Join(terms, ","))
	if err != nil {
		t.Fatalf("Parse at MaxValuesPerField unexpected error: %v", err)
	}
	if got := len(sel.Clauses[0].AgentNames); got != MaxValuesPerField {
		t.Fatalf("Parse at MaxValuesPerField produced %d values, want %d", got, MaxValuesPerField)
	}
}

// TestParseBoundsWorkBeforeNormalizing pins that the caps bound the PARSE, not
// just the persisted selector. The per-field cap is applied to the distinct
// values left after dedupe — deliberately — which on its own lets a clause of
// arbitrarily many repeated terms be split, appended and sorted before
// collapsing to a single value and passing. maxTermsPerClause closes that.
//
// It pins the trade honestly rather than only the happy half: the bound is
// inclusive, it is a REAL narrowing (a saturated clause plus one redundant
// duplicate used to parse and no longer does), and above the bound the error is
// necessarily the coarse one because the per-field pass has not run yet.
func TestParseBoundsWorkBeforeNormalizing(t *testing.T) {
	t.Parallel()

	// Duplicates past the raw-term bound are refused up front...
	dupes := strings.Repeat("agent:x,", maxTermsPerClause) + "agent:x"
	if _, err := Parse(dupes); err == nil {
		t.Fatalf("Parse of %d duplicate terms = nil error, want the raw-term bound to refuse it", maxTermsPerClause+1)
	} else if !strings.Contains(err.Error(), fmt.Sprintf("at most %d are allowed", maxTermsPerClause)) {
		t.Fatalf("Parse error %q does not name the raw-term bound", err)
	}

	// ...and the bound is inclusive, so it is a boundary rather than a blanket
	// refusal of repetition: exactly maxTermsPerClause duplicate terms still
	// normalize to the one value they describe.
	atBound := strings.Repeat("agent:x,", maxTermsPerClause-1) + "agent:x"
	sel, err := Parse(atBound)
	if err != nil {
		t.Fatalf("Parse at maxTermsPerClause unexpected error: %v", err)
	}
	if want := []string{"x"}; !reflect.DeepEqual(sel.Clauses[0].AgentNames, want) {
		t.Fatalf("Parse at maxTermsPerClause = %#v, want %#v", sel.Clauses[0].AgentNames, want)
	}

	// Below the bound — the case an operator actually hits — the per-field cap
	// still wins and still names the offending dimension.
	if _, err := Parse(overCapValues()); err == nil {
		t.Fatal("Parse(overCapValues()) = nil error, want the per-field cap to refuse it")
	} else if !strings.Contains(err.Error(), `"agent"`) {
		t.Fatalf("per-field cap message %q no longer names the key", err)
	}

	// Above the bound it deliberately does NOT: the per-field pass has not run,
	// so the message is the coarse one. Pinned so the trade is a documented
	// contract rather than a surprise to whoever hits it.
	tooManyDistinct := make([]string, 0, maxTermsPerClause+1)
	for i := range maxTermsPerClause + 1 {
		tooManyDistinct = append(tooManyDistinct, fmt.Sprintf("chat:c%d", i))
	}
	_, err = Parse(strings.Join(tooManyDistinct, ","))
	if err == nil {
		t.Fatal("Parse of an over-bound distinct clause = nil error, want an error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("at most %d are allowed", maxTermsPerClause)) {
		t.Fatalf("over-bound error %q is not the coarse raw-term message", err)
	}
	if strings.Contains(err.Error(), `"chat"`) {
		t.Fatalf("over-bound error %q now names the dimension; if the per-field pass runs first, drop this case and tighten the message contract instead", err)
	}
}

// TestErrorsDoNotEchoAnUnboundedInput pins the other half of the cap story.
// Every error in the package names the offending token, and the token is
// caller-supplied, so an unbounded %q would hand back on the rejection path
// exactly the memory the caps exist to deny — refusing a 3 MB selector by
// building a 3 MB error string. These are the branches a cap-sized guard does
// NOT gate, so they are the ones that need quoteBounded.
func TestErrorsDoNotEchoAnUnboundedInput(t *testing.T) {
	t.Parallel()
	const huge = 3 << 20
	tests := []struct {
		name string
		in   string
	}{
		{"whitespace-only input", strings.Repeat(" ", huge)},
		{"one enormous term with no colon", strings.Repeat("x", huge)},
		// The line break has to be INTERIOR: a trailing one is trimmed off the
		// term and what is left is a perfectly legal (enormous) value.
		{"one enormous invalid value", "agent:x\n" + strings.Repeat("x", huge)},
		{"an enormous empty-term clause", "repo:r1,," + strings.Repeat("x", huge)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tt.in)
			if err == nil {
				t.Fatalf("Parse of a %d-byte input = nil error, want an error", len(tt.in))
			}
			// The control: the input really is enormous, so an unbounded quote
			// would have produced a message of that order.
			if len(tt.in) <= maxQuotedInput {
				t.Fatalf("input is only %d bytes; this case no longer proves anything", len(tt.in))
			}
			if got := len(err.Error()); got > 4*maxQuotedInput {
				t.Fatalf("error message is %d bytes for a %d-byte input: it is echoing the input", got, len(tt.in))
			}
		})
	}

	// Bounding must not cost the diagnostic: a realistic token is still quoted
	// whole, which is what TestParseErrorsNameTheOffendingToken relies on.
	_, err := Parse("epic:e1")
	if err == nil {
		t.Fatal("Parse(\"epic:e1\") = nil error, want an error")
	}
	if !strings.Contains(err.Error(), `"epic:e1"`) {
		t.Fatalf("error %q no longer quotes a short token whole", err)
	}
}

func TestQuoteBoundedIncludesInputAtLimit(t *testing.T) {
	t.Parallel()

	input := strings.Repeat("x", maxQuotedInput)
	if got, want := quoteBounded(input), fmt.Sprintf("%q", input); got != want {
		t.Fatalf("quoteBounded(%d-byte input) = %q, want %q", len(input), got, want)
	}
}

// roundTripCorpus is the shared corpus for the String/Parse stability tests.
var roundTripCorpus = []string{
	"chat:c1",
	"session:s1",
	"repo:r1",
	"agent:claude",
	"account:a1",
	"daemon:d1",
	"chat:c1,session:s1,repo:r1,agent:claude,account:a1,daemon:d1",
	"agent:codex,agent:claude",
	"repo:r1,agent:claude+account:a1",
	"repo:r2+repo:r1",
	"daemon:d2,daemon:d1+chat:c9,chat:c8,chat:c7",
	"  repo: r1 , agent:claude  +  account:a1  ",
	"agent:claude,agent:claude,agent:codex",
}

func TestParseStringRoundTrip(t *testing.T) {
	t.Parallel()
	for _, in := range roundTripCorpus {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			first, err := Parse(in)
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", in, err)
			}
			formatted := first.String()
			second, err := Parse(formatted)
			if err != nil {
				t.Fatalf("Parse(%q) (re-parse of %q) unexpected error: %v", formatted, in, err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("round trip of %q: Parse = %#v, Parse(String) = %#v", in, first, second)
			}
			if again := second.String(); again != formatted {
				t.Fatalf("String is not byte-stable for %q: %q then %q", in, formatted, again)
			}
		})
	}
}

func TestStringCanonicalForm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sel  Selector
		want string
	}{
		{
			name: "keys emit in fixed order regardless of construction",
			sel: Selector{Clauses: []Clause{{
				DaemonIDs:  []string{"d1"},
				AccountIDs: []string{"a1"},
				AgentNames: []string{"claude"},
				RepoIDs:    []string{"r1"},
				SessionIDs: []string{"s1"},
				ChatIDs:    []string{"c1"},
			}}},
			want: "chat:c1,session:s1,repo:r1,agent:claude,account:a1,daemon:d1",
		},
		{
			name: "values are sorted even when the selector was built by hand",
			sel:  Selector{Clauses: []Clause{{AgentNames: []string{"codex", "claude"}}}},
			want: "agent:claude,agent:codex",
		},
		{
			name: "clause order is preserved",
			sel: Selector{Clauses: []Clause{
				{RepoIDs: []string{"r2"}},
				{RepoIDs: []string{"r1"}},
			}},
			want: "repo:r2+repo:r1",
		},
		{
			name: "zero selector formats to the empty string",
			sel:  Selector{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.sel.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStringDoesNotMutateReceiver guards the sorting done for canonical output:
// it must operate on a copy, because callers hold selectors that later children
// persist and compare.
func TestStringDoesNotMutateReceiver(t *testing.T) {
	t.Parallel()
	sel := Selector{Clauses: []Clause{{AgentNames: []string{"codex", "claude"}}}}
	_ = sel.String()
	want := []string{"codex", "claude"}
	if !reflect.DeepEqual(sel.Clauses[0].AgentNames, want) {
		t.Fatalf("String() mutated the receiver: AgentNames = %#v, want %#v", sel.Clauses[0].AgentNames, want)
	}
}

// TestValidateRejectsValuesThatBreakTheCanonicalForm pins the reason the value
// rules exist rather than just their existence. String emits values raw and
// unescaped, so a value carrying a clause ("+") or term (",") separator — or
// padding whitespace — makes the canonical form re-parse into a DIFFERENT
// selector, and for "+" a strictly WIDER one. Parse cannot produce such a
// value, but SelectorFromProto and a hand-built literal both can, so Validate
// is the only thing standing between "persist the canonical string" and a
// silently widened audience.
//
// Each case asserts BOTH halves: Validate rejects the value, and — as the
// negative control that keeps the first assertion from being a tautology — the
// round trip it would otherwise have produced genuinely disagrees with the
// original.
func TestValidateRejectsValuesThatBreakTheCanonicalForm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"clause separator widens the audience", "c1+repo:r1"},
		{"term separator narrows the audience", "c1,repo:r1"},
		{"bare clause separator", "c1+c2"},
		{"leading whitespace does not survive", " c1"},
		{"trailing whitespace does not survive", "c1 "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sel := Selector{Clauses: []Clause{{ChatIDs: []string{tt.value}}}}
			if err := sel.Validate(); err == nil {
				t.Fatalf("Validate() = nil for chat value %q, want error", tt.value)
			}
			// Negative control: the value really is unsafe, so a Validate that
			// let it through would be letting through a broken round trip.
			reparsed, err := Parse(sel.String())
			if err == nil && reflect.DeepEqual(reparsed, sel) {
				t.Fatalf("value %q round-trips cleanly (%q); this case no longer proves anything", tt.value, sel.String())
			}
		})
	}

	// The widening case, spelled out: the original matches nothing with that
	// repo, the re-parse of its own canonical form does.
	sel := Selector{Clauses: []Clause{{ChatIDs: []string{"c1+repo:r1"}}}}
	reparsed, err := Parse(sel.String())
	if err != nil {
		t.Fatalf("Parse(%q) unexpected error: %v", sel.String(), err)
	}
	stranger := Target{ChatID: "cX", RepoID: "r1"}
	if sel.Matches(stranger) {
		t.Fatalf("the original selector already matches %#v; the widening case is not set up", stranger)
	}
	if !reparsed.Matches(stranger) {
		t.Fatal("re-parsing the canonical form no longer widens the audience; this guard no longer proves anything")
	}
}

// TestValueRulesKeepSelectorStringsSafeToLog pins the second reason for the
// value rules. The package contract says selector strings carry ids rather than
// credentials and are therefore safe to log; a control character breaks that
// claim, and unlike the separator cases it does NOT announce itself — it
// round-trips through Parse and String intact, so a value like
// "claude\nforged log line" would reach a log record unchanged. Agent names are
// plugin-supplied and a wire-decoded selector is unvalidated until the delivery
// boundary, so this is the boundary that has to say no.
//
// The assertion is paired with the round-trip fact deliberately: the "it
// survives untouched" half is what makes rejecting it necessary rather than
// merely tidy.
func TestValueRulesKeepSelectorStringsSafeToLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"line feed forges a log record", "claude\nforged log line"},
		{"carriage return", "claude\rforged"},
		{"tab", "claude\tcodex"},
		{"NUL", "claude\x00codex"},
		{"escape (terminal control sequence)", "claude\x1b[31m"},
		// Above MaxLatin1, where unicode.IsControl short-circuits to false.
		// U+2028 is a genuine line break and U+202E reorders every line that
		// echoes the selector, so a Latin-1-only test would leave the two
		// nastiest cases in this class open.
		{"U+2028 line separator", "claude\u2028forged log line"},
		{"U+202E right-to-left override", "claude\u202eforged"},
		{"U+200B zero-width space", "claude\u200bcodex"},
		{"U+00A0 non-breaking space", "claude\u00a0codex"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sel := Selector{Clauses: []Clause{{AgentNames: []string{tt.value}}}}
			if err := sel.Validate(); err == nil {
				t.Fatalf("Validate() = nil for agent value %q, want error", tt.value)
			}
			if _, err := Parse("agent:" + tt.value); err == nil {
				t.Fatalf("Parse(%q) = nil error, want the grammar to reject it too", "agent:"+tt.value)
			}
			// The negative control: String still emits the value verbatim, so
			// nothing downstream sanitizes it and these guards are the only
			// thing standing between a plugin-supplied name and a log record.
			if !strings.Contains(sel.String(), tt.value) {
				t.Fatalf("String() = %q no longer carries %q verbatim; this case no longer proves anything", sel.String(), tt.value)
			}
		})
	}
}

// TestCanonicalFormOfDenormalizedSelectorsNormalizes covers the other shape a
// wire-decoded selector arrives in: Validate accepts unsorted and duplicated
// values, and SelectorFromProto preserves them, so String's round trip is an
// equality only UP TO normalization. The pre-normalization form is therefore
// NOT an identity key — "chat:a,chat:a" and "chat:a" address the same audience
// — which is exactly the trap a delivery child using String() as a dedup key
// would fall into. What does hold is that one round trip reaches a fixpoint and
// normalizes to a known selector, which the DeepEqual below pins exactly; the
// Matches check is the spot-sample that the resulting audience still contains
// each clause's own target.
func TestCanonicalFormOfDenormalizedSelectorsNormalizes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sel  Selector
		want Selector
	}{
		{
			name: "unsorted values come back sorted",
			sel:  Selector{Clauses: []Clause{{ChatIDs: []string{"b", "a"}}}},
			want: Selector{Clauses: []Clause{{ChatIDs: []string{"a", "b"}}}},
		},
		{
			name: "duplicate values come back deduped",
			sel:  Selector{Clauses: []Clause{{ChatIDs: []string{"a", "a"}}}},
			want: Selector{Clauses: []Clause{{ChatIDs: []string{"a"}}}},
		},
		{
			name: "unsorted duplicates across two dimensions",
			sel: Selector{Clauses: []Clause{{
				AgentNames: []string{"codex", "claude", "codex"},
				RepoIDs:    []string{"r2", "r1"},
			}}},
			want: Selector{Clauses: []Clause{{
				RepoIDs:    []string{"r1", "r2"},
				AgentNames: []string{"claude", "codex"},
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.sel.Validate(); err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
			got, err := Parse(tt.sel.String())
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.sel.String(), err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse(%q) = %#v, want the normalized %#v", tt.sel.String(), got, tt.want)
			}
			// The audience is unchanged: a target built from each clause still
			// matches both forms.
			for i := range tt.sel.Clauses {
				target := targetFor(t, &tt.sel.Clauses[i])
				if !tt.sel.Matches(target) || !got.Matches(target) {
					t.Fatalf("audience changed for %#v: original=%v normalized=%v", target, tt.sel.Matches(target), got.Matches(target))
				}
			}
			// And normalization is idempotent from here on.
			refetched, err := Parse(got.String())
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", got.String(), err)
			}
			if !reflect.DeepEqual(refetched, got) {
				t.Fatalf("normalized form is not a fixpoint: %#v, want %#v", refetched, got)
			}
		})
	}
}

// targetFor builds a Target that satisfies every dimension the clause
// constrains, using each dimension's first listed value.
//
// It pairs with the fields table by grammar key rather than by index, so a
// reorder of fields is harmless, and it carries the same kind of drift guard
// proto_test.go's requireFieldTablesAgree does for the remaining two ways to
// desynchronize: a new dimension with no setter, and a renamed key. Without
// them the audience assertions in TestCanonicalFormOfDenormalizedSelectorsNormalizes
// would silently run against a Target built from the wrong (or no) dimension
// and pass or fail for the wrong reason.
func targetFor(t *testing.T, c *Clause) Target {
	t.Helper()
	setters := map[string]func(*Target, string){
		"chat":    func(tgt *Target, v string) { tgt.ChatID = v },
		"session": func(tgt *Target, v string) { tgt.SessionID = v },
		"repo":    func(tgt *Target, v string) { tgt.RepoID = v },
		"agent":   func(tgt *Target, v string) { tgt.AgentName = v },
		"account": func(tgt *Target, v string) { tgt.AccountID = v },
		"daemon":  func(tgt *Target, v string) { tgt.DaemonID = v },
	}
	if len(setters) != len(fields) {
		t.Fatalf("targetFor knows %d dimensions, fields has %d", len(setters), len(fields))
	}
	var target Target
	for _, f := range fields {
		set, ok := setters[f.key]
		if !ok {
			t.Fatalf("targetFor has no setter for dimension %q", f.key)
		}
		if values := *f.get(c); len(values) > 0 {
			set(&target, values[0])
		}
	}
	return target
}

// TestCanonicalFormRoundTripsForHandBuiltSelectors covers the selectors Parse
// cannot produce — the wire-decoded and literal-built ones — and pins that the
// value rules were not drawn too tightly: ":" and interior spaces stay legal
// because they survive the round trip untouched (a TAB does not — it is a
// control character, rejected for log safety). Its inputs are already
// normalized; TestCanonicalFormOfDenormalizedSelectorsNormalizes covers the
// unsorted/duplicated shape, for which the round trip is NOT an identity.
func TestCanonicalFormRoundTripsForHandBuiltSelectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sel  Selector
	}{
		{"single value", Selector{Clauses: []Clause{{ChatIDs: []string{"c1"}}}}},
		{"multiple values", Selector{Clauses: []Clause{{AgentNames: []string{"claude", "codex"}}}}},
		{
			name: "multiple clauses",
			sel: Selector{Clauses: []Clause{
				{RepoIDs: []string{"r1"}, AgentNames: []string{"claude"}},
				{AccountIDs: []string{"a1"}},
			}},
		},
		{"a colon inside a value", Selector{Clauses: []Clause{{ChatIDs: []string{"a:b:c"}}}}},
		{"interior whitespace inside a value", Selector{Clauses: []Clause{{RepoIDs: []string{"my repo"}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.sel.Validate(); err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
			got, err := Parse(tt.sel.String())
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.sel.String(), err)
			}
			if !reflect.DeepEqual(got, tt.sel) {
				t.Fatalf("Parse(%q) = %#v, want %#v", tt.sel.String(), got, tt.sel)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sel     Selector
		wantErr bool
	}{
		{
			name:    "zero selector is rejected",
			sel:     Selector{},
			wantErr: true,
		},
		{
			name:    "explicitly empty clause list is rejected",
			sel:     Selector{Clauses: []Clause{}},
			wantErr: true,
		},
		{
			name:    "a single all-empty clause is rejected",
			sel:     Selector{Clauses: []Clause{{}}},
			wantErr: true,
		},
		{
			name: "one populated clause alongside an empty one is rejected",
			sel: Selector{Clauses: []Clause{
				{RepoIDs: []string{"r1"}},
				{},
			}},
			wantErr: true,
		},
		{
			name:    "blank field entry is rejected",
			sel:     Selector{Clauses: []Clause{{RepoIDs: []string{""}}}},
			wantErr: true,
		},
		{
			name:    "whitespace-only field entry is rejected",
			sel:     Selector{Clauses: []Clause{{RepoIDs: []string{"  "}}}},
			wantErr: true,
		},
		{
			name:    "blank entry alongside a good one is rejected",
			sel:     Selector{Clauses: []Clause{{RepoIDs: []string{"r1", ""}}}},
			wantErr: true,
		},
		{
			name:    "over-cap clauses is rejected",
			sel:     selectorWithClauses(MaxClauses + 1),
			wantErr: true,
		},
		{
			name:    "over-cap values is rejected",
			sel:     selectorWithValues(MaxValuesPerField + 1),
			wantErr: true,
		},
		{
			name: "a good selector is accepted",
			sel: Selector{Clauses: []Clause{
				{RepoIDs: []string{"r1"}, AgentNames: []string{"claude", "codex"}},
				{AccountIDs: []string{"a1"}},
			}},
		},
		{
			name: "exactly MaxClauses is accepted",
			sel:  selectorWithClauses(MaxClauses),
		},
		{
			name: "exactly MaxValuesPerField is accepted",
			sel:  selectorWithValues(MaxValuesPerField),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.sel.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want error for %#v", tt.sel)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}
}

func TestValidateReportsOneBasedClauseForTooManyValues(t *testing.T) {
	t.Parallel()

	sel := Selector{Clauses: []Clause{
		{RepoIDs: []string{"r1"}},
		{AgentNames: make([]string, MaxValuesPerField+1)},
	}}

	err := sel.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}
	if got, want := err.Error(), "broadcast selector clause 2 lists"; !strings.Contains(got, want) {
		t.Fatalf("Validate() error = %q, want it to contain %q", got, want)
	}
}

func TestValidateReportsOneBasedClauseForInvalidValue(t *testing.T) {
	t.Parallel()

	sel := Selector{Clauses: []Clause{
		{RepoIDs: []string{"r1"}},
		{AgentNames: []string{""}},
	}}

	err := sel.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}
	if got, want := err.Error(), "broadcast selector clause 2 has"; !strings.Contains(got, want) {
		t.Fatalf("Validate() error = %q, want it to contain %q", got, want)
	}
}

func TestValidateReportsOneBasedClauseForEmptyClause(t *testing.T) {
	t.Parallel()

	sel := Selector{Clauses: []Clause{
		{RepoIDs: []string{"r1"}},
		{},
	}}

	err := sel.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}
	if got, want := err.Error(), "broadcast selector clause 2 constrains nothing"; !strings.Contains(got, want) {
		t.Fatalf("Validate() error = %q, want it to contain %q", got, want)
	}
}

func selectorWithClauses(n int) Selector {
	clauses := make([]Clause, 0, n)
	for i := range n {
		clauses = append(clauses, Clause{RepoIDs: []string{fmt.Sprintf("r%d", i)}})
	}
	return Selector{Clauses: clauses}
}

func selectorWithValues(n int) Selector {
	values := make([]string, 0, n)
	for i := range n {
		values = append(values, fmt.Sprintf("a%d", i))
	}
	return Selector{Clauses: []Clause{{AgentNames: values}}}
}
