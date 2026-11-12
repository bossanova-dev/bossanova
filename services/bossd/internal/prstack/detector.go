package prstack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
)

const (
	// recomputeAfter bounds how long an unchanged open-PR listing reuses its
	// result, so commits that reach the default branch drop out of the ranges.
	recomputeAfter = 10 * time.Minute
	// retryAfter spaces out attempts after a git failure.
	retryAfter = time.Minute
)

// errPartial marks a recompute that served the heads it could read and must
// retry the rest.
var errPartial = errors.New("some PR heads could not be read")

// OpenPRLister reads a repository's open pull requests.
type OpenPRLister interface {
	ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error)
}

// Git reads commits from a repository's local clone.
type Git interface {
	FetchBaseTip(ctx context.Context, localPath, base string) (string, error)
	PatchIDs(ctx context.Context, localPath, baseTip string, heads []string) (map[string][]string, error)
}

// Result is what a session's open PR serves about its base and stack.
type Result struct {
	BaseBranch    string
	DefaultBranch string
	StackedOn     []Parent
}

func (r Result) equal(o Result) bool {
	return r.BaseBranch == o.BaseBranch && r.DefaultBranch == o.DefaultBranch && slices.Equal(r.StackedOn, o.StackedOn)
}

// repoState is one repository's last computation. ids holds patch ids by head
// SHA, all measured from tip. failing is set from a failure until the next
// complete pass, so a lasting failure logs one warning.
type repoState struct {
	key      string
	computed time.Time
	retryAt  time.Time
	failing  bool
	tip      string
	ids      map[string][]string
	parents  map[int][]Parent
	sessions map[string]struct{}
}

// Detector keeps each session's Result from the poll cycle's open-PR
// listing. Refresh runs on the poller; Stack serves readers.
type Detector struct {
	lister   OpenPRLister
	git      Git
	logger   zerolog.Logger
	now      func() time.Time
	onChange func(sessionID string)

	refreshMu sync.Mutex
	repos     map[string]*repoState

	mu      sync.Mutex
	results map[string]Result
}

// NewDetector returns a Detector reading through lister and git.
func NewDetector(lister OpenPRLister, git Git, logger zerolog.Logger) *Detector {
	return &Detector{lister: lister, git: git, logger: logger, now: time.Now, repos: make(map[string]*repoState), results: make(map[string]Result)}
}

// SetOnChange registers fn for every session whose Result changed. Call it
// before the first Refresh.
func (d *Detector) SetOnChange(fn func(sessionID string)) {
	d.onChange = fn
}

// Stack returns the session's Result; ok is false when its PR is not open.
func (d *Detector) Stack(sessionID string) (Result, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.results[sessionID]
	return r, ok
}

// Refresh recomputes repo's stacks when its open PRs changed or the last
// result is stale, then serves sessions from it. A repository with no
// session PR, local clone or default branch is not read and serves nothing.
// A listing or default-branch failure keeps the previous results; PR heads
// that could not be read carry no commits. Either retries a minute later.
func (d *Detector) Refresh(ctx context.Context, repo *models.Repo, sessions []*models.Session) {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	st := d.repos[repo.ID]
	if st == nil {
		st = &repoState{}
		d.repos[repo.ID] = st
	}
	hasPR := slices.ContainsFunc(sessions, func(s *models.Session) bool { return s.PRNumber != nil })
	if repo.LocalPath == "" || repo.DefaultBaseBranch == "" || !hasPR {
		d.apply(st, nil)
		return
	}
	prs, err := d.lister.ListOpenPRs(vcs.WithCachedReads(ctx), repo.OriginURL)
	if err != nil {
		d.logFailure(st, repo, fmt.Errorf("list open PRs: %w", err))
		return
	}
	if !d.refreshParents(ctx, repo, st, prs) {
		return
	}
	open := make(map[int]vcs.PRSummary, len(prs))
	for _, pr := range prs {
		open[pr.Number] = pr
	}
	next := make(map[string]Result)
	for _, sess := range sessions {
		if sess.PRNumber == nil {
			continue
		}
		if pr, ok := open[*sess.PRNumber]; ok {
			next[sess.ID] = Result{BaseBranch: pr.BaseBranch, DefaultBranch: repo.DefaultBaseBranch, StackedOn: st.parents[pr.Number]}
		}
	}
	d.apply(st, next)
}

