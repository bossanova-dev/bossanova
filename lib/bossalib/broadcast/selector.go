// Package broadcast holds the pure, dependency-free vocabulary for addressing a
// set of agent chats: the broadcast *selector* (who receives a message), its
// textual grammar, and its guards. It has no dependency on any services/*
// package or store, so the boss CLI, the MCP tools, bossd and bosso can all
// share exactly one definition of "who is in the audience" rather than each
// re-deriving it from their own models.
//
// A Selector is a disjunction of Clauses; a Clause is a conjunction of
// dimensions; a dimension holds a set of alternative values. That is,
// OR-across-clauses, AND-across-fields, OR-within-a-field. This covers
// "same repo AND same agent" and "these two chats OR everything on that
// account" without a general boolean expression language — a deliberate scope
// choice, since the textual form can grow later without changing the clause
// shape that goes over the wire.
//
// The single most important rule here: an empty selector is a hard error, never
// "match everything". A parse/validate rule rejecting empty input is the only
// thing standing between a typo and a daemon-wide message storm.
//
// Selector strings are user-authored and carry ids, not credentials, so they
// are safe to log. A broadcast's *message body* is not, and must never be
// logged or echoed back on a read surface.
package broadcast

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Clause is a conjunction: every non-empty field must match (AND). Within a
// single field the listed values are alternatives (OR).
type Clause struct {
	ChatIDs    []string // AgentChat.AgentSessionID
	SessionIDs []string // Session.ID
	RepoIDs    []string // Session.RepoID
	AgentNames []string // AgentChat.AgentName
	AccountIDs []string // AgentChat.AccountID (the "" default account is not addressable; see Target)
	DaemonIDs  []string // AgentChat.DaemonID (the "" local daemon is not addressable; see Target)
}

// Selector is a disjunction of clauses: a candidate matches if ANY clause
// matches (OR). A Selector with no clauses is invalid, not universal.
type Selector struct{ Clauses []Clause }

const (
	// MaxClauses caps the disjunction width so a pathological selector cannot
	// be parsed, persisted, or evaluated. Parse enforces it on the raw split,
	// before parsing any clause, and Validate re-checks it at the delivery
	// boundary.
	MaxClauses = 16

	// MaxValuesPerField caps how many alternatives one dimension of one clause
	// may list, for the same reason. Parse applies it to the DISTINCT values
	// left after normalization, so duplicates do not count against this cap
	// (they are instead bounded in bulk by maxTermsPerClause); Validate applies
	// it to the entries it is handed, since it must judge the selector exactly
	// as it will be persisted and evaluated.
	MaxValuesPerField = 64

	// clauseSep separates clauses in the textual grammar; termSep separates the
	// key:value terms inside one clause. They are also two of the characters a
	// value may not contain (valueProblem has the full set): String emits values
	// raw and unescaped, so a value carrying a separator would re-parse into a
	// DIFFERENT selector — and for clauseSep, a strictly WIDER one.
	clauseSep = "+"
	termSep   = ","
)

// maxTermsPerClause bounds the raw key:value terms one clause may carry before
// normalization — every dimension at its value cap. It exists so MaxClauses and
// MaxValuesPerField bound the PARSE as well as the persisted and evaluated
// selector: the per-field cap is deliberately applied to distinct values after
// dedupe, which on its own leaves repeated terms free to make Parse allocate and
// sort without limit.
//
// It is a real narrowing, not a pure pre-filter: a clause listing the maximum
// 384 distinct terms plus one redundant duplicate would have normalized to a
// legal selector and is now refused. Nothing a caller could usefully express is
// lost — the surviving selector is identical without the duplicate — and a
// bound applied before dedupe cannot do better, since knowing which terms
// collapse is exactly the work being bounded.
var maxTermsPerClause = MaxValuesPerField * len(fields)

// maxQuotedInput bounds how much of a rejected selector, clause, term or value
// an error message repeats back. Long enough that every realistic id, agent
// name or short clause is shown whole.
const maxQuotedInput = 160

