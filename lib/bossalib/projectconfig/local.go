package projectconfig

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
)

// The local leg applies a project config to this daemon: ObserveLocal does
// every read, PlanLocal is a pure function of the file, the observed state and
// the options, and ApplyLocal executes the plan. Matching goes through
// MatchDaemon and cron classification through Reconcile; this file only builds
// their inputs and maps their verdicts to writes.

// LocalReader is the read subset of the daemon client the local leg needs.
type LocalReader interface {
	RepoReader
	ValidateRepoPath(ctx context.Context, localPath string) (*pb.ValidateRepoPathResponse, error)
}

// ErrUnknownFactoryStage is what a FactoryResolver returns for a stage its
// table does not hold.
var ErrUnknownFactoryStage = errors.New("unknown factory stage")

// FactoryCron is what one factory stage resolves to on this daemon: the stage
// skill, the agent chosen to run it, the job prompt and gate command for that
// agent, the stage's default schedule, and every gate preflight failure.
type FactoryCron struct {
	Skill             string
	Agent             string
	Prompt            string
	GateCommand       string
	DefaultSchedule   string
	PreflightFailures []string
}

// FactoryResolver resolves a factory stage for an agent ("" chooses the
// default eligible agent). It returns ErrUnknownFactoryStage for a stage it
// does not know, and another error when no agent can run the stage.
type FactoryResolver interface {
	Resolve(stage, agent string) (FactoryCron, error)
}

// FactoryResolution is one cron's resolution, or why it failed.
type FactoryResolution struct {
	Cron FactoryCron
	Err  error
}

// LocalOptions are the inputs ObserveLocal and PlanLocal share.
type LocalOptions struct {
	// Daemon is this daemon's identity, matched against the file's daemons.
	Daemon DaemonIdentity
	// CheckoutPath is the checkout being applied: the repo's place when this
	// daemon matches no entry (or several), or its entry has no path.
	CheckoutPath string
	// HomeDir expands ~/ in daemons[].path and $HOME in factory gates.
	HomeDir string
	// Prune deletes own cron rows the file dropped instead of disabling them.
	Prune bool
	// Secrets resolves BOSS_* references; nil skips every secret.
	Secrets SecretResolver
	// HostFreeFactory plans factory crons without this host's gate, for a
	// caller that cannot resolve the gate's node and skill-root paths (MCP),
	// whose FactoryResolver is HostFreeFactory: an existing factory row
	// converges every field but its gate, and a factory cron with no row is
	// skipped ReasonCreateViaCLI rather than created ungated.
	HostFreeFactory bool
	// MainCheckout maps a path to its repository's main checkout so a
	// registration made through an alias or a linked worktree is recognised.
	// nil compares cleaned paths. Only ObserveLocal calls it.
	MainCheckout func(ctx context.Context, path string) (string, error)
}

// LocalState is everything ObserveLocal read.
type LocalState struct {
	Repos []*pb.Repo
	// RepoCheckouts maps a registration id to its main checkout.
	RepoCheckouts map[string]string
	// TargetPath is where the file places the repo on this daemon.
	TargetPath string
	// TargetCheckout is TargetPath's main checkout; empty when it is not one.
	TargetCheckout string
	Validation     *pb.ValidateRepoPathResponse
	ValidationErr  string
	// CronJobs are the jobs of the registration at TargetCheckout.
	CronJobs []*pb.CronJob
	// Factory holds each selected factory cron's resolution, by cron name.
	Factory map[string]FactoryResolution
}

// Registration is the registration whose main checkout is the target path,
// or nil when there is none.
func (s *LocalState) Registration() *pb.Repo {
	if s == nil || s.TargetCheckout == "" {
		return nil
	}
	for _, r := range s.Repos {
		if s.RepoCheckouts[r.GetId()] == s.TargetCheckout {
			return r
		}
	}
	return nil
}

// DefaultBranch is the repo's default branch as far as the file and this
// daemon know it: the file's defaultBaseBranch, else the registration's, else
// the checkout's detected HEAD branch; empty when none is known.
func (s *LocalState) DefaultBranch(cfg *Config) string {
	if cfg != nil && cfg.Repo.DefaultBaseBranch != nil && *cfg.Repo.DefaultBaseBranch != "" {
		return *cfg.Repo.DefaultBaseBranch
	}
	if r := s.Registration(); r != nil && r.GetDefaultBaseBranch() != "" {
		return r.GetDefaultBaseBranch()
	}
	if s != nil {
		return s.Validation.GetDefaultBranch()
	}
	return ""
}

