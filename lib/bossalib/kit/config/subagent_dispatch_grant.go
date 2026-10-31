package config

import (
	"fmt"
	"strings"
)

// SubagentDispatchGrant names which boss-managed chats bossd grants the bounded
// subagent-dispatch authority to in their appended system prompt. It is a named
// type, not a bare string, so it cannot be silently transposed with the other
// string arguments at the prompt-building call sites it is threaded through.
type SubagentDispatchGrant string

const (
	// SubagentDispatchGrantAlways is the shipped default (BOS-882): unattended
	// runs and attended chats both receive the grant.
	SubagentDispatchGrantAlways SubagentDispatchGrant = "always"
	// SubagentDispatchGrantUnattended is the operator opt-out: only unattended
	// sessions receive the grant, restoring the earlier attended behaviour in
	// which an attended chat had to be re-authorised by hand each session.
	SubagentDispatchGrantUnattended SubagentDispatchGrant = "unattended"
)

// ResolveSubagentDispatchGrant maps a Settings value to the mode bossd acts on.
//
// It never errors and never panics: nil settings, an absent key, whitespace, a
// differently-cased spelling, and any UNRECOGNISED value all resolve to
// SubagentDispatchGrantAlways. That direction is deliberate — a typo in
// settings.json must not silently withdraw the shipped default grant and
// degrade every attended chat, and it must not fail daemon startup either.
// Case and surrounding whitespace are normalised before matching so that a
// plausible spelling of the opt-out ("Unattended", " unattended ") is honoured
// rather than quietly falling through to the default it was written to remove.
// It has no production caller — the spawn path goes through
// LoadSubagentDispatchGrant, which needs the discard reason this signature
// cannot return — and that is deliberate rather than dead code awaiting
// removal. It is a library package's mode accessor for callers holding a
// Settings they already loaded, and it is what lets the prompt tests assert the
// settings-string-to-mode wiring end to end without a settings file on disk.
// Delete it only together with those tests, not on a bare no-callers reading.
func ResolveSubagentDispatchGrant(s *Settings) SubagentDispatchGrant {
	if s == nil {
		return SubagentDispatchGrantAlways
	}
	grant, _ := parseSubagentDispatchGrant(s.SubagentDispatchGrant)
	return grant
}

// parseSubagentDispatchGrant is the ONE place a settings string becomes a mode,
// returning the resolved mode and — when the operator's configured value was
// DISCARDED — the reason, or "" when their configuration was honoured.
//
// Both halves come out of a single normalisation and a single switch on
// purpose. They are two views of one decision, and splitting them across two
// functions couples their correctness with nothing enforcing it: a third mode
// added to one switch and not the other makes bossd warn about a value it
// honours, or honour a value it warns about — and the warning is the operator
// signal this whole path exists to provide. One switch cannot disagree with
// itself.
//
// The discard direction is deliberate. The resolver fails open — a typo must
// not silently degrade every attended chat, and it must not fail daemon startup
// — but failing open *silently* is the defect: the discard is exactly the case
// the operator most needs told about, and the case in which the attended
// directive's claim about their configuration is least able to back itself.
//
// It returns the reason rather than logging it because config is linked into
// the boss TUI, where zerolog's default stderr writer would corrupt the Bubble
// Tea display. The caller owns the logger; see session.ResolveSubagentGrantForSpawn.
func parseSubagentDispatchGrant(raw string) (SubagentDispatchGrant, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return SubagentDispatchGrantAlways, ""
	}
	switch SubagentDispatchGrant(strings.ToLower(trimmed)) {
	case SubagentDispatchGrantUnattended:
		return SubagentDispatchGrantUnattended, ""
	case SubagentDispatchGrantAlways:
		return SubagentDispatchGrantAlways, ""
	default:
		return SubagentDispatchGrantAlways, fmt.Sprintf(
			"subagent_dispatch_grant %q is not a recognised mode (%q or %q); "+
				"falling back to %q, so an intended opt-out is NOT in effect",
			trimmed, SubagentDispatchGrantAlways, SubagentDispatchGrantUnattended, SubagentDispatchGrantAlways)
	}
}

// LoadSubagentDispatchGrant loads global settings and resolves the grant mode,
// degrading to the shipped default when settings cannot be loaded at all.
//
// Chat spawn sites call this so the resolved mode is passed into prompt
// composition as a plain value rather than re-read inside it.
//
// The second return is a discard reason ("" when the operator's configuration
// was honoured): a failed load and an unrecognised value both resolve to the
// shipped default, and neither may pass unremarked. Failing open is deliberate
// (AC 7); failing open in silence was the defect.
func LoadSubagentDispatchGrant() (SubagentDispatchGrant, string) {
	s, err := Load()
	if err != nil {
		return SubagentDispatchGrantAlways, fmt.Sprintf(
			"settings could not be loaded (%v); falling back to subagent_dispatch_grant %q, "+
				"so any configured opt-out is NOT in effect", err, SubagentDispatchGrantAlways)
	}
	return parseSubagentDispatchGrant(s.SubagentDispatchGrant)
}
