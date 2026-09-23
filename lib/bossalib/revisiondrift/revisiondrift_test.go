package revisiondrift

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/buildinfo"
)

// fakeGit builds a GitRunner that answers each git subcommand from a table,
// recording the argument vectors it was asked for. The table is keyed on the
// first argument pair so a test can pin `rev-parse HEAD` and
// `merge-base --is-ancestor` independently, which is what lets the exit-1 and
// exit-128 cases be separate tests rather than one parameterised row.
type fakeGit struct {
	head       string
	commonDir  string
	ancestor   GitResult
	ancestorer error
	calls      [][]string
}

func (f *fakeGit) runner() GitRunner {
	return func(_ context.Context, _ string, args ...string) (GitResult, error) {
		f.calls = append(f.calls, args)
		switch {
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "HEAD":
			return GitResult{Stdout: f.head}, nil
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--git-common-dir":
			return GitResult{Stdout: f.commonDir}, nil
		case len(args) >= 2 && args[0] == "merge-base" && args[1] == "--is-ancestor":
			return f.ancestor, f.ancestorer
		}
		return GitResult{}, nil
	}
}

func (f *fakeGit) sawSubcommand(name string) bool {
	for _, call := range f.calls {
		if len(call) > 0 && call[0] == name {
			return true
		}
	}
	return false
}

// optionsFor builds Options pointing at a checkout that always resolves and
// always authenticates, so a test can vary exactly one input.
func optionsFor(revision, version string, git GitRunner) Options {
	return Options{
		BinaryRevision: revision,
		BinaryVersion:  version,
		StartDir:       "/irrelevant",
		Git:            git,
		FindCheckout:   func(string) (string, bool) { return "/checkout", true },
		TrustCheckout:  func(string) (bool, error) { return true, nil },
	}
}

func TestInspectAncestorRevisionIsDescendant(t *testing.T) {
	git := &fakeGit{head: "headsha", ancestor: GitResult{ExitCode: 0}}
	got := Inspect(context.Background(), optionsFor("oldsha", "v1.2.3", git.runner()))

	if !got.BehindKnown {
		t.Fatalf("BehindKnown = false, want a known verdict: %+v", got)
	}
	if got.Behind {
		t.Errorf("Behind = true for an ancestor revision: %+v", got)
	}
	if got.Reason != ReasonDescendant {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonDescendant)
	}
	if got.CheckoutRevision != "headsha" || got.CheckoutRoot != "/checkout" {
		t.Errorf("verdict does not name what it compared against: %+v", got)
	}
	if got.Unknown() {
		t.Errorf("Unknown() = true on a known verdict: %+v", got)
	}
	if got.Relation() != "descendant" {
		t.Errorf("Relation() = %q, want descendant", got.Relation())
	}
}

// TestInspectExitOneIsBehind and TestInspectExitOneTwentyEightIsAbsent are two
// tests on purpose. Distinguishing "not an ancestor" from "no such commit" is
// the point of this package, and a single table row with a shared expectation
// would pass against a classifier that collapsed them.
func TestInspectExitOneIsBehind(t *testing.T) {
	git := &fakeGit{head: "headsha", ancestor: GitResult{ExitCode: 1}}
	got := Inspect(context.Background(), optionsFor("staleshaa", "v1.2.3", git.runner()))

	if !got.BehindKnown || !got.Behind {
		t.Fatalf("exit 1 did not produce a known behind verdict: %+v", got)
	}
	if got.Reason != ReasonBehind {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonBehind)
	}
	if got.Relation() != "behind" {
		t.Errorf("Relation() = %q, want behind", got.Relation())
	}
	line := got.Describe("boss binary")
	for _, want := range []string{"boss binary", "staleshaa", "headsha", "/checkout"} {
		if !strings.Contains(line, want) {
			t.Errorf("Describe() = %q, missing %q", line, want)
		}
	}
}

func TestInspectExitOneTwentyEightIsAbsentNotBehind(t *testing.T) {
	git := &fakeGit{
		head:      "headsha",
		commonDir: t.TempDir(),
		ancestor:  GitResult{ExitCode: 128, Stderr: "fatal: Not a valid object name prunedsha"},
	}
	got := Inspect(context.Background(), optionsFor("prunedsha", "v1.2.3", git.runner()))

	if got.Reason != ReasonRevisionAbsent {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonRevisionAbsent)
	}
	if got.BehindKnown {
		t.Errorf("BehindKnown = true for an unresolvable revision: %+v", got)
	}
	if got.Behind {
		t.Errorf("Behind = true for an unresolvable revision — exit 128 was read as exit 1: %+v", got)
	}
	if got.Relation() != "unknown" {
		t.Errorf("Relation() = %q, want unknown", got.Relation())
	}
	if !strings.Contains(got.Detail, "Not a valid object name") {
		t.Errorf("Detail = %q, want git's own stderr preserved", got.Detail)
	}
}

