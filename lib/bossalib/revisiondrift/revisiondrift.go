// Package revisiondrift classifies a binary's embedded git revision against
// the checkout it is being run from, answering one question: does this
// executing binary contain the commits this working tree has?
//
// It answers by git ancestry and never by comparing dates or version strings. A
// timestamp comparison cannot see a sibling merge, and a version string is a
// label a build chose rather than a fact about history. The incident this
// package exists for is a defect fixed in source that destroyed live data two
// days later, because the running binary predated the fix: a source-level
// inspection concluded the incident was impossible while the machine that
// suffered it was still executing the stale build.
//
// The verdict is paired with its own known flag, on the daemonbin.Inspect
// model: an undeterminable input is reported as unknown, never as healthy and
// never as unhealthy. The could-not-evaluate causes stay distinct from one
// another, because failing closed and being diagnosable are separate
// properties and the second is not implied by the first.
//
// Nothing here writes. No code path mutates the repository, the binary, or any
// file, so there is no re-stage loop and no silent downgrade to express.
package revisiondrift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/recurser/bossalib/skillinstall"
)

// Reason is a short human phrase drawn from the fixed vocabulary below.
//
// It is its own type rather than a bare string so an outcome cannot be
// fabricated at a call site, and so a switch over outcomes is exhaustive by
// inspection. One definition keeps every surface that renders drift — a doctor
// check, an env section, a startup warning — from inventing three different
// phrasings for one fact.
type Reason string

const (
	// ReasonDescendant is the only healthy outcome: the binary's revision is
	// contained in the checkout's history, so every commit the working tree
	// has is also in the executing bytes.
	ReasonDescendant Reason = "binary revision is contained in the checkout history"
	// ReasonBehind is the only unhealthy outcome, and the one the incident
	// reproduced: git proved the revision is not an ancestor of HEAD.
	ReasonBehind Reason = "binary revision is not an ancestor of the checkout HEAD"
	// ReasonNoCheckout covers an installed binary run outside any checkout.
	// There is nothing to compare against, which is unknown rather than
	// healthy.
	ReasonNoCheckout Reason = "not running from a checkout"
	// ReasonUnstamped covers a binary built without the version ldflags. It is
	// a genuine producer state and not a theoretical one: a Dockerfile in this
	// repo builds with a bare `go build`.
	ReasonUnstamped Reason = "binary carries no embedded revision"
	// ReasonDevBuild covers a binary whose version is the build system's
	// describe fallback. Kept separate from ReasonUnstamped because the two are
	// different producer states with different fixes.
	ReasonDevBuild Reason = "binary carries the dev-build version fallback"
	// ReasonRevisionAbsent covers a revision this clone has never heard of — a
	// shallow clone, or a commit that was pruned or force-pushed away. It is
	// deliberately NOT ReasonBehind: git reports "not an ancestor" and "no such
	// commit" with different exit codes, and collapsing them reports a shallow
	// clone as a stale binary.
	ReasonRevisionAbsent Reason = "binary revision is absent from this clone"
	// ReasonCheckoutUntrusted covers a working tree that did not authenticate
	// as the caller's own project. Without the check any look-alike directory
	// above the working directory becomes the reference history.
	ReasonCheckoutUntrusted Reason = "checkout did not authenticate as the expected clone"
	// ReasonGitUnavailable covers git missing, git timing out, or git failing
	// for a reason that is not an ancestry answer.
	ReasonGitUnavailable Reason = "git could not be run against the checkout"
	// ReasonRevisionUnreadable covers a binary whose embedded revision could
	// not be obtained at all — the file is missing, or interrogating it failed.
	//
	// Deliberately distinct from ReasonUnstamped, which is a claim about how
	// the binary was BUILT and one this outcome has not established. It is set
	// by a caller that could not produce the input, which is why it is in the
	// vocabulary rather than phrased afresh at each surface.
	ReasonRevisionUnreadable Reason = "binary's embedded revision could not be read"
)

// DevVersionFallback is the literal a version string carries when the build
// system's `git describe ... || echo "dev"` fallback fired. It is also the
// buildinfo package's own default, so an unstamped binary carries it too —
// which is why Inspect tests Stamped first and this second.
//
// Matched exactly, and deliberately NOT through a release-tag predicate: such a
// predicate also rejects `v1.2.3-5-gabc` and `v1.2.3-dirty`, which are ordinary
// builds from a real checkout carrying a real revision. Treating those as
// undeterminable would make ancestry unreachable for nearly every build this
// package exists to classify.
const DevVersionFallback = "dev"

