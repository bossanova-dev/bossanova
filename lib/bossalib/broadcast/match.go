package broadcast

import "slices"

// Target is the flattened, storage-free view of one candidate chat: the six
// addressable dimensions and nothing else.
//
// It deliberately is not models.AgentChat. bossd builds a []Target from its own
// session/chat stores and bosso builds one from its registry, so the matcher
// itself stays free of any store, context or services/* dependency and both
// sides share exactly one definition of "in the audience". It is also the single
// place that has to follow if a later refactor moves AccountID or DaemonID off
// the underlying models.
//
// Two dimensions carry a meaningful empty value: AccountID "" is the
// system-default account 0 and DaemonID "" is the local daemon. Matches treats
// those as ordinary values rather than wildcards, so a clause naming a real id
// never sweeps them up.
//
// They are deliberately NOT addressable: the grammar rejects a blank value and
// Validate rejects a blank entry, so no selector that reaches delivery can list
// "". A default-account or local-daemon chat is therefore reached through the
// other four dimensions (chat/session/repo/agent). Matches still handles a ""
// entry correctly because an unvalidated wire-decoded Selector can carry one;
// addressing the default account by name would need an explicit grammar
// sentinel (account:default / daemon:local), which is a later child's decision
// and not something a blank value should back into.
type Target struct {
	ChatID    string // AgentChat.AgentSessionID
	SessionID string // Session.ID
	RepoID    string // Session.RepoID
	AgentName string // AgentChat.AgentName
	AccountID string // AgentChat.AccountID ("" = system-default account 0; not addressable)
	DaemonID  string // AgentChat.DaemonID ("" = local; not addressable)
}

// Matches reports whether the target is in the selector's audience: OR across
// clauses, AND across the constrained dimensions of a clause, OR across the
// values of one dimension. A dimension a clause leaves unset is not a
// constraint and does not restrict the clause.
//
// Matches is total — it never errors and never panics — and allocates nothing,
// because bossd evaluates it once per live chat on every fan-out.
//
// A selector with no clauses, and a clause that constrains no dimension, match
// NOTHING rather than everything. Validate already rejects both, so they only
// arise from an unvalidated hand-built or wire-decoded value — precisely the
// case where "everyone" would turn a typo into a daemon-wide message storm.
func (s Selector) Matches(t Target) bool {
	for i := range s.Clauses {
		if clauseMatches(&s.Clauses[i], t) {
			return true
		}
	}
	return false
}

// clauseMatches evaluates one clause. It returns false for a clause that
// constrains nothing, so an all-empty clause can never widen a selector.
func clauseMatches(c *Clause, t Target) bool {
	constrained := false
	for _, f := range fields {
		values := *f.get(c)
		if len(values) == 0 {
			continue
		}
		constrained = true
		if !slices.Contains(values, f.value(t)) {
			return false
		}
	}
	return constrained
}

// Filter returns the targets the selector matches, in their input order. It
// never mutates or aliases targets: callers keep using the slice they passed in.
func (s Selector) Filter(targets []Target) []Target {
	var kept []Target
	for _, t := range targets {
		if s.Matches(t) {
			kept = append(kept, t)
		}
	}
	return kept
}
