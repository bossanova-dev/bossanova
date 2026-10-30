// Package verifybridge runs the installed boss-verify toolbox's operator verbs
// (`verify-gate.mjs approve` and `mark-merged`) for one PR and decodes their
// JSON.
//
// The verbs live in verify-gate because the park format, the needs-human label
// and the tracker adapter already live there; this package only bridges. Like
// the `boss init` node bridge, it is fail-closed: node missing, a non-zero exit,
// a timeout, empty stdout, unparseable JSON and a result of the wrong shape are
// each their own error, and none of them is ever read as success.
package verifybridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

// scriptInToolbox is verify-gate.mjs relative to the boss-verify toolbox root,
// the directory the skill's $BOSS_VERIFY_TOOLBOX names.
var scriptInToolbox = filepath.Join("verify", "verify-gate.mjs")

// ScriptRelPath is verify-gate.mjs relative to an installed skills tree.
var ScriptRelPath = filepath.Join("boss-verify", "toolbox", scriptInToolbox)

// DefaultTimeout bounds one verb run. approve, the larger verb, makes about
// twenty sequential gh and tracker calls (reads, permission checks, the record,
// its read-back and the label write); anything slower is a hang. A timeout after
// the record was posted leaves its effect unknown, so the operator re-runs
// approve to confirm.
//
// bossd runs the verbs inside a unary RPC, so this stays below the daemon's
// 120s http.Server WriteTimeout: a verb that outlived it would have its
// response dropped, and the client would see a transport error instead of the
// timeout that tells the operator to re-run approve.
const DefaultTimeout = domain.VerifyVerbTimeout

const waitDelay = 5 * time.Second

// Aliases preserve the bridge API while consumers depend on domain values.
type Target = domain.VerifyTarget
type MergeApproval = domain.VerifyMergeApproval
type ApproveResult = domain.VerifyApproveResult
type MarkMergedResult = domain.VerifyMarkMergedResult
type MarkMergedWrite = domain.VerifyMarkMergedWrite

var (
	ErrToolboxMissing = domain.ErrVerifyToolboxMissing
	ErrNodeMissing    = domain.ErrVerifyNodeMissing
	ErrNodeExit       = domain.ErrVerifyNodeExit
	ErrNodeTimeout    = domain.ErrVerifyNodeTimeout
	ErrEmptyOutput    = domain.ErrVerifyEmptyOutput
	ErrBadJSON        = domain.ErrVerifyBadJSON
	ErrShape          = domain.ErrVerifyShape
	ErrInvalidTarget  = domain.ErrVerifyInvalidTarget
)

// Bridge runs verify-gate.mjs. The zero value uses `node` from PATH, locates the
// script under the installed skill trees, applies DefaultTimeout and lets the
// child inherit this process's environment.
type Bridge struct {
	Node    string        // node executable; empty looks `node` up on PATH
	Script  string        // verify-gate.mjs; empty locates the installed copy
	Timeout time.Duration // zero means DefaultTimeout
	// Env is the child's whole environment, as exec.Cmd.Env takes it; nil
	// inherits this process's. Values are secret-bearing and never logged.
	Env []string
}