// TestInspectEmptyRevisionIsUnstamped and
// TestInspectUnknownRevisionIsUnstamped are separate because the second is the
// one that is actually reachable in this tree: a bare `go build` leaves the
// buildinfo default "unknown" in place, not "". A classifier that tested only
// for emptiness would pass the first and ask git about a literal "unknown".
func TestInspectEmptyRevisionIsUnstamped(t *testing.T) {
	git := &fakeGit{}
	got := Inspect(context.Background(), optionsFor("", "v1.2.3", git.runner()))

	if got.Reason != ReasonUnstamped {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonUnstamped)
	}
	if got.BehindKnown {
		t.Errorf("BehindKnown = true for an unstamped binary: %+v", got)
	}
	if got.RevisionStamped {
		t.Errorf("RevisionStamped = true for an empty revision")
	}
	if len(git.calls) != 0 {
		t.Errorf("git was consulted for an unstamped binary: %v", git.calls)
	}
}

func TestInspectUnknownRevisionIsUnstamped(t *testing.T) {
	git := &fakeGit{}
	got := Inspect(context.Background(), optionsFor("unknown", "v1.2.3", git.runner()))

	if got.Reason != ReasonUnstamped {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonUnstamped)
	}
	if got.RevisionStamped {
		t.Errorf("the buildinfo \"unknown\" default was treated as a real revision")
	}
	if len(git.calls) != 0 {
		t.Errorf("git was asked to resolve the literal \"unknown\": %v", git.calls)
	}
	if got.RevisionLabel() != "(unstamped)" {
		t.Errorf("RevisionLabel() = %q, want (unstamped)", got.RevisionLabel())
	}
}

func TestInspectDevVersionIsDevBuildNotUnstamped(t *testing.T) {
	git := &fakeGit{}
	got := Inspect(context.Background(), optionsFor("realsha", DevVersionFallback, git.runner()))

	if got.Reason != ReasonDevBuild {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonDevBuild)
	}
	if got.Reason == ReasonUnstamped {
		t.Errorf("a dev build was collapsed into the unstamped outcome")
	}
	if got.BehindKnown {
		t.Errorf("BehindKnown = true for a dev build: %+v", got)
	}
	if !got.RevisionStamped {
		t.Errorf("RevisionStamped = false for a dev build carrying a real revision")
	}
}

// TestInspectDescribeVersionStillClassifiesByAncestry guards the predicate
// choice. `git describe` emits v1.2.3-5-gabc and v1.2.3-dirty for ordinary
// builds from a real checkout; treating those as dev builds — which a
// release-tag predicate would — makes ancestry unreachable for nearly every
// build this package exists to classify, while every other test here stays
// green.
func TestInspectDescribeVersionStillClassifiesByAncestry(t *testing.T) {
	for _, version := range []string{"v1.2.3-5-gabcdef0", "v1.2.3-dirty", "abc1234"} {
		git := &fakeGit{head: "headsha", ancestor: GitResult{ExitCode: 1}}
		got := Inspect(context.Background(), optionsFor("staleshaa", version, git.runner()))
		if got.Reason != ReasonBehind {
			t.Errorf("version %q: Reason = %q, want %q", version, got.Reason, ReasonBehind)
		}
		if !git.sawSubcommand("merge-base") {
			t.Errorf("version %q: ancestry was never tested", version)
		}
	}
}

func TestInspectNoCheckoutIsUnknown(t *testing.T) {
	git := &fakeGit{}
	opts := optionsFor("realsha", "v1.2.3", git.runner())
	opts.FindCheckout = func(string) (string, bool) { return "", false }

	got := Inspect(context.Background(), opts)

	if got.Reason != ReasonNoCheckout {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonNoCheckout)
	}
	if got.BehindKnown {
		t.Errorf("BehindKnown = true with no checkout to compare against: %+v", got)
	}
	if got.CheckoutRoot != "" || got.CheckoutRevision != "" {
		t.Errorf("verdict names a checkout it never found: %+v", got)
	}
}

