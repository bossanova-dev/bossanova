// Package resumepolicy decides how the factory resumes a dead run (BOS-1537).
//
// The resume sweep and the attention producer must agree on the schedule, on
// when to stop retrying, and on the sentence a `fix` row shows, so the rules
// live here as pure functions over the persisted attempt ledger
// (session_resume_attempts). Nothing here reads the clock, the host name or
// the database: callers pass them in.
package resumepolicy

import (
	"strings"
	"time"
)

// FailureClass names why a run (or a resume attempt) died. The string values
// are the session_resume_attempts.failure_class CHECK values verbatim.
type FailureClass string

// The failure classes, in the order ClassifyFailure tests for them.
const (
	FailureAuthExpired  FailureClass = "auth_expired"
	FailureTokenRevoked FailureClass = "token_revoked"
	FailureDiskFull     FailureClass = "disk_full"
	FailureCrash        FailureClass = "crash"
	FailureRespawnCap   FailureClass = "respawn_cap"
	FailureUnknown      FailureClass = "unknown"
)

// ParseFailureClass returns the class a stored value names, and false for a
// value outside the CHECK set (the caller decides whether that is an error).
func ParseFailureClass(s string) (FailureClass, bool) {
	switch c := FailureClass(s); c {
	case FailureAuthExpired, FailureTokenRevoked, FailureDiskFull, FailureCrash, FailureRespawnCap, FailureUnknown:
		return c, true
	}
	return FailureUnknown, false
}

// Markers are matched case-insensitively against the pane tail and the exit
// detail together.
var (
	tokenRevokedMarkers = []string{
		"token has been revoked",
		"token was revoked",
		"token revoked",
		"revoked token",
		"oauth token has been revoked",
	}
	authExpiredMarkers = []string{
		"api error: 401",
		"status 401",
		"401 unauthorized",
		"please run /login",
		"not logged in",
		"authentication_error",
		"oauth token has expired",
	}
	diskFullMarkers = []string{
		"no space left on device",
	}
	respawnCapMarkers = []string{
		"respawn cap reached",
		"respawn_cap_exhausted",
	}
)

// ClassifyFailure maps what a dead run left behind to a FailureClass.
//
// paneTail is the last screenful of the agent's pane, exitDetail the runner's
// description of how the process ended ("" for a clean exit), and authFailed
// the daemon's own auth-failed marker for the run. A revoked token is tested
// before the generic auth markers because a revocation also surfaces as a 401;
// a non-empty exitDetail matching nothing more specific is a crash; anything
// else is unknown.
func ClassifyFailure(paneTail, exitDetail string, authFailed bool) FailureClass {
	text := strings.ToLower(paneTail + "\n" + exitDetail)
	switch {
	case containsAny(text, tokenRevokedMarkers):
		return FailureTokenRevoked
	case authFailed || containsAny(text, authExpiredMarkers):
		return FailureAuthExpired
	case containsAny(text, diskFullMarkers):
		return FailureDiskFull
	case containsAny(text, respawnCapMarkers):
		return FailureRespawnCap
	case strings.TrimSpace(exitDetail) != "":
		return FailureCrash
	default:
		return FailureUnknown
	}
}

