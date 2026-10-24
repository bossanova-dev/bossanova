// Package verifyevent runs a repo's verify gate in response to PR webhooks.
package verifyevent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/gatecmd"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/projectconfig"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

const coalesceWindow = 5 * time.Second
const memoTTL = 24 * time.Hour

// These markers are written and parsed by skills-toolbox/verify/verify-gate.mjs.
const approveMarker = "<!-- boss-approve head="
const sendBackMarker = "<!-- boss-send-back head="

type repoStore interface {
	GetByOriginURL(context.Context, string) (*domain.Repo, error)
}
type jobStore interface {
	ListByRepo(context.Context, string) ([]*domain.CronJob, error)
}
type checksReader interface {
	ChecksPassedReady(context.Context, string, string, int) (string, bool, error)
}
type prKey struct {
	origin string
	number int
}
type memo struct {
	head string
	at   time.Time
}
type pending struct {
	class      string
	queued     bool
	generation uint64
}

// Trigger coalesces per-PR notifications and bounds concurrent gate processes.
// Its dependencies and proof resolver must be configured before the first Notify.
type Trigger struct {
	repos    repoStore
	jobs     jobStore
	checks   checksReader
	proof    cron.GateProofEnvResolver
	logger   zerolog.Logger
	mu       sync.Mutex
	active   map[prKey]*pending
	heads    map[prKey]memo
	slots    chan struct{}
	now      func() time.Time
	wait     func(context.Context) bool
	run      func(context.Context, gatecmd.Options) gatecmd.Result
	doneHook func(<-chan struct{}) // tests await detached workers
}

// New constructs an event trigger. A nil checks reader disables checks events.
func New(repos repoStore, jobs jobStore, checks checksReader, proof cron.GateProofEnvResolver, logger zerolog.Logger) *Trigger {
	return &Trigger{repos: repos, jobs: jobs, checks: checks, proof: proof, logger: logger,
		active: make(map[prKey]*pending), heads: make(map[prKey]memo), slots: make(chan struct{}, 2),
		now: time.Now, run: gatecmd.Run, wait: func(ctx context.Context) bool {
			timer := time.NewTimer(coalesceWindow)
			defer timer.Stop()
			select {
			case <-timer.C:
				return true
			case <-ctx.Done():
				return false
			}
		}}
}

func classify(ev *pb.WebhookEvent) string {
	var payload struct {
		Action  string `json:"action"`
		Comment struct {
			Body string `json:"body"`
		} `json:"comment"`
		Review struct {
			State string `json:"state"`
		} `json:"review"`
	}
	if ev == nil {
		return ""
	}
	if ev.GetEventType() == "status" {
		return "checks"
	}
	if json.Unmarshal(ev.GetPayload(), &payload) != nil {
		return ""
	}
	switch ev.GetEventType() {
	case "check_suite", "check_run", "workflow_run":
		if payload.Action == "completed" {
			return "checks"
		}
	case "pull_request":
		switch payload.Action {
		case "ready_for_review":
			return "checks"
		case "synchronize", "closed":
			return "forget"
		}
	case "issue_comment":
		if payload.Action == "created" && (strings.Contains(payload.Comment.Body, approveMarker) || strings.Contains(payload.Comment.Body, sendBackMarker)) {
			return "hand-back"
		}
	case "pull_request_review":
		if payload.Action == "submitted" && strings.EqualFold(payload.Review.State, "approved") {
			return "hand-back"
		}
	}
	return ""
}

// Notify never waits on GitHub or a gate process, keeping webhook acknowledgement fast.
func (t *Trigger) Notify(ctx context.Context, ev *pb.WebhookEvent, prNumber int) {
	if t == nil || ev == nil || prNumber <= 0 {
		return
	}
	class := classify(ev)
	if class == "" {
		return
	}
	key := prKey{ev.GetRepoOriginUrl(), prNumber}
	if vcs.GitHubNWO(key.origin) == "" {
		return
	}
	t.mu.Lock()
	for k, m := range t.heads {
		if t.now().Sub(m.at) >= memoTTL {
			delete(t.heads, k)
		}
	}
	if class == "forget" {
		delete(t.heads, key)
		if p := t.active[key]; p != nil {
			p.generation++
			p.queued = false
			p.class = ""
		}
		t.mu.Unlock()
		return
	}
	if p := t.active[key]; p != nil {
		p.queued = true
		if class == "hand-back" || p.class == "" {
			p.class = class
		}
		t.mu.Unlock()
		return
	}
	p := &pending{class: class, queued: true}
	t.active[key] = p
	t.mu.Unlock()
	done := safego.Go(t.logger, func() { t.work(context.WithoutCancel(ctx), key, p) })
	if t.doneHook != nil {
		t.doneHook(done)
	}
}