// unstampedRevisions are the values an embedded revision carries when no
// ldflags stamp reached the build: the buildinfo package's declared "unknown"
// default, and the empty string a caller passes for a binary it could not read
// at all.
var unstampedRevisions = []string{"", "unknown"}

// Stamped reports whether revision is a real embedded revision rather than one
// of the no-ldflags defaults.
//
// One predicate, called once, whose answer is carried on the result as
// RevisionStamped. Re-deriving it downstream by testing the revision string for
// emptiness rebuilds the conflation this package exists to prevent: "" and
// "unknown" are the same producer state, and a consumer that tests only one of
// them classifies the other as a real revision and asks git about it.
func Stamped(revision string) bool {
	for _, unstamped := range unstampedRevisions {
		if revision == unstamped {
			return false
		}
	}
	return true
}

// Drift is the classifier's verdict about one binary.
type Drift struct {
	// Behind reports that the binary's revision is provably not contained in
	// the checkout's history. Meaningful only when BehindKnown is true.
	Behind bool
	// BehindKnown is false whenever the comparison could not be made at all,
	// so an unresolvable input can never be misread as a healthy verdict.
	BehindKnown bool
	// Reason is the outcome, drawn from the Reason* vocabulary.
	Reason Reason
	// Detail carries the underlying cause — git's own stderr, or the error text
	// from a failed invocation — so a failure reads as the real message rather
	// than a bare "exit status 1". Empty when there was nothing to add.
	Detail string
	// RevisionStamped is Stamped(BinaryRevision), computed once here so no
	// consumer re-derives it from the revision string's emptiness.
	RevisionStamped bool
	// BinaryRevision and BinaryVersion echo what the caller supplied.
	BinaryRevision string
	BinaryVersion  string
	// CheckoutRoot is the working tree compared against. Empty when none was
	// found or when it did not authenticate.
	CheckoutRoot string
	// CheckoutRevision is that working tree's HEAD. Empty when unresolved.
	CheckoutRevision string
	// CloneGitDir is the clone's COMMON git directory — the one that holds the
	// object database every linked worktree of the clone shares. Empty when
	// unresolved.
	CloneGitDir string
	// ShallowClone reports that the clone's history is truncated, which is one
	// of the two causes of ReasonRevisionAbsent and the actionable one.
	ShallowClone bool
}

// Unknown builds a caller-produced unknown outcome coherently.
//
// Inspect is the only place that establishes the fact flags from a real input,
// so a caller that could not produce an input at all must not hand-build a
// Drift literal: the zero value leaves RevisionStamped false, which is the
// build-time claim "this binary carries no embedded revision" — a claim
// nothing observed. The measured defect is ReasonRevisionUnreadable arriving
// with RevisionStamped false purely because nothing was read, and a renderer
// then asserting the very thing that reason exists to withhold.
//
// Every field is derived here rather than accepted: BehindKnown stays false so
// the outcome is neither healthy nor unhealthy, and RevisionStamped is derived
// through the same Stamped predicate Inspect uses, from the empty revision this
// outcome actually has.
func Unknown(reason Reason, detail string) Drift {
	return Drift{
		Reason:          reason,
		Detail:          detail,
		RevisionStamped: Stamped(""),
	}
}

// Unknown reports that no verdict was reached. Unknown is neither healthy nor
// unhealthy, and a caller must not render it as either.
func (d Drift) Unknown() bool { return !d.BehindKnown }

// Describe renders the verdict as one line, naming the binary, the revision it
// carries, and the checkout revision it was compared against.
//
// A method rather than a format string at each call site, because the
// requirement is that no two surfaces disagree and three renderers is exactly
// how they start to. A surface adds its own marker — a FAIL prefix, a section
// indent — but the facts all come from here.
func (d Drift) Describe(binaryLabel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s revision %s: %s", binaryLabel, d.RevisionLabel(), d.Reason)
	if d.CheckoutRevision != "" {
		fmt.Fprintf(&b, " (compared against checkout %s at %s)", d.CheckoutRevision, d.CheckoutRoot)
	}
	if d.Detail != "" {
		fmt.Fprintf(&b, " [%s]", d.Detail)
	}
	return b.String()
}

