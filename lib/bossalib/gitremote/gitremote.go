// Package gitremote decides whether a failed git remote operation is worth
// retrying, and runs a bounded backoff ladder over the ones that are.
//
// # Why this exists
//
// On 2026-08-13 a four-minute GitHub-side degradation made eight SSH handshakes
// fail inside one window while other pushes in the same window succeeded. Every
// failure surfaced as `exit status 128: git@github.com: Permission denied
// (publickey).`, so a transient transport fault was treated as terminal: five
// sessions never started, and two more reported a permanent-looking draft-PR
// failure. Nothing in the tree could tell "the remote is flapping" from "your
// key is wrong", and nothing retried.
//
// # Why the signature set must stay narrow
//
// Retrying is safe only when a second attempt cannot leave the remote in a state
// the first one did not intend. That is the whole safety argument, and it is not
// a claim that these errors are usually transient — it is a claim that retrying
// them is harmless. Every signature in transientSignatures satisfies one of two
// limbs of it:
//
//  1. No ref update was negotiated. The failure comes from TCP, OpenSSH, or
//     git's transport wrapper, before the remote agreed to touch anything, so
//     its refs are unchanged and a retry starts from scratch. Every handshake
//     and authentication signature below is of this kind.
//  2. Re-running the operation converges on the same result. "RPC failed",
//     "early EOF", and "remote end hung up" (in its "unexpectedly" wording) can
//     also fire AFTER receive-pack has begun — a proxy 502 between the pack
//     transfer and the status report leaves the update applied but unreported —
//     so limb 1 does not cover them. What makes them safe is that pushing the
//     same commit to the same ref a second time either fast-forwards to where
//     the remote already is or reports "Everything up-to-date".
//
// One entry sits on both limbs: "remote end hung up" is truncated to cover git's
// two hangup wordings, and the "upon initial contact" one is a limb-1 failure.
// Its comment below splits them.
//
// Limb 2 is narrower than it looks: it holds for a plain push of a known commit,
// and NOT for an operation whose outcome depends on the remote's prior state.
// `--force-with-lease` is the case to watch — its lease expectation must be
// re-derived between attempts by whatever wires the retry, never assumed to
// survive a first try that may have partly landed.
//
// A signature that satisfies neither limb does not belong here, however
// transient it looks. Anyone adding an entry owes that argument for it, entry by
// entry, naming which limb carries it — that is why each literal below carries a
// comment naming the layer that emits it.
//
// The negative side of the contract is equally load-bearing and is pinned in the
// tests: a rejected push (non-fast-forward, stale info, protected branch), the
// distinctive line printed when a remote does not resolve to a repository
// ("does not appear to be a git repository", "ERROR: Repository not found."),
// and a local index.lock are all TERMINAL. Retrying those burns a session-start
// budget and hides a real misconfiguration.
//
// Read that as a claim about those distinctive lines alone, because they are what
// the classifier is contracted on. It does NOT extend to the full stderr git
// emits around them: any SSH-phase terminal failure has git's own "fatal: Could
// not read from remote repository." appended after the distinctive line, and that
// line is transient signature #2, so the composite classifies transient. The
// overlap is a deliberate, bounded cost — DefaultPolicy spends ~4s and then
// AttemptsError surfaces the real stderr — characterized in
// TestTerminalCompositesClassifyTransientAsAcceptedCost.
//
// # Matching is a case-sensitive substring test
//
// The predicate is exposed at the string level, not as a typed error, because
// both consumers only ever hold text: the git manager collapses git's stderr
// into a wrapped error's message, and session blocked reasons are persisted as a
// plain string. One definition, shared by both, is the point of this package.
// Matching is case-sensitive because git and OpenSSH emit these with stable
// casing; a case-insensitive match would widen the set for no benefit.
package gitremote

import "strings"

// transientSignatures is the fixed set of stderr fragments that mark a git
// remote failure as worth one more attempt. Each entry names the layer that
// emits it and which limb of the package doc's two-limb safety argument carries
// it, so a reviewer can audit that argument entry by entry.
//
// Adding to this list widens what bossanova retries. Do not do it without making
// that same argument for the new entry.
var transientSignatures = []string{
	// OpenSSH client, authentication phase. Ambiguous by nature — a permanently
	// broken key emits it too — which is exactly why the ladder is bounded: a
	// truly misconfigured host pays ~4 wasted seconds and then reports itself.
	"Permission denied (publickey)",

	// git's transport wrapper, printed after the ssh/https child exits non-zero.
	// The connection never produced a usable pack stream, so no ref was touched.
	"Could not read from remote repository",

	// OpenSSH key exchange, before authentication even begins. The canonical
	// signature of a load-shedding or rate-limiting SSH frontend.
	"kex_exchange_identification",

	// OpenSSH: the peer dropped the connection during the handshake.
	"Connection closed by",

	// TCP reset surfaced by ssh or curl mid-handshake.
	"Connection reset by",

	// TCP connect timeout (the macOS wording); the remote was never reached.
	// Linux/glibc says "Connection timed out" instead, which is deliberately not
	// listed: ssh's connect failure makes git append its own "fatal: Could not
	// read from remote repository." line, and both consumers hold the whole
	// multi-line stderr, so the composite is caught by that signature. Do not
	// "fix" this by adding the Linux wording without re-checking that.
	"Operation timed out",

	// Truncated on purpose to cover BOTH of git's hangup wordings, which come
	// from different phases and so sit on different limbs:
	//   - "...hung up upon initial contact" — connect.c's die_initial_contact,
	//     when git read part of the ref advertisement and the stream then ended
	//     without a flush packet. Limb 1: nothing was negotiated. That function's
	//     other branch, taken when nothing at all was read, prints "Could not
	//     read from remote repository." INSTEAD — so this wording arrives alone,
	//     and unlike the set's other transport-phase gaps it has no composite
	//     second line to be caught by. The substring is what covers it.
	//   - "...hung up unexpectedly" — git's pkt-line reader, on an EOF mid-stream
	//     rather than at initial contact. Limb 2: this can fire after
	//     receive-pack started, so safety rests on re-pushing the same commit to
	//     the same ref converging, not on nothing having landed.
	// The two are not alternatives within one function; do not collapse their
	// explanations. Shortening the literal widens the transient set by exactly
	// the initial-contact wording: those are the only two strings in the git
	// binary containing this substring, and no negative-corpus string does.
	"remote end hung up",

	// git's smart-HTTP transport, wrapping a curl-level transfer failure. Limb 2
	// for the same reason: an HTTP error can arrive after the pack was accepted.
	"RPC failed",

	// git's pack reader hit EOF before the stream was complete. Limb 2 — the
	// truncation may be in the status report rather than the pack itself.
	"early EOF",
}

// IsTransientMessage reports whether a git failure message carries one of the
// transient transport signatures documented on this package.
//
// It takes a string rather than an error because both callers hold text: a
// wrapped error's message on the git path, and a persisted blocked reason on the
// session path.
func IsTransientMessage(msg string) bool {
	for _, signature := range transientSignatures {
		if strings.Contains(msg, signature) {
			return true
		}
	}
	return false
}

// IsTransient reports whether err is a transient git remote failure. It is
// IsTransientMessage over err.Error(), so it sees wrapped causes too — the git
// manager's errors nest the stderr text several layers down. A nil error is not
// transient.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	return IsTransientMessage(err.Error())
}