func containsAny(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// OutOfReach reports whether retrying cannot fix the class: an expired or
// revoked credential and a full disk need the operator.
func OutOfReach(c FailureClass) bool {
	switch c {
	case FailureAuthExpired, FailureTokenRevoked, FailureDiskFull:
		return true
	case FailureCrash, FailureRespawnCap, FailureUnknown:
		return false
	}
	return false
}

// Attempt is one automatic resume of a run: when it was launched and the
// failure it got.
type Attempt struct {
	At    time.Time
	Class FailureClass
}

// Action is what the resume sweep should do next for one run.
type Action string

// The actions Next returns.
const (
	ActionResume Action = "resume"
	ActionWait   Action = "wait"
	ActionGiveUp Action = "give_up"
)

// Decision is Next's verdict. At is when the next resume is due (zero for
// give_up); Reason is a short machine-stable explanation for logs.
type Decision struct {
	Action Action
	At     time.Time
	Reason string
}

// MaxAttempts is the resume budget of one run.
const MaxAttempts = 3

// backoff is the delay before attempt i+1: measured from the death for the
// first attempt and from the previous attempt for the rest.
var backoff = [MaxAttempts]time.Duration{time.Minute, 2 * time.Minute, 8 * time.Minute}

// Next decides what to do with a dead run that died at diedAt and has already
// been resumed len(attempts) times (oldest first, as the store lists them).
//
// The first resume is due 1 minute after the death, the second 2 minutes after
// the first attempt, the third 8 minutes after the second; after MaxAttempts
// the factory gives up. It gives up early once two consecutive attempts got the
// same out-of-reach class: the first identical repeat proves the failure is not
// transient. A due resume is ActionResume; one in the future is ActionWait with
// At set to its due time.
func Next(diedAt time.Time, attempts []Attempt, now time.Time) Decision {
	n := len(attempts)
	if n >= MaxAttempts {
		return Decision{Action: ActionGiveUp, Reason: "attempt budget exhausted"}
	}
	if n >= 2 {
		last, prev := attempts[n-1].Class, attempts[n-2].Class
		if last == prev && OutOfReach(last) {
			return Decision{Action: ActionGiveUp, Reason: "repeated out-of-reach failure: " + string(last)}
		}
	}
	from := diedAt
	if n > 0 {
		from = attempts[n-1].At
	}
	due := from.Add(backoff[n])
	if now.Before(due) {
		return Decision{Action: ActionWait, At: due, Reason: "backoff"}
	}
	return Decision{Action: ActionResume, At: due, Reason: "due"}
}

// classWords is how a sentence names a class: the noun phrase and its article.
var classWords = map[FailureClass]struct{ article, noun string }{
	FailureAuthExpired:  {"a", "401"},
	FailureTokenRevoked: {"a", "revoked-token error"},
	FailureDiskFull:     {"a", "disk-full error"},
	FailureCrash:        {"a", "crash"},
	FailureRespawnCap:   {"a", "respawn-cap stop"},
	FailureUnknown:      {"an", "unexplained failure"},
}

func words(c FailureClass) (string, string) {
	w, ok := classWords[c]
	if !ok {
		w = classWords[FailureUnknown]
	}
	return w.article, w.noun
}

// Sentence renders the fix row's explanation of a run whose automatic
// resumes gave up, for example:
//
//	Resumed automatically at 23:17, 23:19 and 23:27; every attempt got the same
//	401, so automatic retries stopped. Run claude /login on mac-mini, then
//	press r.
//
// attempts are oldest first; times render as 15:04 in loc (UTC when nil). host
// is the machine the operator must act on (the caller passes os.Hostname()).
// The next step follows the last attempt's class. No attempts renders "".
func Sentence(attempts []Attempt, host string, loc *time.Location) string {
	if len(attempts) == 0 {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}
	times := make([]string, len(attempts))
	same := true
	for i, a := range attempts {
		times[i] = a.At.In(loc).Format("15:04")
		if a.Class != attempts[0].Class {
			same = false
		}
	}
	last := attempts[len(attempts)-1].Class
	article, noun := words(last)

	var b strings.Builder
	b.WriteString("Resumed automatically at ")
	b.WriteString(joinAnd(times))
	switch {
	case len(attempts) == 1:
		b.WriteString("; the attempt got " + article + " " + noun)
	case same:
		b.WriteString("; every attempt got the same " + noun)
	default:
		b.WriteString("; the last attempt got " + article + " " + noun)
	}
	b.WriteString(", so automatic retries stopped. ")
	b.WriteString(nextStep(last, host))
	return b.String()
}

// nextStep is the operator's way out for a class.
func nextStep(c FailureClass, host string) string {
	switch c {
	case FailureAuthExpired, FailureTokenRevoked:
		return "Run claude /login on " + host + ", then press r."
	case FailureDiskFull:
		return "Free disk space on " + host + ", then press r."
	default:
		return "Open the failed chat, then press r."
	}
}

// joinAnd joins items as "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// AlertKey is the dismissal key of a resume alert: "resume:<class>". The run
// key is deliberately not an input, so the same failure recurring on a later
// run stays dismissed while a different class shows again.
func AlertKey(class FailureClass) string {
	return "resume:" + string(class)
}