// target is where the file places the repo on this daemon.
type target struct {
	verdict MatchVerdict
	entry   *Daemon // nil unless matched by id or name
	path    string
}

func resolveTarget(cfg *Config, opts LocalOptions) target {
	idx, verdict := MatchDaemon(cfg.Daemons, opts.Daemon)
	t := target{verdict: verdict, path: opts.CheckoutPath}
	if idx >= 0 {
		entry := cfg.Daemons[idx]
		t.entry = &entry
		if entry.Path != nil && *entry.Path != "" {
			t.path = expandTilde(*entry.Path, opts.HomeDir)
		}
	}
	return t
}

// expandTilde expands a leading ~/ against home.
func expandTilde(p, home string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok && home != "" {
		return filepath.Join(home, filepath.FromSlash(rest))
	}
	return filepath.Clean(p)
}

// selectedCrons are the file's crons the matched entry runs: those naming it,
// or every cron when the file has exactly one entry and the cron names none.
func selectedCrons(cfg *Config, entry *Daemon) []Cron {
	if entry == nil {
		return nil
	}
	var out []Cron
	for _, c := range cfg.Crons {
		if (c.Daemons == nil && len(cfg.Daemons) == 1) || slices.Contains(c.Daemons, entry.Name) {
			out = append(out, c)
		}
	}
	return out
}

// CronsForDaemon is the file's crons the daemon with identity id runs: none
// unless exactly one daemons entry matches it.
func CronsForDaemon(cfg *Config, id DaemonIdentity) []Cron {
	return selectedCrons(cfg, resolveTarget(cfg, LocalOptions{Daemon: id}).entry)
}

// ObserveLocal reads what PlanLocal needs: the registrations and their main
// checkouts, the target path's validation, the target registration's cron
// jobs, and each selected factory cron's resolution (nil factory leaves them
// unresolved, which plans as skip).
func ObserveLocal(ctx context.Context, cfg *Config, r LocalReader, factory FactoryResolver, opts LocalOptions) (*LocalState, error) {
	if cfg == nil {
		return nil, errors.New("observe: no config")
	}
	mainCheckout := opts.MainCheckout
	if mainCheckout == nil {
		mainCheckout = func(_ context.Context, p string) (string, error) { return filepath.Clean(p), nil }
	}
	t := resolveTarget(cfg, opts)
	st := &LocalState{TargetPath: t.path, RepoCheckouts: map[string]string{}, Factory: map[string]FactoryResolution{}}

	repos, err := r.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repository registrations: %w", err)
	}
	st.Repos = repos
	for _, repo := range repos {
		if key, err := mainCheckout(ctx, repo.GetLocalPath()); err == nil {
			st.RepoCheckouts[repo.GetId()] = key
		}
	}
	if t.path != "" {
		v, err := r.ValidateRepoPath(ctx, t.path)
		if err != nil {
			st.ValidationErr = err.Error()
		} else {
			st.Validation = v
		}
		if key, err := mainCheckout(ctx, t.path); err == nil {
			st.TargetCheckout = key
		}
	}
	if t.entry == nil || cfg.Crons == nil {
		return st, nil
	}
	if repo := st.Registration(); repo != nil {
		jobs, err := r.ListCronJobs(ctx, repo.GetId())
		if err != nil {
			return nil, fmt.Errorf("list the repository's cron jobs: %w", err)
		}
		st.CronJobs = jobs
	}
	if factory != nil {
		for _, c := range selectedCrons(cfg, t.entry) {
			if !c.IsFactory() || c.Unsupported() {
				continue
			}
			fc, err := factory.Resolve(*c.FactoryStage, deref(c.Agent))
			st.Factory[c.Name] = FactoryResolution{Cron: fc, Err: err}
		}
	}
	return st, nil
}

// LocalClass is a class of local writes. Classes apply in the order repo,
// secrets, crons; each stops at its first failed write.
type LocalClass string