func (t *Trigger) work(parent context.Context, key prKey, p *pending) {
	defer func() {
		t.mu.Lock()
		if t.active[key] == p {
			delete(t.active, key)
		}
		t.mu.Unlock()
	}()
	for {
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		if !t.wait(ctx) {
			cancel()
			return
		}
		t.mu.Lock()
		class, generation := p.class, p.generation
		p.queued, p.class = false, ""
		t.mu.Unlock()
		if class != "" {
			t.execute(ctx, key, class, generation, p)
		}
		cancel()
		t.mu.Lock()
		if !p.queued {
			delete(t.active, key)
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
	}
}

func (t *Trigger) execute(ctx context.Context, key prKey, class string, generation uint64, p *pending) {
	nwo := vcs.GitHubNWO(key.origin)
	owner, name, ok := strings.Cut(nwo, "/")
	if !ok {
		return
	}
	ctx = vcs.WithCachedReads(ctx)
	var head string
	if class == "checks" {
		if t.checks == nil {
			return
		}
		var ready bool
		var err error
		head, ready, err = t.checks.ChecksPassedReady(ctx, owner, name, key.number)
		if err != nil {
			t.logger.Warn().Err(err).Str("repo", nwo).Int("pr", key.number).Msg("verify event: checks read failed")
			return
		}
		if !ready || head == "" {
			return
		}
		t.mu.Lock()
		m := t.heads[key]
		valid := generation == p.generation
		t.mu.Unlock()
		if !valid || (m.head == head && t.now().Sub(m.at) < memoTTL) {
			return
		}
	}
	if t.repos == nil || t.jobs == nil {
		return
	}
	repo, err := t.repos.GetByOriginURL(ctx, key.origin)
	if err != nil || repo == nil {
		t.logger.Warn().Err(err).Str("repo", nwo).Msg("verify event: repo lookup failed")
		return
	}
	jobs, err := t.jobs.ListByRepo(ctx, repo.ID)
	if err != nil {
		t.logger.Warn().Err(err).Str("repo", nwo).Msg("verify event: job lookup failed")
		return
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i] == nil {
			return false
		}
		if jobs[j] == nil {
			return true
		}
		return jobs[i].ID < jobs[j].ID
	})
	stage, _ := projectconfig.DefaultFactoryStages().Lookup("verify")
	var job *domain.CronJob
	for _, candidate := range jobs {
		if candidate != nil && candidate.IsEnabled && strings.TrimSpace(candidate.GateCommand) != "" && (stage.MatchesPrompt(candidate.Prompt) || stage.MatchesGate(candidate.GateCommand)) {
			job = candidate
			break
		}
	}
	if job == nil {
		t.logger.Debug().Str("repo", nwo).Msg("verify event: no enabled verify gate")
		return
	}
	select {
	case t.slots <- struct{}{}:
		defer func() { <-t.slots }()
	case <-ctx.Done():
		return
	}
	t.mu.Lock()
	valid := generation == p.generation
	t.mu.Unlock()
	if !valid {
		return
	}
	var output tailWriter
	opts := cron.GateOptions(repo, job, t.proof, false, nil)
	opts.Output = &output
	opts.ExtraEnv = map[string]string{"BOSS_VERIFY_EVENT_PR_URL": fmt.Sprintf("https://github.com/%s/pull/%d", nwo, key.number)}
	result := t.run(ctx, opts)
	event := t.logger.Info()
	if result.CouldNotRun() {
		event = t.logger.Warn()
	}
	event.Err(result.Err).Str("repo", nwo).Int("pr", key.number).Str("class", class).Int("exit_code", result.ExitCode).Str("gate_output", string(output.data)).Msg("verify event")
	if class == "checks" && !result.CouldNotRun() {
		t.mu.Lock()
		if generation == p.generation {
			t.heads[key] = memo{head, t.now()}
		}
		t.mu.Unlock()
	}
}

// tailWriter retains at most 8 KiB even for noisy gate processes.
type tailWriter struct{ data []byte }

func (w *tailWriter) Write(b []byte) (int, error) {
	n := len(b)
	if len(b) >= 8192 {
		w.data = append(w.data[:0], b[len(b)-8192:]...)
		return n, nil
	}
	if excess := len(w.data) + len(b) - 8192; excess > 0 {
		w.data = w.data[excess:]
	}
	w.data = append(w.data, b...)
	return n, nil
}
