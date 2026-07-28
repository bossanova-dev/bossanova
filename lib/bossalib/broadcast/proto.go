package broadcast

import (
	"slices"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// This file is the one place the Selector <-> wire mapping lives, so no
// consumer (the boss CLI, the MCP tools, bossd, bosso) hand-rolls it and drifts.
//
// MESSAGE-BODY HYGIENE, established here for the whole broadcast epic:
// pb.Broadcast.Message is a SECRET. It is stored verbatim and must NEVER be
// echoed back on any read surface — not in a list/get response, not in a log
// line, not in an error detail. That is precisely why this file converts only
// the selector: a Broadcast-to-anything helper would be the obvious place for a
// body to leak into a read path. The precedent to copy when a read surface does
// need a Broadcast shape is the deliberately body-free githubCallbackJSON in
// services/boss/cmd/callback.go.
//
// A selector, by contrast, carries ids rather than credentials, so selector
// strings are safe to log.

// SelectorToProto converts a Selector to its wire form. The result shares no
// backing array with the selector, so a caller may mutate either freely.
//
// The zero Selector converts to a message with no clauses — which
// SelectorFromProto decodes back to the zero Selector, and which Validate and
// Matches both refuse. An absent selector is never "everyone".
func SelectorToProto(s Selector) *pb.BroadcastSelector {
	msg := &pb.BroadcastSelector{}
	if len(s.Clauses) == 0 {
		return msg
	}
	msg.Clauses = make([]*pb.BroadcastSelectorClause, 0, len(s.Clauses))
	for i := range s.Clauses {
		c := &s.Clauses[i]
		msg.Clauses = append(msg.Clauses, &pb.BroadcastSelectorClause{
			ChatIds:    slices.Clone(c.ChatIDs),
			SessionIds: slices.Clone(c.SessionIDs),
			RepoIds:    slices.Clone(c.RepoIDs),
			AgentNames: slices.Clone(c.AgentNames),
			AccountIds: slices.Clone(c.AccountIDs),
			DaemonIds:  slices.Clone(c.DaemonIDs),
		})
	}
	return msg
}

// SelectorFromProto converts a wire selector to a Selector. It is total: a nil
// message, a message with no clauses, and a nil clause entry all decode to a
// value that Validate rejects and Matches refuses rather than panicking.
//
// It does NOT validate — decoding and validating are separate steps, so the
// delivery boundary always makes an explicit Validate call rather than trusting
// whatever crossed the wire. The result shares no backing array with the
// message.
func SelectorFromProto(msg *pb.BroadcastSelector) Selector {
	clauses := msg.GetClauses()
	if len(clauses) == 0 {
		return Selector{}
	}
	out := make([]Clause, 0, len(clauses))
	for _, c := range clauses {
		out = append(out, Clause{
			ChatIDs:    slices.Clone(c.GetChatIds()),
			SessionIDs: slices.Clone(c.GetSessionIds()),
			RepoIDs:    slices.Clone(c.GetRepoIds()),
			AgentNames: slices.Clone(c.GetAgentNames()),
			AccountIDs: slices.Clone(c.GetAccountIds()),
			DaemonIDs:  slices.Clone(c.GetDaemonIds()),
		})
	}
	return Selector{Clauses: out}
}