// Local write classes.
const (
	ClassRepo    LocalClass = "repo"
	ClassSecrets LocalClass = "secrets"
	ClassCrons   LocalClass = "crons"
)

var classOrder = []LocalClass{ClassRepo, ClassSecrets, ClassCrons}

// localOp is one daemon write and the change it applies.
type localOp struct {
	change int
	class  LocalClass
	rank   int // within a class: repo register 0 / update 1; crons write 0, disable 1, delete 2
	name   string
	// needsRepo ops take the repo id from a registration this plan creates.
	needsRepo  bool
	register   *pb.RegisterRepoRequest
	updateRepo *pb.UpdateRepoRequest
	secret     bool
	createCron *pb.CreateCronJobRequest
	updateCron *pb.UpdateCronJobRequest
	deleteCron string
	// marker, when set, must come back on the echoed cron row.
	marker string
}

// LocalPlan is a Plan plus the exact writes that apply it. Rendering, JSON
// and Converged use the embedded Plan; the writes are never rendered, so a
// secret value an op carries cannot reach output.
type LocalPlan struct {
	Plan
	ops []localOp
}

// HasWrites reports whether applying the plan writes anything.
func (p LocalPlan) HasWrites() bool { return len(p.ops) > 0 }

// NeedsConfirmation reports whether any change the plan writes sets a cron
// prompt or gate (every cron create, and an update or adoption changing one).
func (p LocalPlan) NeedsConfirmation() bool {
	for _, op := range p.ops {
		if p.Changes[op.change].NeedsConfirmation {
			return true
		}
	}
	return false
}

// WithoutClass drops every write of class c, turning its changes into skips
// with reason ReasonOverridden and detail why.
func (p LocalPlan) WithoutClass(c LocalClass, why string) LocalPlan {
	return p.without(func(op localOp) bool { return op.class == c }, why)
}

// WithoutRepoCreate drops a planned registration and every write that needs
// its id, turning their changes into skips with reason ReasonOverridden.
func (p LocalPlan) WithoutRepoCreate(why string) LocalPlan {
	return p.without(func(op localOp) bool { return op.register != nil || op.needsRepo }, why)
}

func (p LocalPlan) without(drop func(localOp) bool, why string) LocalPlan {
	out := LocalPlan{Plan: Plan{Changes: slices.Clone(p.Changes), Warnings: slices.Clone(p.Warnings)}}
	for _, op := range p.ops {
		if !drop(op) {
			out.ops = append(out.ops, op)
			continue
		}
		c := &out.Changes[op.change]
		c.Action, c.Reason, c.Detail, c.Fields, c.NeedsConfirmation = ActionSkip, ReasonOverridden, why, nil, false
	}
	return out
}

// planner builds one LocalPlan. The remote-cron class reuses it, with remote
// set, to plan one of the caller's other daemons through the same cron rules.
type planner struct {
	cfg    *Config
	st     *LocalState
	opts   LocalOptions
	marker string
	daemon string
	out    LocalPlan
	// remote plans factory crons without this host's gate: another host's
	// crons (the remote-cron class), or this host's under
	// LocalOptions.HostFreeFactory. Factory gates, which embed the host's own
	// paths, are then neither created nor compared.
	remote bool
}

// repoStatus is what the crons and secrets can rely on.
type repoStatus int

const (
	repoUnavailable repoStatus = iota
	repoPending                // registered by this plan
	repoReady
)

// PlanLocal plans converging this daemon onto cfg. It does no I/O: every
// fact comes from st, which ObserveLocal read.
func PlanLocal(cfg *Config, st *LocalState, opts LocalOptions) LocalPlan {
	if st == nil {
		st = &LocalState{}
	}
	marker, _ := Marker(cfg.Project)
	p := &planner{cfg: cfg, st: st, opts: opts, marker: marker, remote: opts.HostFreeFactory}
	t := resolveTarget(cfg, opts)
	p.daemon = opts.Daemon.DisplayName
	if t.entry != nil {
		p.daemon = t.entry.Name
	}
	switch t.verdict {
	case MatchUnmatched:
		p.warn(fmt.Sprintf("this daemon matches no daemons entry, so the repo is applied at %s and no cron runs here; to run crons here add %s to daemons", t.path, p.entryToAdd(t.path)))
	case MatchAmbiguous:
		p.add(Change{
			Resource: ResourceRepo, Identity: "daemons", Daemon: p.daemon, Action: ActionConflict, Reason: ReasonAmbiguous,
			Detail: fmt.Sprintf("several daemons entries match this daemon (%s); crons are skipped until exactly one does", strings.Join(p.matchingEntries(), ", ")),
		})
	case MatchByID, MatchByName:
	}

	status, repoID := p.planRepo(t)
	p.planSecrets(status, repoID)
	p.planCrons(t, status, repoID)

	slices.SortStableFunc(p.out.ops, func(a, b localOp) int {
		if d := slices.Index(classOrder, a.class) - slices.Index(classOrder, b.class); d != 0 {
			return d
		}
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return strings.Compare(a.name, b.name)
	})
	return p.out
}