func TestInspectUntrustedCheckoutIsUnknown(t *testing.T) {
	git := &fakeGit{}
	opts := optionsFor("realsha", "v1.2.3", git.runner())
	opts.TrustCheckout = func(string) (bool, error) { return false, nil }

	got := Inspect(context.Background(), opts)

	if got.Reason != ReasonCheckoutUntrusted {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonCheckoutUntrusted)
	}
	if got.BehindKnown {
		t.Errorf("an unauthenticated look-alike produced a verdict: %+v", got)
	}
	if len(git.calls) != 0 {
		t.Errorf("an untrusted checkout's history was still read: %v", git.calls)
	}
}

func TestInspectTrustCheckoutErrorFailsClosedToUntrusted(t *testing.T) {
	git := &fakeGit{}
	opts := optionsFor("realsha", "v1.2.3", git.runner())
	opts.TrustCheckout = func(string) (bool, error) { return true, os.ErrPermission }

	got := Inspect(context.Background(), opts)

	if got.Reason != ReasonCheckoutUntrusted {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonCheckoutUntrusted)
	}
	if !strings.Contains(got.Detail, os.ErrPermission.Error()) {
		t.Errorf("Detail = %q, want the underlying cause", got.Detail)
	}
}

// TestInspectGitFailurePreservesTheRealMessage is the "not a bare exit status
// 1" assertion: a runner that cannot execute git at all must surface its own
// text, because an operator reading "exit status 1" learns nothing.
func TestInspectGitFailurePreservesTheRealMessage(t *testing.T) {
	git := func(_ context.Context, _ string, _ ...string) (GitResult, error) {
		return GitResult{}, exec.ErrNotFound
	}
	got := Inspect(context.Background(), optionsFor("realsha", "v1.2.3", git))

	if got.Reason != ReasonGitUnavailable {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonGitUnavailable)
	}
	if got.BehindKnown {
		t.Errorf("BehindKnown = true with no git: %+v", got)
	}
	if !strings.Contains(got.Detail, exec.ErrNotFound.Error()) {
		t.Errorf("Detail = %q, want the real git failure", got.Detail)
	}
	if got.Detail == "exit status 1" {
		t.Errorf("Detail collapsed to a bare exit status")
	}
}

func TestInspectAncestryRunFailureIsGitUnavailable(t *testing.T) {
	git := &fakeGit{
		head:       "headsha",
		commonDir:  t.TempDir(),
		ancestorer: context.DeadlineExceeded,
	}
	got := Inspect(context.Background(), optionsFor("realsha", "v1.2.3", git.runner()))

	if got.Reason != ReasonGitUnavailable {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonGitUnavailable)
	}
	if !strings.Contains(got.Detail, context.DeadlineExceeded.Error()) {
		t.Errorf("Detail = %q, want the timeout cause", got.Detail)
	}
}

func TestInspectNilGitRunnerIsUnknownNotAPanic(t *testing.T) {
	opts := optionsFor("realsha", "v1.2.3", nil)
	got := Inspect(context.Background(), opts)

	if got.Reason != ReasonGitUnavailable {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonGitUnavailable)
	}
	if got.BehindKnown {
		t.Errorf("BehindKnown = true with no runner: %+v", got)
	}
}

func TestInspectShallowCloneExplainsTheAbsentRevision(t *testing.T) {
	commonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(commonDir, "shallow"), []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatalf("write shallow marker: %v", err)
	}
	git := &fakeGit{head: "headsha", commonDir: commonDir, ancestor: GitResult{ExitCode: 128, Stderr: "fatal: Not a valid commit name deepsha"}}

	got := Inspect(context.Background(), optionsFor("deepsha", "v1.2.3", git.runner()))

	if got.Reason != ReasonRevisionAbsent {
		t.Fatalf("Reason = %q, want %q", got.Reason, ReasonRevisionAbsent)
	}
	if !got.ShallowClone {
		t.Errorf("ShallowClone = false with a shallow marker in the common git dir")
	}
	if !strings.Contains(got.Detail, "shallow") {
		t.Errorf("Detail = %q, want the shallow cause named", got.Detail)
	}
}

func TestInspectResolvesRelativeCommonDirAgainstTheCheckout(t *testing.T) {
	git := &fakeGit{head: "headsha", commonDir: ".git", ancestor: GitResult{ExitCode: 0}}
	got := Inspect(context.Background(), optionsFor("realsha", "v1.2.3", git.runner()))

	if want := filepath.Join("/checkout", ".git"); got.CloneGitDir != want {
		t.Fatalf("CloneGitDir = %q, want %q", got.CloneGitDir, want)
	}
}

