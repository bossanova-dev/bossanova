package gitremote

import (
	"errors"
	"strings"
	"testing"
)

// TestIsTransientMessageSignatures proves every signature in the transient set
// classifies transient, one case per signature.
//
// Each fixture is a realistic wrapped error string of the shape
// runGitWithTimeout produces ("git <args>: exit status 128: <stderr>"), and each
// deliberately contains EXACTLY ONE signature from the set — asserted below. A
// fixture that matched two signatures would let a deleted signature keep passing
// on its neighbour's match, which is exactly the regression this table exists to
// catch.
func TestIsTransientMessageSignatures(t *testing.T) {
	cases := []struct {
		signature string
		message   string
	}{
		{
			signature: "Permission denied (publickey)",
			message:   "git fetch origin: exit status 128: git@github.com: Permission denied (publickey).",
		},
		{
			signature: "Could not read from remote repository",
			message:   "git ls-remote origin: exit status 128: fatal: Could not read from remote repository.",
		},
		{
			signature: "kex_exchange_identification",
			message:   "git fetch origin: exit status 128: kex_exchange_identification: banner line contains invalid characters",
		},
		{
			signature: "Connection closed by",
			message:   "git fetch origin: exit status 128: Connection closed by 140.82.121.4 port 22",
		},
		{
			signature: "Connection reset by",
			message:   "git fetch origin: exit status 128: Connection reset by 140.82.121.3 port 22",
		},
		{
			signature: "Operation timed out",
			message:   "git ls-remote origin: exit status 128: ssh: connect to host github.com port 22: Operation timed out",
		},
		{
			signature: "remote end hung up",
			message:   "git push origin HEAD: exit status 128: fatal: the remote end hung up unexpectedly",
		},
		{
			signature: "RPC failed",
			message:   "git fetch origin: exit status 128: error: RPC failed; curl 92 HTTP/2 stream 5 was not closed cleanly",
		},
		{
			signature: "early EOF",
			message:   "git fetch origin: exit status 128: fatal: early EOF",
		},
	}

	for _, tc := range cases {
		t.Run(tc.signature, func(t *testing.T) {
			if !IsTransientMessage(tc.message) {
				t.Fatalf("IsTransientMessage(%q) = false, want true", tc.message)
			}

			// The fixture must isolate its own signature (see the doc comment).
			var matched []string
			for _, sig := range transientSignatures {
				if strings.Contains(tc.message, sig) {
					matched = append(matched, sig)
				}
			}
			if len(matched) != 1 || matched[0] != tc.signature {
				t.Fatalf("fixture %q matches %v, want exactly [%q]", tc.message, matched, tc.signature)
			}
		})
	}

	// Completeness guard: adding a signature without a table case fails here
	// rather than shipping an untested classification.
	if len(cases) != len(transientSignatures) {
		t.Fatalf("table has %d cases for %d signatures; add a case per signature", len(cases), len(transientSignatures))
	}
	covered := make(map[string]bool, len(cases))
	for _, tc := range cases {
		covered[tc.signature] = true
	}
	for _, sig := range transientSignatures {
		if !covered[sig] {
			t.Errorf("signature %q has no table case", sig)
		}
	}
}

// TestBothGitHangupWordingsClassifyTransient pins why the hangup signature is
// truncated to "remote end hung up". git has two hangup messages, emitted from
// different phases — connect.c's die_initial_contact for "...upon initial
// contact", and the pkt-line reader for "...unexpectedly" on a mid-stream EOF —
// and both ship in the git binary (verified with `strings` against git 2.53.0,
// which contains exactly these two matches for the substring).
//
// The initial-contact one is what makes the truncation necessary rather than
// tidy. die_initial_contact prints "Could not read from remote repository."
// instead when nothing at all was read, so the hangup wording arrives ALONE:
// reproduced here by driving `git ls-remote` through a GIT_SSH_COMMAND that emits
// one valid pkt-line and then EOFs without a flush, whose entire stderr is
// "fatal: the remote end hung up upon initial contact". Unlike every other
// transport-phase near-miss in this package it therefore has no composite second
// line to be caught by, and a genuine transient hangup of exactly the shape this
// epic exists to survive would classify terminal and never be retried.
func TestBothGitHangupWordingsClassifyTransient(t *testing.T) {
	const truncated = "remote end hung up"

	cases := []struct {
		name    string
		message string
	}{
		{
			// Remote answered, then EOF'd before finishing ref advertisement.
			name:    "upon initial contact",
			message: "git ls-remote origin: exit status 128: fatal: the remote end hung up upon initial contact",
		},
		{
			name:    "unexpectedly",
			message: "git push origin HEAD: exit status 128: fatal: the remote end hung up unexpectedly",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsTransientMessage(tc.message) {
				t.Fatalf("IsTransientMessage(%q) = false, want true: the signature must cover both of git's hangup wordings", tc.message)
			}
		})
	}

	// Guard the truncation itself: re-lengthening the literal to either wording
	// silently drops the other, and the loop above would still pass on one case.
	var found bool
	for _, sig := range transientSignatures {
		if sig == truncated {
			found = true
		}
	}
	if !found {
		t.Fatalf("transientSignatures no longer contains %q; re-lengthening it drops one of git's two hangup wordings", truncated)
	}
}