// quoteBounded renders untrusted input for an error message: %q, truncated.
//
// Every error in this file names the offending token, and the token comes from
// the caller — so without a bound, refusing a 3 MB selector builds a 3 MB error
// string, and the caps meant to stop a pathological selector from costing
// memory would hand that memory straight back on the rejection path. Truncating
// here is what makes the cap guards' no-echo rule true of the whole file rather
// than only of the two cap messages.
func quoteBounded(s string) string {
	if len(s) <= maxQuotedInput {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q... (%d bytes)", s[:maxQuotedInput], len(s))
}

// valueProblem reports why v is unusable as a selector value, or "" when it is
// fine. Parse cannot produce an offending value (it splits on the separators
// and trims), but a hand-built or wire-decoded Selector can, so Validate
// applies exactly these rules at the delivery boundary — otherwise a selector
// could pass Validate, be persisted as its canonical String, and come back from
// a re-parse addressing a wider audience.
//
// Non-printable runes are rejected for a second reason: this package's contract
// says selector strings carry ids rather than credentials and are therefore
// safe to log, and a value holding a line break would let a plugin-supplied
// agent name forge a log record. Such a rune round-trips through Parse and
// String intact, so nothing downstream would catch it.
//
// The test is unicode.IsPrint rather than unicode.IsControl on purpose:
// IsControl short-circuits to false above MaxLatin1, so it misses U+2028 LINE
// SEPARATOR (a real line break) and U+202E RIGHT-TO-LEFT OVERRIDE (a
// Trojan-Source-style visual reorder of any line that echoes the selector).
// !IsPrint is a strict superset — Cc, Cf, Zl, Zp, Co, Cs and non-ASCII spaces.
//
// Interior ASCII spaces and ":" are deliberately legal: IsPrint admits the
// ASCII space explicitly, they survive the round trip untouched, and ids are
// not the only values (agent names are plugin-supplied).
func valueProblem(v string) string {
	switch {
	case strings.TrimSpace(v) == "":
		return "is blank"
	case v != strings.TrimSpace(v):
		return "has leading or trailing whitespace"
	case strings.ContainsAny(v, clauseSep+termSep):
		return fmt.Sprintf("contains a %q or %q separator", clauseSep, termSep)
	case strings.ContainsFunc(v, func(r rune) bool { return !unicode.IsPrint(r) }):
		return "contains a non-printable character (selector strings must stay safe to log)"
	}
	return ""
}

// field describes one addressable dimension: its grammar key, how to reach the
// corresponding Clause slice, and how to read the same dimension off a
// candidate Target. Parse, String, Validate and Matches all iterate this one
// table, and the fixed emission order of String is this slice's order.
//
// It does NOT make a dimension a one-line change, and it would be dangerous to
// read it that way. A seventh dimension also needs: the Clause and Target
// fields, the proto message, the two hand-rolled conversions in proto.go, and
// the two mirror tables in the tests (protoFields, targetFor's setters). What
// the table buys is that the four *grammar and matching* functions cannot
// disagree with each other about the dimension set; the wire conversion — the
// one boundary where a missed dimension is silent rather than loud — is
// deliberately left explicit and is covered instead by
// TestProtoRoundTripCoversEveryDimension, which drives this table so the
// omission fails a test rather than shipping.
//
// value takes a Target by value rather than by pointer deliberately: an
// indirect call through a package-level closure would force a *Target argument
// to escape, and Matches is on bossd's fan-out hot path where it must not
// allocate.
type field struct {
	key   string
	get   func(*Clause) *[]string
	value func(Target) string
}

var fields = []field{
	{"chat", func(c *Clause) *[]string { return &c.ChatIDs }, func(t Target) string { return t.ChatID }},
	{"session", func(c *Clause) *[]string { return &c.SessionIDs }, func(t Target) string { return t.SessionID }},
	{"repo", func(c *Clause) *[]string { return &c.RepoIDs }, func(t Target) string { return t.RepoID }},
	{"agent", func(c *Clause) *[]string { return &c.AgentNames }, func(t Target) string { return t.AgentName }},
	{"account", func(c *Clause) *[]string { return &c.AccountIDs }, func(t Target) string { return t.AccountID }},
	{"daemon", func(c *Clause) *[]string { return &c.DaemonIDs }, func(t Target) string { return t.DaemonID }},
}

// validKeys lists the accepted grammar keys in their canonical order, for
// error messages a caller (or an agent) can self-correct from.
func validKeys() string {
	keys := make([]string, len(fields))
	for i, f := range fields {
		keys[i] = f.key
	}
	return strings.Join(keys, ", ")
}

// lookupField resolves a grammar key to its dimension.
func lookupField(key string) (field, bool) {
	for _, f := range fields {
		if f.key == key {
			return f, true
		}
	}
	return field{}, false
}

// Parse turns the textual selector grammar into a Selector.
//
// A clause is comma-separated key:value terms ("repo:<id>,agent:claude");
// clauses are separated by "+" ("repo:<id>,agent:claude+account:<id>");
// repeating a key within a clause unions its values ("agent:claude,agent:codex").
// The valid keys are chat, session, repo, agent, account and daemon.
//
// Every error names the offending token, and an unknown key additionally lists
// the valid set, because the CLI surfaces these messages verbatim.
//
// Empty or whitespace-only input is an error, never "match everything".
//
// Parsed values are normalized per field — trimmed, deduped and sorted — so a
// parsed Selector is already in String's canonical shape and
// Parse(Parse(s).String()) equals Parse(s).
func Parse(s string) (Selector, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return Selector{}, fmt.Errorf("broadcast selector %s is empty: expected at least one term such as repo:<id> (an empty selector never means everyone; valid keys are %s)", quoteBounded(s), validKeys())
	}

	// SplitN, not Split: the whole point of the cap is to bound the work, and a
	// plain Split materializes every clause of a megabyte-sized input before the
	// check can reject it. The limit is MaxClauses+2 — one slot past the first
	// over-cap count, so `len > MaxClauses` still decides correctly while the
	// unread remainder stays a single unsplit string. The message therefore says
	// "more than" rather than an exact count, and quotes nothing: quoteBounded
	// applies the same rule to every other message on this path.
	rawClauses := strings.SplitN(trimmed, clauseSep, MaxClauses+2)
	if len(rawClauses) > MaxClauses {
		return Selector{}, fmt.Errorf("broadcast selector has more than %d clauses: at most %d clauses are allowed", MaxClauses, MaxClauses)
	}

	clauses := make([]Clause, 0, len(rawClauses))
	for _, raw := range rawClauses {
		clause, err := parseClause(raw, s)
		if err != nil {
			return Selector{}, err
		}
		clauses = append(clauses, clause)
	}

	selector := Selector{Clauses: clauses}
	// Belt and braces: the grammar rules above already guarantee this, but a
	// selector must never leave Parse in a state Validate would reject.
	if err := selector.Validate(); err != nil {
		return Selector{}, err
	}
	return selector, nil
}