func (p *planner) add(c Change) int {
	p.out.Changes = append(p.out.Changes, c)
	return len(p.out.Changes) - 1
}

func (p *planner) warn(w string) { p.out.Warnings = append(p.out.Warnings, w) }

func (p *planner) op(op localOp) { p.out.ops = append(p.out.ops, op) }

// entryToAdd renders the daemons entry that would match this daemon.
func (p *planner) entryToAdd(path string) string {
	parts := []string{fmt.Sprintf(`"name": %q`, p.opts.Daemon.DisplayName)}
	if p.opts.Daemon.ID != "" {
		parts = append(parts, fmt.Sprintf(`"daemonId": %q`, p.opts.Daemon.ID))
	}
	if rel := homeRelative(path, p.opts.HomeDir); rel != "" {
		parts = append(parts, fmt.Sprintf(`"path": %q`, rel))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// matchingEntries names the entries an ambiguous match found, for the
// conflict's detail: the entries pinned to this id, else the unpinned ones
// carrying this name (MatchDaemon's two deciding steps).
func (p *planner) matchingEntries() []string {
	id := p.opts.Daemon
	var byID, byName []string
	for _, d := range p.cfg.Daemons {
		switch {
		case id.ID != "" && d.DaemonID != nil && *d.DaemonID == id.ID:
			byID = append(byID, strconv.Quote(d.Name))
		case d.DaemonID == nil && d.Name == id.DisplayName:
			byName = append(byName, strconv.Quote(d.Name))
		}
	}
	if len(byID) > 1 {
		return byID
	}
	return byName
}

// --- Repo -------------------------------------------------------------------

// planRepo resolves the registration (never re-registering an existing one)
// and plans its settings. It returns the repo id when one exists.
func (p *planner) planRepo(t target) (repoStatus, string) {
	origin := vcs.CanonicalRepoURL(p.cfg.Repo.OriginURL)
	change := Change{Resource: ResourceRepo, Identity: origin, Daemon: p.daemon}
	v := p.st.Validation
	switch {
	case p.st.ValidationErr != "" || v == nil || !v.GetIsValid():
		why := p.st.ValidationErr
		if why == "" {
			why = v.GetErrorMessage()
		}
		change.Action, change.Reason = ActionConflict, ReasonRepoPath
		change.Detail = fmt.Sprintf("%s is not a checkout the daemon can register", t.path)
		if why != "" {
			change.Detail += ": " + why
		}
		p.add(change)
		return repoUnavailable, ""
	case vcs.CanonicalRepoURL(v.GetOriginUrl()) != origin:
		change.Action, change.Reason = ActionConflict, ReasonOriginMismatch
		change.Detail = fmt.Sprintf("%s is a clone of %q, not of the file's repo", t.path, v.GetOriginUrl())
		p.add(change)
		return repoUnavailable, ""
	}

	repo := p.st.Registration()
	if repo == nil {
		for _, other := range p.st.Repos {
			if vcs.CanonicalRepoURL(other.GetOriginUrl()) == origin {
				change.Action, change.Reason = ActionConflict, ReasonRegisteredElsewhere
				change.Detail = fmt.Sprintf("already registered at %s; the file places it at %s", other.GetLocalPath(), t.path)
				p.add(change)
				return repoUnavailable, ""
			}
		}
		return repoPending, p.planRepoCreate(change, t, v)
	}

	fields, req := p.repoDiff(repo, false)
	change.Fields = fields
	change.Action = ActionUnchanged
	if len(fields) > 0 {
		change.Action = ActionUpdate
		req.Id = repo.GetId()
		p.op(localOp{change: p.add(change), class: ClassRepo, rank: 1, name: origin, updateRepo: req})
	} else {
		p.add(change)
	}
	if want := p.cfg.Repo.DefaultBaseBranch; want != nil && *want != repo.GetDefaultBaseBranch() {
		p.add(Change{
			Resource: ResourceRepo, Identity: "repo.defaultBaseBranch", Daemon: p.daemon, Action: ActionSkip, Reason: ReasonRegistrationOnly,
			Detail: fmt.Sprintf("the registration's default base branch is %q and can only be set when registering; the file says %q", repo.GetDefaultBaseBranch(), *want),
		})
	}
	return repoReady, repo.GetId()
}

func (p *planner) planRepoCreate(change Change, t target, v *pb.ValidateRepoPathResponse) string {
	r := p.cfg.Repo
	reg := &pb.RegisterRepoRequest{LocalPath: t.path, DisplayName: filepath.Base(t.path), DefaultBaseBranch: v.GetDefaultBranch(), SetupScript: r.SetupScript}
	if r.DisplayName != nil {
		reg.DisplayName = *r.DisplayName
	}
	if r.DefaultBaseBranch != nil {
		reg.DefaultBaseBranch = *r.DefaultBaseBranch
	}
	change.Action = ActionCreate
	change.Fields = append(change.Fields, FieldDiff{Field: "localPath", New: t.path}, FieldDiff{Field: "displayName", New: reg.DisplayName})
	if reg.DefaultBaseBranch != "" {
		change.Fields = append(change.Fields, FieldDiff{Field: "defaultBaseBranch", New: reg.DefaultBaseBranch})
	}
	if reg.SetupScript != nil {
		change.Fields = append(change.Fields, FieldDiff{Field: "setupScript", New: *reg.SetupScript})
	}
	// Settings RegisterRepo cannot carry are set right after registering.
	// Every stated setting is sent: the daemon's registration defaults are not
	// known here, so diffing against them could leave a stated value unset.
	all, req := p.repoDiff(&pb.Repo{}, true)
	req.DisplayName, req.SetupScript = nil, nil
	var settings []FieldDiff
	for _, f := range all {
		if f.Field != "displayName" && f.Field != "setupScript" {
			f.Old = ""
			settings = append(settings, f)
		}
	}
	change.Fields = append(change.Fields, settings...)
	idx := p.add(change)
	p.op(localOp{change: idx, class: ClassRepo, rank: 0, name: change.Identity, register: reg})
	if len(settings) > 0 {
		p.op(localOp{change: idx, class: ClassRepo, rank: 1, name: change.Identity, needsRepo: true, updateRepo: req})
	}
	return ""
}

// repoDiff compares every repo field the file states and the daemon can
// update; absent fields are left alone. all reports every stated field, equal
// or not.
func (p *planner) repoDiff(repo *pb.Repo, all bool) ([]FieldDiff, *pb.UpdateRepoRequest) {
	r := p.cfg.Repo
	req := &pb.UpdateRepoRequest{}
	var fields []FieldDiff
	str := func(field string, want *string, have string, set func(*string)) {
		if want != nil && (all || *want != have) {
			fields = append(fields, FieldDiff{Field: field, Old: have, New: *want})
			set(want)
		}
	}
	boolean := func(field string, want *bool, have bool, set func(*bool)) {
		if want != nil && (all || *want != have) {
			fields = append(fields, FieldDiff{Field: field, Old: strconv.FormatBool(have), New: strconv.FormatBool(*want)})
			set(want)
		}
	}
	str("displayName", r.DisplayName, repo.GetDisplayName(), func(v *string) { req.DisplayName = v })
	str("setupScript", r.SetupScript, repo.GetSetupScript(), func(v *string) { req.SetupScript = v })
	str("sentryOrg", r.SentryOrg, repo.GetSentryOrg(), func(v *string) { req.SentryOrg = v })
	if s := r.Settings; s != nil {
		boolean("settings.canAutoMerge", s.CanAutoMerge, repo.GetCanAutoMerge(), func(v *bool) { req.CanAutoMerge = v })
		boolean("settings.canAutoMergeDependabot", s.CanAutoMergeDependabot, repo.GetCanAutoMergeDependabot(), func(v *bool) { req.CanAutoMergeDependabot = v })
		if !r.Unsupported() {
			str("settings.mergeStrategy", s.MergeStrategy, repo.GetMergeStrategy(), func(v *string) { req.MergeStrategy = v })
		}
		boolean("settings.canAutoRepair", s.CanAutoRepair, repo.GetCanAutoRepair(), func(v *bool) { req.CanAutoRepair = v })
		boolean("settings.shouldArchiveSessionsAfterMerge", s.ShouldArchiveSessionsAfterMerge, repo.GetShouldArchiveSessionsAfterMerge(), func(v *bool) { req.ShouldArchiveSessionsAfterMerge = v })
		boolean("settings.canAutoDeleteBranches", s.CanAutoDeleteBranches, repo.GetCanAutoDeleteBranches(), func(v *bool) { req.CanAutoDeleteBranches = v })
		boolean("settings.shouldKeepBranchesCurrent", s.ShouldKeepBranchesCurrent, repo.GetShouldKeepBranchesCurrent(), func(v *bool) { req.ShouldKeepBranchesCurrent = v })
	}
	if r.Unsupported() && r.Settings != nil && r.Settings.MergeStrategy != nil {
		p.warn(fmt.Sprintf("repo.settings.mergeStrategy: %q is not a value this boss understands; left alone", *r.Settings.MergeStrategy))
	}
	return fields, req
}

// --- Secrets ----------------------------------------------------------------

// planSecrets sets each declared secret whose BOSS_* variable resolves to a
// value differing from the stored one. Values never enter a Change, and a
// secret is never cleared.
func (p *planner) planSecrets(status repoStatus, repoID string) {
	refs := p.cfg.Repo.Secrets
	if refs == nil {
		return
	}
	repo := p.st.Registration()
	secrets := []struct {
		field  string
		ref    *SecretRef
		stored string
		set    func(*pb.UpdateRepoRequest, *pb.SecretUpdate)
	}{
		{"linearApiKey", refs.LinearAPIKey, repo.GetLinearApiKey(), func(r *pb.UpdateRepoRequest, u *pb.SecretUpdate) { r.LinearKey = u }},
		{"sentryAuthToken", refs.SentryAuthToken, repo.GetSentryApiKey(), func(r *pb.UpdateRepoRequest, u *pb.SecretUpdate) { r.SentryKey = u }},
	}
	for _, s := range secrets {
		if s.ref == nil {
			continue
		}
		change := Change{Resource: ResourceRepo, Identity: "secrets." + s.field, Daemon: p.daemon}
		if status == repoUnavailable {
			change.Action, change.Reason = ActionSkip, ReasonRepoUnavailable
			p.add(change)
			continue
		}
		if status == repoPending {
			s.stored = ""
		}
		if p.opts.Secrets == nil {
			change.Action, change.Reason, change.Detail = ActionSkip, ReasonSecretUnresolved, "resolve via the CLI"
			p.add(change)
			continue
		}
		value, ok := ResolveSecret(p.opts.Secrets, *s.ref)
		switch {
		case ok && value == s.stored:
			change.Action = ActionUnchanged
			p.add(change)
		case ok:
			change.Action = ActionUpdate
			change.Detail = "set from $" + s.ref.Env
			change.Fields = []FieldDiff{{Field: s.field, Secret: true}}
			req := &pb.UpdateRepoRequest{Id: repoID}
			s.set(req, &pb.SecretUpdate{Action: pb.SecretAction_SECRET_ACTION_SET, Value: &value})
			p.op(localOp{change: p.add(change), class: ClassSecrets, name: s.field, needsRepo: status == repoPending, secret: true, updateRepo: req})
		case s.stored != "":
			change.Action = ActionUnchanged
			p.add(change)
			p.warn(fmt.Sprintf("secrets.%s: $%s is not set, so the stored value is kept", s.field, s.ref.Env))
		default:
			change.Action, change.Reason, change.Detail = ActionSkip, ReasonSecretUnresolved, "$"+s.ref.Env+" is not set"
			p.add(change)
		}
	}
}