// TestIsTransientMessageNegativeCorpus is the load-bearing half of the contract.
// Every string here is a TERMINAL git failure: retrying it burns a session-start
// budget and hides a real misconfiguration, so widening the classifier until one
// of these matches is a regression, not a feature.
//
// Each fixture is the distinctive line of git's stderr, not always the whole of
// it. Two of them ("Repository not found", "does not appear to be a git
// repository") come with a second line from git that DOES classify transient;
// that overlap is a known, bounded cost and is characterized in
// TestTerminalCompositesClassifyTransientAsAcceptedCost below.
func TestIsTransientMessageNegativeCorpus(t *testing.T) {
	cases := []struct {
		name    string
		message string
	}{
		{
			name:    "non-fast-forward",
			message: "git push origin HEAD: exit status 1: ! [rejected]        main -> main (non-fast-forward)",
		},
		{
			name:    "stale info",
			message: "git push origin HEAD: exit status 1: ! [rejected]        main -> main (stale info)",
		},
		{
			name:    "rejected",
			message: "git push origin HEAD: exit status 1: ! [rejected]        HEAD -> feature (fetch first)",
		},
		{
			name:    "Repository not found",
			message: "git ls-remote origin: exit status 128: ERROR: Repository not found.",
		},
		{
			name:    "does not appear to be a git repository",
			message: "git fetch origin: exit status 128: fatal: 'origin' does not appear to be a git repository",
		},
		{
			name:    "protected branch",
			message: "git push origin HEAD: exit status 1: remote: error: refusing to allow a Personal Access Token to push to a protected branch",
		},
		{
			name:    "index.lock",
			message: "git commit: exit status 128: fatal: Unable to create '/repo/.git/index.lock': File exists.",
		},
		{
			name:    "failed to push some refs",
			message: "git push origin HEAD: exit status 1: error: failed to push some refs to 'github.com:recurser/bossanova.git'",
		},
	}

	if len(cases) != 8 {
		t.Fatalf("negative corpus has %d cases, want the 8 the contract names", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if IsTransientMessage(tc.message) {
				t.Fatalf("IsTransientMessage(%q) = true, want false: retrying a terminal failure hides a real misconfiguration", tc.message)
			}
		})
	}
}

func TestIsTransient(t *testing.T) {
	t.Run("nil error is not transient", func(t *testing.T) {
		if IsTransient(nil) {
			t.Fatal("IsTransient(nil) = true, want false")
		}
	})

	t.Run("delegates over err.Error", func(t *testing.T) {
		err := errors.New("git fetch origin: exit status 128: fatal: early EOF")
		if !IsTransient(err) {
			t.Fatalf("IsTransient(%v) = false, want true", err)
		}
	})

	t.Run("reaches a wrapped cause", func(t *testing.T) {
		inner := errors.New("git@github.com: Permission denied (publickey).")
		wrapped := errWrap("push branch", inner)
		if !IsTransient(wrapped) {
			t.Fatalf("IsTransient(%v) = false, want true", wrapped)
		}
	})

	t.Run("terminal error is not transient", func(t *testing.T) {
		err := errors.New("git push origin HEAD: exit status 1: error: failed to push some refs")
		if IsTransient(err) {
			t.Fatalf("IsTransient(%v) = true, want false", err)
		}
	})

	t.Run("empty message is not transient", func(t *testing.T) {
		if IsTransientMessage("") {
			t.Fatal(`IsTransientMessage("") = true, want false`)
		}
	})
}

// TestTerminalCompositesClassifyTransientAsAcceptedCost characterizes known
// overlaps rather than asserting a desired outcome. Two of the corpus fixtures
// above are the distinctive FIRST line of a multi-line stderr, and in both cases
// git appends its own "fatal: Could not read from remote repository." — so the
// full composite classifies transient even though the failure is terminal:
//
//   - a repository you cannot see over SSH: GitHub answers "ERROR: Repository
//     not found." and git appends its read failure;
//   - a missing or misnamed remote, or a path that is not a repository: git
//     prints "does not appear to be a git repository" and appends the same line.
//     Confirmed against real git here with `git fetch <undefined-remote>` and
//     `git ls-remote /tmp/not-a-repo`.
//
// The corpus keeps only the first line of each on purpose: it pins that the
// distinctive text alone is terminal, which is what the classifier is contracted
// on. The composites belong here instead, as a cost.
//
// The cost is bounded, and that is what makes the overlap tolerable: DefaultPolicy
// retries twice, ~4s, and then AttemptsError surfaces the real stderr with its
// "after N attempts over" suffix, so the misconfiguration still reaches the
// operator legibly. It is worth most attention for a wrong remote URL, which
// fails this way on every remote git op rather than once.
//
// The classifier is a substring match over a fixed set by design (one definition,
// shared by two consumers); adding terminal-override precedence would be a
// different design than the one this package was specified with. If a future
// ticket introduces it, this test is the one to update — and the package doc's
// two-limb safety argument is what it must satisfy.
func TestTerminalCompositesClassifyTransientAsAcceptedCost(t *testing.T) {
	cases := []struct {
		name     string
		combined string
	}{
		{
			name:     "Repository not found",
			combined: "git ls-remote origin: exit status 128: ERROR: Repository not found.\nfatal: Could not read from remote repository.",
		},
		{
			name:     "does not appear to be a git repository",
			combined: "git fetch upstream: exit status 128: fatal: 'upstream' does not appear to be a git repository\nfatal: Could not read from remote repository.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsTransientMessage(tc.combined) {
				t.Fatalf("IsTransientMessage(%q) = false; the documented overlap changed — re-read the package doc's safety argument before updating this test", tc.combined)
			}
		})
	}
}

// errWrap builds a wrapped error without pulling fmt into the assertion path.
func errWrap(context string, err error) error {
	return &wrapped{context: context, err: err}
}

type wrapped struct {
	context string
	err     error
}

func (w *wrapped) Error() string { return w.context + ": " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }
