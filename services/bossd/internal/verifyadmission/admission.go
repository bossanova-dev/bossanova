// Package verifyadmission reads the verify gate's persisted admission facts.
package verifyadmission

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Snapshot is a versioned admission scan across the repository's verify scopes.
// Its zero value has no known admission answer.
type Snapshot struct {
	Version int              `json:"version"`
	Scopes  map[string]scope `json:"scopes"`
}

type scope struct {
	ScannedAt string           `json:"scannedAt"`
	PRs       map[string]entry `json:"prs"`
	// Remote lists the PRs the scan skipped because their live session is on
	// another machine. Nil means the writer recorded no list (an older writer,
	// or a scope no trustworthy full scan has reached), which is not "none".
	Remote *[]int `json:"remote,omitempty"`
}

type entry struct {
	Head   string `json:"head"`
	State  string `json:"state"`
	Reason string `json:"reason"`
	// Rule names the selection rule a ticket-not-selected skip failed
	// (labels, assignees, creators, projects or unreadable); absent otherwise.
	Rule string `json:"rule,omitempty"`
}

// Read returns an empty snapshot on missing, unreadable, or unsupported files.
func Read(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read verify admission: %w", err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("parse verify admission: %w", err)
	}
	if snapshot.Version != 1 {
		return Snapshot{}, fmt.Errorf("unsupported verify admission version %d", snapshot.Version)
	}
	return snapshot, nil
}

// Admission states persisted by the verify gate.
const (
	StateAdmitted = "admitted"
	StatePending  = "pending"
	StateExcluded = "excluded"
)

// Answer is the admission state, reason and failed selection rule for a
// particular PR head. Its zero value means the snapshot has no conclusive answer.
type Answer struct {
	State  string
	Reason string
	Rule   string
}

// Lookup combines matching scope answers: admitted wins, then pending, then
// excluded only if every matching scope excludes the head. Tied reasons, then
// tied rules, sort lexically so the result is independent of scope iteration
// order, except that an unprobed pending default loses to a decided pending reason.
func (s Snapshot) Lookup(prNumber int, headSHA string) Answer {
	if s.Version != 1 || prNumber <= 0 || headSHA == "" {
		return Answer{}
	}
	key := strconv.Itoa(prNumber)
	var admitted, pending, excluded Answer
	unknown := false
	for _, scope := range s.Scopes {
		e, exists := scope.PRs[key]
		if !exists || !strings.EqualFold(e.Head, headSHA) {
			continue
		}
		var candidate *Answer
		switch e.State {
		case StateAdmitted:
			candidate = &admitted
		case StatePending:
			candidate = &pending
		case StateExcluded:
			candidate = &excluded
		default:
			unknown = true
			continue
		}
		prefer := e.Reason < candidate.Reason || (e.Reason == candidate.Reason && ruleBefore(e.Rule, candidate.Rule))
		if e.State == StatePending && (e.Reason == "ticket-not-in-review") != (candidate.Reason == "ticket-not-in-review") {
			prefer = candidate.Reason == "ticket-not-in-review"
		}
		if candidate.State == "" || prefer {
			*candidate = Answer{State: e.State, Reason: e.Reason, Rule: e.Rule}
		}
	}
	if admitted.State != "" {
		return admitted
	}
	if pending.State != "" {
		return pending
	}
	if !unknown {
		return excluded
	}
	return Answer{}
}

// RemotePRs returns, ascending, the PRs the newest scan with a remote list
// left to another machine's session while admitting them. Scopes without a
// list or a parseable scannedAt are ignored, and a tie on scannedAt goes to
// the lexically smallest scope key. A remote PR the scan did not admit is
// still being built or is not ready for verify, so it is not reported. Nil
// when no scope qualifies or the version is unsupported.
func (s Snapshot) RemotePRs() []int {
	if s.Version != 1 {
		return nil
	}
	var (
		newestKey string
		newestAt  time.Time
		found     bool
	)
	for key, sc := range s.Scopes {
		if sc.Remote == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, sc.ScannedAt)
		if err != nil {
			continue
		}
		if !found || at.After(newestAt) || (at.Equal(newestAt) && key < newestKey) {
			newestKey, newestAt, found = key, at, true
		}
	}
	if !found {
		return nil
	}
	newest := s.Scopes[newestKey]
	var out []int
	for _, pr := range *newest.Remote {
		if e, ok := newest.PRs[strconv.Itoa(pr)]; ok && e.State == StateAdmitted && !slices.Contains(out, pr) {
			out = append(out, pr)
		}
	}
	slices.Sort(out)
	return out
}

// Path resolves the admission file shared by all worktrees of repoPath.
func Path(ctx context.Context, repoPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve verify admission git common dir: %w", err)
	}
	commonDir := strings.TrimSpace(string(out))
	if commonDir == "" || !filepath.IsAbs(commonDir) {
		return "", fmt.Errorf("invalid git common dir %q", commonDir)
	}
	return filepath.Join(commonDir, "boss-verify", "verify-admission.json"), nil
}

// ruleBefore orders recorded rules lexically and ahead of an absent one, so a
// stale rule-less scope never hides a scope that names the failed rule.
func ruleBefore(a, b string) bool {
	if (a == "") != (b == "") {
		return b == ""
	}
	return a < b
}