// TestStampedRejectsTheBuildinfoDefault asserts the PRODUCER rather than this
// package's own constant list. The test binary carries no ldflags, so
// buildinfo.Commit here IS the no-stamp default; if that default is ever
// changed to a third literal, this fails instead of the unstamped outcome
// silently becoming unreachable.
func TestStampedRejectsTheBuildinfoDefault(t *testing.T) {
	if Stamped(buildinfo.Commit) {
		t.Fatalf("Stamped(%q) = true for the buildinfo no-ldflags default", buildinfo.Commit)
	}
	if Stamped("") {
		t.Errorf("Stamped(\"\") = true")
	}
	if !Stamped("da9dd17c9f") {
		t.Errorf("Stamped(%q) = false for a real revision", "da9dd17c9f")
	}
}

func TestDevVersionFallbackMatchesTheBuildinfoDefault(t *testing.T) {
	if buildinfo.Version != DevVersionFallback {
		t.Fatalf("buildinfo.Version default = %q, but DevVersionFallback = %q — the dev-build outcome no longer matches its producer",
			buildinfo.Version, DevVersionFallback)
	}
}

// TestExecGitReportsExitCodeAndStderr pins the production runner's contract
// against real git: a non-zero exit must arrive as an ExitCode with a nil
// error, carrying git's own message. Inspect's whole exit-1-versus-128 split
// rests on this, and a runner that returned the ExitError instead would make
// every absent revision read as git-unavailable.
func TestExecGitReportsExitCodeAndStderr(t *testing.T) {
	repo := initTestRepo(t)

	result, err := ExecGit(context.Background(), repo, "rev-parse", "--verify", "definitelynotarevision")
	if err != nil {
		t.Fatalf("ExecGit returned an error for a non-zero exit: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("ExitCode = 0 for an unresolvable revision")
	}
	if result.ExitCode < 128 {
		t.Errorf("ExitCode = %d, want >= 128 so it classifies as absent-from-clone", result.ExitCode)
	}
	if result.Stderr == "" {
		t.Errorf("Stderr is empty — git's real message was dropped")
	}

	head, err := ExecGit(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil || head.ExitCode != 0 || head.Stdout == "" {
		t.Fatalf("ExecGit rev-parse HEAD = (%+v, %v), want a trimmed sha", head, err)
	}
	if strings.ContainsAny(head.Stdout, "\n\r ") {
		t.Errorf("Stdout = %q, want it trimmed", head.Stdout)
	}
}

func TestExecGitNonAncestorIsExitOne(t *testing.T) {
	repo := initTestRepo(t)
	first := gitOutput(t, repo, "rev-parse", "HEAD")
	writeCommit(t, repo, "second.txt", "second")

	ancestor, err := ExecGit(context.Background(), repo, "merge-base", "--is-ancestor", first, "HEAD")
	if err != nil || ancestor.ExitCode != 0 {
		t.Fatalf("first commit is not reported as an ancestor: (%+v, %v)", ancestor, err)
	}

	orphan := makeUnrelatedCommit(t, repo)
	notAncestor, err := ExecGit(context.Background(), repo, "merge-base", "--is-ancestor", orphan, "HEAD")
	if err != nil {
		t.Fatalf("ExecGit: %v", err)
	}
	if notAncestor.ExitCode != 1 {
		t.Fatalf("ExitCode = %d for a non-ancestor commit, want exactly 1", notAncestor.ExitCode)
	}
}

// TestInspectFromLinkedWorktreeReadsTheClonesCommonGitDir is the structural
// test for the worktree distinction, run against real git through the real
// production runner.
//
// A main-worktree fixture is blind here: --absolute-git-dir and
// --git-common-dir return the same path there, so a classifier keyed on the
// wrong one stays green. From a linked worktree they differ, and the shallow
// marker — which only ever exists in the common dir — turns that difference
// into a failing assertion rather than a cosmetic one.
func TestInspectFromLinkedWorktreeReadsTheClonesCommonGitDir(t *testing.T) {
	repo := initTestRepo(t)
	behindRevision := gitOutput(t, repo, "rev-parse", "HEAD")
	writeCommit(t, repo, "second.txt", "second")
	headRevision := gitOutput(t, repo, "rev-parse", "HEAD")

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo, "worktree", "add", "-b", "linked-branch", linked)

	opts := Options{
		BinaryRevision: behindRevision,
		BinaryVersion:  "v1.2.3",
		StartDir:       linked,
		Git:            ExecGit,
		FindCheckout:   func(string) (string, bool) { return linked, true },
	}

	got := Inspect(context.Background(), opts)
	if !got.BehindKnown {
		t.Fatalf("no verdict from a linked worktree: %+v", got)
	}
	if got.Behind {
		t.Errorf("the first commit is an ancestor of the linked worktree HEAD: %+v", got)
	}
	if got.CheckoutRevision != headRevision {
		t.Errorf("CheckoutRevision = %q, want the linked worktree HEAD %q", got.CheckoutRevision, headRevision)
	}

	wantCommon := evalPath(t, filepath.Join(repo, ".git"))
	if evalPath(t, got.CloneGitDir) != wantCommon {
		t.Fatalf("CloneGitDir = %q (resolved %q), want the clone's common git dir %q — this is --absolute-git-dir, not --git-common-dir",
			got.CloneGitDir, evalPath(t, got.CloneGitDir), wantCommon)
	}

	// The marker is written where only the common git dir can see it. A
	// classifier keyed on the linked worktree's private git directory reports
	// ShallowClone false here and its own absent-revision detail loses the one
	// cause an operator can act on.
	if err := os.WriteFile(filepath.Join(repo, ".git", "shallow"), []byte(headRevision+"\n"), 0o600); err != nil {
		t.Fatalf("write shallow marker: %v", err)
	}
	shallow := Inspect(context.Background(), opts)
	if !shallow.ShallowClone {
		t.Fatalf("ShallowClone = false from a linked worktree of a clone marked shallow: %+v", shallow)
	}
}

func TestFindCheckoutRootReturnsTheRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, skillinstallSourceRelPath(), "skills", "boss")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir skill sources: %v", err)
	}

	got, ok := FindCheckoutRoot(nested)
	if !ok {
		t.Fatalf("FindCheckoutRoot(%q) found no checkout", nested)
	}
	if evalPath(t, got) != evalPath(t, root) {
		t.Fatalf("FindCheckoutRoot = %q, want the repository root %q", got, root)
	}

	if _, ok := FindCheckoutRoot(t.TempDir()); ok {
		t.Errorf("FindCheckoutRoot reported a checkout outside one")
	}
}