// parseClause parses one "+"-delimited clause. input is the whole selector
// string, quoted into errors so the caller can see which selector failed.
func parseClause(raw, input string) (Clause, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Clause{}, fmt.Errorf("broadcast selector %s contains an empty clause: clauses are separated by + and each must hold at least one key:value term", quoteBounded(input))
	}

	// Bound the work BEFORE accumulating. The per-field cap below is applied to
	// the distinct values left after normalization, so without this a clause of
	// a million repeated "agent:x" terms would be split, appended and sorted
	// only to collapse to one value and pass — the caps would bound the
	// persisted and evaluated selector but not the parse.
	//
	// This is a genuine narrowing, not a pure pre-filter, and it is worth being
	// precise about what it costs. maxTermsPerClause is the widest clause that
	// can survive normalization (every dimension at its value cap), so no
	// selector that was going to be USEFUL is refused — but a clause of 384
	// distinct terms plus one redundant duplicate was accepted before and is
	// refused now. That is the price of capping duplicate work at all: a bound
	// on raw terms cannot know which of them will collapse. It is also why this
	// message is the coarse one — above the bound, the per-field pass has not
	// run, so it cannot say which dimension overflowed. Below the bound (the
	// case an operator actually hits) the specific per-field message still
	// wins.
	//
	// SplitN for the same reason as the clause split above (same +2 limit, same
	// reasoning): stop just past the bound instead of materializing every term
	// first. The message quotes nothing for the same reason either.
	rawTerms := strings.SplitN(text, termSep, maxTermsPerClause+2)
	if len(rawTerms) > maxTermsPerClause {
		return Clause{}, fmt.Errorf("broadcast selector clause has more than %d terms: at most %d are allowed (%d keys x %d values)", maxTermsPerClause, maxTermsPerClause, len(fields), MaxValuesPerField)
	}

	var clause Clause
	for _, rawTerm := range rawTerms {
		term := strings.TrimSpace(rawTerm)
		if term == "" {
			return Clause{}, fmt.Errorf("broadcast selector clause %s contains an empty term: terms are separated by , and each must be key:value", quoteBounded(text))
		}
		key, value, found := strings.Cut(term, ":")
		if !found {
			return Clause{}, fmt.Errorf("broadcast selector term %s is not key:value: valid keys are %s", quoteBounded(term), validKeys())
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		f, known := lookupField(key)
		if !known {
			return Clause{}, fmt.Errorf("broadcast selector term %s uses unknown key %q: valid keys are %s", quoteBounded(term), key, validKeys())
		}
		if problem := valueProblem(value); problem != "" {
			return Clause{}, fmt.Errorf("broadcast selector term %s has an invalid %q value: it %s", quoteBounded(term), key, problem)
		}
		values := f.get(&clause)
		*values = append(*values, value)
	}

	for _, f := range fields {
		values := f.get(&clause)
		slices.Sort(*values)
		*values = slices.Compact(*values)
		if len(*values) > MaxValuesPerField {
			return Clause{}, fmt.Errorf("broadcast selector clause %s lists %d values for key %q: at most %d are allowed", quoteBounded(text), len(*values), f.key, MaxValuesPerField)
		}
	}
	return clause, nil
}