// RevisionLabel renders the binary revision for display, keyed on the carried
// RevisionStamped flag rather than on the string being empty.
func (d Drift) RevisionLabel() string {
	// Checked BEFORE the stamped flag. An unreadable revision leaves
	// RevisionStamped false because nothing was read, but rendering it as
	// "(unstamped)" asserts how the binary was BUILT — which is exactly the
	// claim ReasonRevisionUnreadable exists to withhold.
	if d.Reason == ReasonRevisionUnreadable {
		return "(unreadable)"
	}
	if !d.RevisionStamped {
		return "(unstamped)"
	}
	return d.BinaryRevision
}

// Relation is the tri-state ancestry relation, for a machine-readable surface
// that must not reduce it to a boolean: "descendant", "behind" or "unknown".
func (d Drift) Relation() string {
	switch {
	case !d.BehindKnown:
		return "unknown"
	case d.Behind:
		return "behind"
	default:
		return "descendant"
	}
}

// GitResult is one git invocation's outcome. ExitCode is meaningful only when
// the GitRunner returned a nil error.
type GitResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// GitRunner runs `git -C repoRoot args...`.
//
// The contract splits the two failures os/exec conflates: a non-zero exit is
// reported as ExitCode with a nil error, and a non-nil error is reserved for
// "git could not be run at all" — binary missing, context deadline, a signal.
// That split is the whole point. `merge-base --is-ancestor` answers "not an
// ancestor" with exit 1 and "I have never heard of that commit" with exit 128,
// and a seam that returns only (output, error) makes those two indistinguishable
// without reconstructing an *exec.ExitError inside a test.
type GitRunner func(ctx context.Context, repoRoot string, args ...string) (GitResult, error)

// ExecGitTimeout bounds each git invocation. Short on purpose: this runs inside
// a diagnostic, and an unbounded advisory subprocess in a diagnostic has
// already caused a multi-minute silent stall in this codebase.
const ExecGitTimeout = 5 * time.Second

// execGitWaitDelay bounds how long Run waits for the output pipe to close after
// the context deadline has already killed git. Without it the two bounds are
// not the same bound, and only the first one is enforced.
const execGitWaitDelay = 2 * time.Second

// ExecGit is the production GitRunner.
//
// Two deliberate departures from the bare `.Output()` idiom this repository
// also uses. The context is given its own deadline, so a pathological
// repository cannot stall a diagnostic. And Stderr is captured, because
// `.Output()` without it parks git's real message on (*exec.ExitError).Stderr,
// where formatting the error alone prints the useless "exit status 1".
func ExecGit(ctx context.Context, repoRoot string, args ...string) (GitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, ExecGitTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Bounds the pipe-close wait, not the process. Capturing into a buffer
	// makes os/exec copy through a pipe, and Run blocks until every writer
	// closes it — so a git subprocess that spawns a grandchild (a pager, a
	// credential or hook helper) keeps the diagnostic hanging past the deadline
	// that already killed git itself.
	cmd.WaitDelay = execGitWaitDelay
	runErr := cmd.Run()

	result := GitResult{
		Stdout: strings.TrimSpace(stdout.String()),
		Stderr: strings.TrimSpace(stderr.String()),
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return result, nil
	case errors.As(runErr, &exitErr):
		// A deadline kills the child, which surfaces as an ExitError with a
		// signal rather than an exit status. Reporting that as an exit code
		// would hand the caller a -1 to classify; it is a failure to run.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, fmt.Errorf("git %s: %w", strings.Join(args, " "), ctxErr)
		}
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return result, fmt.Errorf("git %s: %w", strings.Join(args, " "), runErr)
	}
}

// FindCheckoutRoot resolves the working tree at or above startDir, structurally
// rather than by asking git — a look-alike directory that happens to be a git
// repository is not this project's checkout.
//
// It returns ok == false when startDir is not inside a checkout, which *is* the
// installed-binary case rather than an error.
func FindCheckoutRoot(startDir string) (string, bool) {
	srcRoot, ok := skillinstall.FindSourceRoot(startDir)
	if !ok {
		return "", false
	}
	root := srcRoot
	for range strings.Split(skillinstall.SourceRelPath, "/") {
		root = filepath.Dir(root)
	}
	return root, true
}