func skillinstallSourceRelPath() string {
	return filepath.FromSlash("services/boss/internal/skillinstall")
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	writeCommit(t, repo, "first.txt", "first")
	return repo
}

func writeCommit(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	runGit(t, repo, "add", name)
	runGit(t, repo, "commit", "-m", "add "+name)
}

// makeUnrelatedCommit produces a commit that is reachable in the object
// database but not from HEAD, which is what makes `merge-base --is-ancestor`
// exit 1 rather than 128.
func makeUnrelatedCommit(t *testing.T, repo string) string {
	t.Helper()
	runGit(t, repo, "checkout", "--orphan", "sidebranch")
	writeCommit(t, repo, "side.txt", "side")
	revision := gitOutput(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "checkout", "main")
	return revision
}

func runGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	if _, err := gitRun(t, repo, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := gitRun(t, repo, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func gitRun(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	result, err := ExecGit(context.Background(), repo, args...)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("exit %d: %s", result.ExitCode, result.Stderr)
	}
	return result.Stdout, nil
}

func evalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}

// --- caller-produced outcomes and fatal classification ----------------------

// TestUnknownBuildsCoherentFactFlags covers the defect a hand-built Drift
// literal produced: Inspect is the only other place the fact flags are
// established, so a caller that could not produce an input at all was setting
// RevisionStamped false BY OMISSION — which is the build-time claim "this
// binary carries no embedded revision", a claim nothing observed.
func TestUnknownBuildsCoherentFactFlags(t *testing.T) {
	got := Unknown(ReasonRevisionUnreadable, "run bossd --version: exec format error")

	if got.Reason != ReasonRevisionUnreadable {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonRevisionUnreadable)
	}
	if got.BehindKnown {
		t.Errorf("BehindKnown = true for an outcome with no comparison: %+v", got)
	}
	if !got.Unknown() {
		t.Errorf("Unknown() = false, want an undeterminable outcome")
	}
	if got.Relation() != "unknown" {
		t.Errorf("Relation() = %q, want unknown", got.Relation())
	}
	if got.RevisionStamped {
		t.Errorf("RevisionStamped = true with no revision read: %+v", got)
	}
	if got.BinaryRevision != "" {
		t.Errorf("BinaryRevision = %q, want empty — nothing was read", got.BinaryRevision)
	}
	if !strings.Contains(got.Detail, "exec format error") {
		t.Errorf("Detail = %q, want the underlying cause carried", got.Detail)
	}
}