// String renders the Selector in its canonical textual form: keys in a fixed
// order, values sorted, clauses in their original order. It does not mutate the
// receiver.
//
// For a NORMALIZED, Validate-accepted Selector — per-field values sorted and
// deduped, which is everything Parse returns — the result re-parses to a
// Selector with the same clauses and values, and String is byte-stable, so it
// is safe to persist and compare byte-for-byte. BOTH preconditions are load
// bearing: normalization alone does not save a selector whose values carry a
// clause or term separator, since String emits values raw and unescaped and
// "chat:a+chat:b" re-parses strictly wider. Only Validate rules that out, so
// normalize-then-persist without validating is not a licensed shortcut.
//
// A hand-built or wire-decoded Selector need not be normalized: Validate
// accepts unsorted and duplicated values, and SelectorFromProto deliberately
// preserves whatever arrived rather than tidying it, so the delivery boundary
// judges the selector exactly as it was sent. For those the re-parse is equal
// only UP TO normalization — same audience, values sorted and deduped — and two
// selectors with the same audience can render differently ("chat:a,chat:a" vs
// "chat:a"). A caller wanting an identity or dedup key must therefore key on
// Parse(s.String()) (or its String), never on the pre-normalization form. If a
// later child needs that as a first-class operation, the shape to add is an
// exported Normalize, not a quietly normalizing decoder.
//
// The zero Selector formats to the empty string — which Parse rejects, by
// design: an empty selector is never a valid audience.
func (s Selector) String() string {
	clauses := make([]string, 0, len(s.Clauses))
	for i := range s.Clauses {
		clause := &s.Clauses[i]
		var terms []string
		for _, f := range fields {
			values := slices.Clone(*f.get(clause))
			slices.Sort(values)
			for _, v := range values {
				terms = append(terms, f.key+":"+v)
			}
		}
		clauses = append(clauses, strings.Join(terms, termSep))
	}
	return strings.Join(clauses, clauseSep)
}

// Validate reports whether the Selector is safe to persist and deliver against:
// it must hold at least one clause, every clause must constrain at least one
// dimension, every value must satisfy valueProblem (non-blank, unpadded, free
// of the "+" / "," separators so the canonical String survives a re-parse, and
// free of non-printable runes so the string stays safe to log), and the
// MaxClauses / MaxValuesPerField caps must hold. Parse
// guarantees all of this, but a
// Selector can also arrive from the wire or be built by hand, so the delivery
// boundary re-checks it — an unvalidated selector must never reach delivery.
func (s Selector) Validate() error {
	if len(s.Clauses) == 0 {
		return fmt.Errorf("broadcast selector has no clauses: at least one clause is required (an empty selector never means everyone)")
	}
	if len(s.Clauses) > MaxClauses {
		return fmt.Errorf("broadcast selector has %d clauses: at most %d clauses are allowed", len(s.Clauses), MaxClauses)
	}
	for i := range s.Clauses {
		clause := &s.Clauses[i]
		populated := 0
		for _, f := range fields {
			values := *f.get(clause)
			if len(values) == 0 {
				continue
			}
			if len(values) > MaxValuesPerField {
				return fmt.Errorf("broadcast selector clause %d lists %d values for key %q: at most %d are allowed", i+1, len(values), f.key, MaxValuesPerField)
			}
			for _, v := range values {
				if problem := valueProblem(v); problem != "" {
					return fmt.Errorf("broadcast selector clause %d has an invalid %q value %s: it %s", i+1, f.key, quoteBounded(v), problem)
				}
			}
			populated++
		}
		if populated == 0 {
			return fmt.Errorf("broadcast selector clause %d constrains nothing: every clause must set at least one of %s", i+1, validKeys())
		}
	}
	return nil
}