// Options are Inspect's inputs.
type Options struct {
	// BinaryRevision is the embedded revision of the binary being classified.
	// Taken from the caller rather than read from buildinfo here, so one
	// classifier answers for the executing process, for a sibling binary the
	// caller interrogated, or for a fixture.
	BinaryRevision string
	// BinaryVersion is that binary's embedded version string.
	BinaryVersion string
	// StartDir is where the upward search for a checkout begins.
	//
	// The verdict is cwd-dependent by design: the same installed binary is
	// undeterminable from a home directory and classifiable from inside a
	// checkout. That is a property of resolving the reference structurally, not
	// a bug to hide, which is why the result names the checkout it compared
	// against rather than implying there is only one.
	StartDir string
	// Git runs git. A nil Git yields ReasonGitUnavailable rather than a panic,
	// because every caller is a diagnostic and a diagnostic must not crash the
	// command it is reporting on.
	Git GitRunner
	// FindCheckout resolves a working tree from StartDir. Defaults to
	// FindCheckoutRoot when nil.
	FindCheckout func(startDir string) (string, bool)
	// TrustCheckout optionally authenticates the resolved working tree before
	// its history becomes the reference. Repository identity stays with the
	// caller that already owns it, so this package holds no project constants;
	// when nil, no authentication is performed.
	TrustCheckout func(root string) (bool, error)
}

// Inspect classifies BinaryRevision against the checkout found from StartDir.
//
// It is a pure comparison and returns no error: every failure is an outcome in
// the Reason vocabulary with BehindKnown false, so a caller has no error value
// it could accidentally render as a healthy verdict.
func Inspect(ctx context.Context, opts Options) Drift {
	drift := Drift{
		BinaryRevision:  opts.BinaryRevision,
		BinaryVersion:   opts.BinaryVersion,
		RevisionStamped: Stamped(opts.BinaryRevision),
	}
	// Ordered before the dev-build test because an unstamped binary carries the
	// dev version too: the buildinfo defaults are "unknown" and "dev" together,
	// so testing the version first would report every no-ldflags build as a dev
	// build and make the unstamped outcome unreachable.
	if !drift.RevisionStamped {
		drift.Reason = ReasonUnstamped
		return drift
	}
	if opts.BinaryVersion == DevVersionFallback {
		drift.Reason = ReasonDevBuild
		return drift
	}
	if opts.Git == nil {
		drift.Reason = ReasonGitUnavailable
		drift.Detail = "no git runner configured"
		return drift
	}

	findCheckout := opts.FindCheckout
	if findCheckout == nil {
		findCheckout = FindCheckoutRoot
	}
	root, ok := findCheckout(opts.StartDir)
	if !ok {
		drift.Reason = ReasonNoCheckout
		return drift
	}
	if opts.TrustCheckout != nil {
		trusted, err := opts.TrustCheckout(root)
		switch {
		case err != nil:
			// Fail closed toward untrusted, not toward trusted: a check that
			// could not complete has not authenticated anything.
			drift.Reason = ReasonCheckoutUntrusted
			drift.Detail = err.Error()
			return drift
		case !trusted:
			drift.Reason = ReasonCheckoutUntrusted
			drift.Detail = root + " is not the expected clone"
			return drift
		}
	}
	drift.CheckoutRoot = root

	head, err := gitLine(ctx, opts.Git, root, "rev-parse", "HEAD")
	if err != nil {
		drift.Reason = ReasonGitUnavailable
		drift.Detail = err.Error()
		return drift
	}
	drift.CheckoutRevision = head

	// --git-common-dir, never --absolute-git-dir. In a linked worktree the
	// latter names <main>/.git/worktrees/<name>, a private directory that holds
	// no object database and no shallow marker, while the former names the
	// clone-wide .git that holds both. Keying on the wrong one is silently
	// correct in a main worktree and silently wrong in every linked one — and
	// every session of this project's own tooling runs in a linked worktree.
	commonDir, err := gitLine(ctx, opts.Git, root, "rev-parse", "--git-common-dir")
	if err != nil {
		drift.Reason = ReasonGitUnavailable
		drift.Detail = err.Error()
		return drift
	}
	drift.CloneGitDir = resolveAgainst(root, commonDir)
	drift.ShallowClone = shallowMarkerPresent(drift.CloneGitDir)

	result, err := opts.Git(ctx, root, "merge-base", "--is-ancestor", opts.BinaryRevision, "HEAD")
	switch {
	case err != nil:
		drift.Reason = ReasonGitUnavailable
		drift.Detail = err.Error()
	case result.ExitCode == 0:
		drift.BehindKnown = true
		drift.Reason = ReasonDescendant
	case result.ExitCode == 1:
		drift.BehindKnown = true
		drift.Behind = true
		drift.Reason = ReasonBehind
	case result.ExitCode >= 128 && revisionAbsentFatal(result.Stderr):
		drift.Reason = ReasonRevisionAbsent
		drift.Detail = absentDetail(drift, result)
	default:
		drift.Reason = ReasonGitUnavailable
		drift.Detail = fmt.Sprintf("git merge-base --is-ancestor exited %d: %s", result.ExitCode, result.Stderr)
	}
	return drift
}