// TestRevisionLabelUnreadableIsNotUnstamped is the renderer half of the same
// defect. RevisionStamped is false for an unreadable binary because nothing was
// read, and rendering that as "(unstamped)" asserts how the binary was BUILT —
// exactly the claim ReasonRevisionUnreadable exists to withhold.
func TestRevisionLabelUnreadableIsNotUnstamped(t *testing.T) {
	unreadable := Unknown(ReasonRevisionUnreadable, "boom")
	if got := unreadable.RevisionLabel(); got != "(unreadable)" {
		t.Fatalf("RevisionLabel() = %q, want (unreadable)", got)
	}
	if strings.Contains(unreadable.Describe("bossd binary"), "(unstamped)") {
		t.Errorf("Describe() asserts the build-time claim: %s", unreadable.Describe("bossd binary"))
	}

	// The sibling outcome must keep its own label, or the fix has merely
	// swapped which cause is misreported.
	unstamped := Drift{Reason: ReasonUnstamped}
	if got := unstamped.RevisionLabel(); got != "(unstamped)" {
		t.Errorf("RevisionLabel() = %q for the unstamped outcome, want (unstamped)", got)
	}
}

// TestInspectNonAbsentFatalIsGitUnavailableNotAbsent covers the exit-code
// collapse: `merge-base --is-ancestor` uses >=128 for EVERY fatal, not only for
// an unknown revision, and reading them all as ReasonRevisionAbsent publishes a
// wrong cause through `boss env --json`. Each stderr below was measured from
// git itself, not invented.
func TestInspectNonAbsentFatalIsGitUnavailableNotAbsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result GitResult
	}{
		{
			name:   "missing working directory",
			result: GitResult{ExitCode: 128, Stderr: "fatal: cannot change to '/gone': No such file or directory"},
		},
		{
			name:   "not a git repository",
			result: GitResult{ExitCode: 128, Stderr: "fatal: not a git repository (or any of the parent directories): .git"},
		},
		{
			name:   "corrupt object database",
			result: GitResult{ExitCode: 128, Stderr: "fatal: loose object abc123 is corrupt"},
		},
		{
			name:   "usage error",
			result: GitResult{ExitCode: 129, Stderr: "error: unknown option `bogusflag'"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := &fakeGit{head: "headsha", commonDir: t.TempDir(), ancestor: tc.result}
			got := Inspect(context.Background(), optionsFor("realsha", "v1.2.3", git.runner()))

			if got.Reason == ReasonRevisionAbsent {
				t.Fatalf("a %s fatal was reported as a revision absent from this clone: %+v", tc.name, got)
			}
			if got.Reason != ReasonGitUnavailable {
				t.Errorf("Reason = %q, want %q", got.Reason, ReasonGitUnavailable)
			}
			if got.BehindKnown {
				t.Errorf("BehindKnown = true for a fatal: %+v", got)
			}
			if !strings.Contains(got.Detail, tc.result.Stderr) {
				t.Errorf("Detail = %q, want git's own stderr preserved", got.Detail)
			}
			if got.ShallowClone {
				t.Errorf("ShallowClone = true with no shallow marker: %+v", got)
			}
		})
	}
}

// TestInspectAbsentRevisionStillClassifiesAsAbsent is the green-required
// sibling of the test above: narrowing the >=128 branch must not make the
// genuinely-absent outcome unreachable. Both of git's measured phrasings for
// an unknown name are covered, because a 40-hex SHA and a short name produce
// different ones.
func TestInspectAbsentRevisionStillClassifiesAsAbsent(t *testing.T) {
	for _, stderr := range []string{
		"fatal: Not a valid object name deadbeef",
		"fatal: Not a valid commit name 0000000000000000000000000000000000000001",
	} {
		git := &fakeGit{head: "headsha", commonDir: t.TempDir(), ancestor: GitResult{ExitCode: 128, Stderr: stderr}}
		got := Inspect(context.Background(), optionsFor("prunedsha", "v1.2.3", git.runner()))

		if got.Reason != ReasonRevisionAbsent {
			t.Errorf("%q: Reason = %q, want %q", stderr, got.Reason, ReasonRevisionAbsent)
		}
	}
}