// Locate finds the installed verify-gate.mjs the way the skills find their
// toolbox: $BOSS_VERIFY_TOOLBOX, then $BOSS_SKILLS_HOME, then ~/.claude/skills,
// then ~/.codex/skills. The first existing regular file wins.
func Locate(getenv func(string) string, home string) (string, error) {
	var candidates []string
	if dir := getenv("BOSS_VERIFY_TOOLBOX"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, scriptInToolbox))
	}
	if dir := getenv("BOSS_SKILLS_HOME"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, ScriptRelPath))
	}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".claude", "skills", ScriptRelPath),
			filepath.Join(home, ".codex", "skills", ScriptRelPath))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.Mode().IsRegular() {
			// The script runs with cmd.Dir set to the PR worktree, so a relative
			// candidate (BOSS_SKILLS_HOME=./skills) must be anchored here first.
			abs, err := filepath.Abs(c)
			if err != nil {
				return "", fmt.Errorf("%w: resolve %s: %v", ErrToolboxMissing, c, err)
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: looked in %s; run `boss skills install`", ErrToolboxMissing, strings.Join(candidates, ", "))
}

// Approve hands the standing park on t's current head back to verify. A
// non-nil forMerge signs off an operator merge instead: approve runs with
// --for-merge pinned to forMerge's head and waived codes.
func (b *Bridge) Approve(ctx context.Context, t Target, forMerge *MergeApproval) (ApproveResult, error) {
	args, err := verbArgs("approve", t)
	if err != nil {
		return ApproveResult{}, err
	}
	if forMerge != nil {
		if !shaPattern.MatchString(forMerge.Head) {
			return ApproveResult{}, fmt.Errorf("%w: merge approval head %q is not a 40-hex SHA", ErrInvalidTarget, forMerge.Head)
		}
		// Inserted before the trailing --json; `--waives=` keeps an empty set
		// an explicit "nothing parked".
		args = append(args[:len(args)-1], "--for-merge", "--head="+forMerge.Head, "--waives="+strings.Join(forMerge.Waives, ","), args[len(args)-1])
	}
	out, err := b.run(ctx, t.Dir, args)
	if err != nil {
		return ApproveResult{}, err
	}
	var wire struct {
		ApproveResult
		Approved *bool `json:"approved"`
	}
	if err := decode(out, &wire); err != nil {
		return ApproveResult{}, err
	}
	if wire.Approved == nil {
		return ApproveResult{}, fmt.Errorf("%w: approve printed no `approved` field: %s", ErrShape, truncate(out))
	}
	r := wire.ApproveResult
	r.Approved = *wire.Approved
	switch {
	case r.Approved && (r.HeadSHA == "" || r.ApprovalURL == "" || (r.Waives == "" && r.Lifted != "sent-back" && r.SignOff != "merge")):
		return ApproveResult{}, fmt.Errorf("%w: approve reported approved without a head, an approval record and waived codes, a lifted send-back or a merge sign-off (an older verify-gate removes the needs-human label without a record: re-add the label and run `boss skills install`): %s", ErrShape, truncate(out))
	case r.Approved && forMerge != nil && !strings.EqualFold(r.HeadSHA, forMerge.Head):
		return ApproveResult{}, fmt.Errorf("%w: approve --for-merge approved head %s, not the pinned %s: %s", ErrShape, r.HeadSHA, forMerge.Head, truncate(out))
	case !r.Approved && !r.Planned && r.Reason == "":
		return ApproveResult{}, fmt.Errorf("%w: approve refused without a reason: %s", ErrShape, truncate(out))
	}
	return r, nil
}

// MarkMerged runs the post-merge tracker bookkeeping for t's merged PR.
func (b *Bridge) MarkMerged(ctx context.Context, t Target) (MarkMergedResult, error) {
	args, err := verbArgs("mark-merged", t)
	if err != nil {
		return MarkMergedResult{}, err
	}
	out, err := b.run(ctx, t.Dir, args)
	if err != nil {
		return MarkMergedResult{}, err
	}
	var wire struct {
		MarkMergedResult
		Merged *bool `json:"merged"`
	}
	if err := decode(out, &wire); err != nil {
		return MarkMergedResult{}, err
	}
	if wire.Merged == nil {
		return MarkMergedResult{}, fmt.Errorf("%w: mark-merged printed no `merged` field: %s", ErrShape, truncate(out))
	}
	r := wire.MarkMergedResult
	r.Merged = *wire.Merged
	switch {
	case r.Merged && r.TrackerWrites == "":
		return MarkMergedResult{}, fmt.Errorf("%w: mark-merged reported merged without a tracker outcome: %s", ErrShape, truncate(out))
	case !r.Merged && r.Reason == "":
		return MarkMergedResult{}, fmt.Errorf("%w: mark-merged reported not merged without a reason: %s", ErrShape, truncate(out))
	}
	return r, nil
}

var (
	repoSlug   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

// verbArgs validates t and renders the verb's argv. The last element is always
// `--json`, so callers may insert verb-specific flags before it.
func verbArgs(verb string, t Target) ([]string, error) {
	if t.PR < 1 {
		return nil, fmt.Errorf("%w: PR number must be positive, got %d", ErrInvalidTarget, t.PR)
	}
	if t.Repo != "" && !repoSlug.MatchString(t.Repo) {
		return nil, fmt.Errorf("%w: repo %q is not owner/repo", ErrInvalidTarget, t.Repo)
	}
	if t.Dir == "" {
		return nil, fmt.Errorf("%w: the PR's session has no worktree path", ErrInvalidTarget)
	}
	if info, err := os.Stat(t.Dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: worktree %s is not a readable directory", ErrInvalidTarget, t.Dir)
	}
	args := []string{verb, "--pr=" + strconv.Itoa(t.PR)}
	if t.Repo != "" {
		args = append(args, "--repo="+t.Repo)
	}
	if t.DryRun {
		args = append(args, "--dry-run")
	}
	return append(args, "--json"), nil
}

func (b *Bridge) run(ctx context.Context, dir string, args []string) ([]byte, error) {
	script := b.Script
	if script == "" {
		home, _ := os.UserHomeDir()
		found, err := Locate(os.Getenv, home)
		if err != nil {
			return nil, err
		}
		script = found
	}
	nodePath := b.Node
	if nodePath == "" {
		found, err := exec.LookPath("node")
		if err != nil {
			return nil, fmt.Errorf("%w: the boss-verify toolbox needs `node` on PATH: %v", ErrNodeMissing, err)
		}
		nodePath = found
	}
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	what := "verify-gate " + args[0]

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodePath, append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = b.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %s did not finish within %s: %s", ErrNodeTimeout, what, timeout, truncate(stderr.Bytes()))
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%w: %s failed with exit status %d: %s", ErrNodeExit, what, exitErr.ExitCode(), truncate(stderr.Bytes()))
		}
		return nil, fmt.Errorf("%w: %s could not be run: %v: %s", ErrNodeExit, what, err, truncate(stderr.Bytes()))
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s exited 0 but printed nothing; that is a broken bridge, not a result: %s", ErrEmptyOutput, what, truncate(stderr.Bytes()))
	}
	return out, nil
}

// decode parses exactly one JSON object.
func decode(out []byte, v any) error {
	if !bytes.HasPrefix(out, []byte("{")) {
		return fmt.Errorf("%w: want one JSON object: %s", ErrBadJSON, truncate(out))
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("%w: %v: %s", ErrBadJSON, err, truncate(out))
	}
	return nil
}

func truncate(b []byte) string {
	const limit = 2000
	s := strings.TrimSpace(string(b))
	if len(s) > limit {
		return s[:limit] + "… (truncated)"
	}
	return s
}