// revisionAbsentUnknownObjectMessages are git's own phrasings for "I have never
// heard of that name", measured against git's actual output rather than
// guessed: a well-formed but absent 40-hex SHA reports "Not a valid commit
// name <sha>", and a shorter or malformed name reports "Not a valid object
// name <name>".
var revisionAbsentUnknownObjectMessages = []string{
	"not a valid commit name",
	"not a valid object name",
}

// revisionAbsentFatal reports whether a git fatal means the revision is absent
// from this clone, rather than meaning git could not answer at all.
//
// Keying ReasonRevisionAbsent on the exit code alone reads EVERY fatal as
// "absent from this clone": measured, `git -C <missing-dir> merge-base
// --is-ancestor` exits 128 with "cannot change to ...: No such file or
// directory", and an unknown option exits 129 — both outside the documented
// scope of ReasonRevisionAbsent and inside ReasonGitUnavailable's. That matters
// past taxonomy because `boss env --json` publishes the reason, so a corrupt
// object database or a wrong working directory is reported to a machine
// consumer as a shallow clone.
func revisionAbsentFatal(stderr string) bool {
	lowered := strings.ToLower(stderr)
	for _, message := range revisionAbsentUnknownObjectMessages {
		if strings.Contains(lowered, message) {
			return true
		}
	}
	return false
}

// gitLine runs one git query that is only meaningful on success, folding a
// non-zero exit into the error so the caller has a single failure branch. The
// captured stderr rides along, which is the reason this does not use a bare
// .Output() shape.
func gitLine(ctx context.Context, git GitRunner, root string, args ...string) (string, error) {
	result, err := git(ctx, root, args...)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git %s exited %d: %s", strings.Join(args, " "), result.ExitCode, result.Stderr)
	}
	return result.Stdout, nil
}

// resolveAgainst absolutises a git-reported path. git answers
// --git-common-dir relatively in a main worktree and absolutely in a linked
// one, so a caller that stored it verbatim would hold a path that means
// different things on the two shapes.
func resolveAgainst(root, path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(root, path)
}

// shallowMarkerPresent reports whether the clone's history is truncated.
//
// The marker is a file in the clone's COMMON git directory, which is what makes
// reading it through --git-common-dir load-bearing rather than cosmetic: in a
// linked worktree no shallow file ever exists under the worktree's private git
// directory, so a check keyed on --absolute-git-dir reports every shallow
// linked worktree as complete and never fails a test run from a main worktree.
func shallowMarkerPresent(commonGitDir string) bool {
	if commonGitDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(commonGitDir, "shallow"))
	return err == nil
}

// absentDetail explains an absent revision, distinguishing the two causes the
// single outcome covers. A truncated clone is fixable by deepening it; a pruned
// or force-pushed commit is not, and an operator needs to know which they have.
func absentDetail(drift Drift, result GitResult) string {
	var parts []string
	if drift.ShallowClone {
		parts = append(parts, "this clone is shallow, so older history is absent locally")
	}
	if result.Stderr != "" {
		parts = append(parts, result.Stderr)
	}
	if len(parts) == 0 {
		parts = append(parts, "the revision is not in this clone's object database")
	}
	return strings.Join(parts, "; ")
}