// refreshParents recomputes st's parents when prs changed or the result is
// stale, unless a failed pass's retry is not yet due. It reports whether
// st.parents may be served for prs: a partial read may, a failed one may not.
func (d *Detector) refreshParents(ctx context.Context, repo *models.Repo, st *repoState, prs []vcs.PRSummary) bool {
	now := d.now()
	key := openPRKey(prs)
	if key == st.key && now.Sub(st.computed) < recomputeAfter {
		st.failing = false
		return true
	}
	if now.Before(st.retryAt) {
		return false
	}
	err := d.recompute(ctx, repo, st, prs)
	if err == nil {
		st.key, st.computed, st.failing = key, now, false
		return true
	}
	st.key, st.retryAt = "", now.Add(retryAfter)
	d.logFailure(st, repo, err)
	return errors.Is(err, errPartial)
}

func (d *Detector) recompute(ctx context.Context, repo *models.Repo, st *repoState, prs []vcs.PRSummary) error {
	tip, err := d.git.FetchBaseTip(ctx, repo.LocalPath, repo.DefaultBaseBranch)
	if err != nil {
		return fmt.Errorf("fetch default branch: %w", err)
	}
	if tip != st.tip {
		st.tip, st.ids = tip, nil
	}
	ids := make(map[string][]string, len(prs))
	var missing []string
	for _, pr := range prs {
		if got, ok := st.ids[pr.HeadSHA]; ok {
			ids[pr.HeadSHA] = got
		} else if pr.HeadSHA != "" {
			missing = append(missing, pr.HeadSHA)
		}
	}
	var readErr error
	if len(missing) > 0 {
		got, err := d.git.PatchIDs(ctx, repo.LocalPath, tip, missing)
		if err != nil {
			readErr = fmt.Errorf("%w: %w", errPartial, err)
		}
		for head, v := range got {
			ids[head] = v
		}
	}
	st.ids = ids
	in := make([]PR, 0, len(prs))
	byNumber := make(map[int][]string, len(prs))
	for _, pr := range prs {
		in = append(in, PR{Number: pr.Number, Title: pr.Title, URL: vcs.ConstructPRURL(repo.OriginURL, pr.Number), HeadBranch: pr.HeadBranch, BaseBranch: pr.BaseBranch})
		if v, ok := ids[pr.HeadSHA]; ok {
			byNumber[pr.Number] = v
		}
	}
	st.parents = Resolve(repo.DefaultBaseBranch, in, byNumber)
	return readErr
}

// logFailure warns on a repository's first failure and logs repeats at debug
// until a complete pass, since the poller retries every cycle.
func (d *Detector) logFailure(st *repoState, repo *models.Repo, err error) {
	level := zerolog.WarnLevel
	if st.failing {
		level = zerolog.DebugLevel
	}
	st.failing = true
	d.logger.WithLevel(level).Err(err).Str("repo", repo.ID).Msg("pr stack: read failed; retrying")
}

// apply serves next for st's repository and drops the sessions it no longer
// lists, then reports each session whose Result changed.
func (d *Detector) apply(st *repoState, next map[string]Result) {
	var changed []string
	d.mu.Lock()
	for id := range st.sessions {
		if _, ok := next[id]; !ok {
			delete(d.results, id)
			changed = append(changed, id)
		}
	}
	st.sessions = make(map[string]struct{}, len(next))
	for id, r := range next {
		st.sessions[id] = struct{}{}
		if prev, ok := d.results[id]; !ok || !prev.equal(r) {
			d.results[id] = r
			changed = append(changed, id)
		}
	}
	d.mu.Unlock()
	if d.onChange == nil {
		return
	}
	slices.Sort(changed)
	for _, id := range changed {
		d.onChange(id)
	}
}

// openPRKey names everything about the open PRs that changes their stacks.
func openPRKey(prs []vcs.PRSummary) string {
	sorted := slices.SortedFunc(slices.Values(prs), func(a, b vcs.PRSummary) int { return cmp.Compare(a.Number, b.Number) })
	var b strings.Builder
	for _, pr := range sorted {
		fmt.Fprintf(&b, "%d\x00%s\x00%s\x00%s\n", pr.Number, pr.HeadSHA, pr.HeadBranch, pr.BaseBranch)
	}
	return b.String()
}
