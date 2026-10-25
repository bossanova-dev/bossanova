package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/recurser/boss/internal/accountflow"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// --- Interactive boss init ------------------------------------------------
//
// On a terminal boss init asks for whatever its flags left open, in the order
// credentials -> Linear mapping -> filter -> config -> crons. The interview
// adds no mechanics of its own: each answer becomes the value of the flag that
// would have answered it, and the writes go through the same functions the flag
// path uses (resolveInitRepo, storeInitCredentials, runInit). That is what keeps
// the prompt and flag paths from drifting apart.

// initIsTerminal reports whether boss init may interview. Like
// StartupIsTerminal it is a package var so tests force either branch.

// initNewPrompter builds the interview's prompter over the command's streams.
// AskSecret masks input when the stream is the real terminal stdin.

// initWatchInterrupt installs the interview's SIGINT handling and returns the
// function that removes it; tests replace it.

// initConfigFilename is the config file the skill-config module loads. runInit
// re-reads the name from the module itself; the interview needs it earlier,
// only to decide whether to ask the merge question.
const ConfigFilename = ".boss-skills.json"

// initInterview is one interview run. mu is held while a step writes (to the
// daemon, Linear or disk) and released while a question waits for an answer,
// so the interrupt handler reports a consistent applied list.
type initInterview struct {
	cmd    *cobra.Command
	ctx    context.Context
	out    io.Writer
	errOut io.Writer
	p      accountflow.Prompter
	getenv func(string) string

	dir     string // as passed to runInit
	repoDir string // absolute
	force   bool
	opts    initOptions

	mu    sync.Mutex
	steps *initAppliedSteps

	client        Client   // nil without a local daemon
	repo          *pb.Repo // nil when the repository is not registered
	storedKey     string   // the Linear key stored before this run
	linearKey     string   // the key this run uses; empty skips Linear
	viewer        linearViewer
	team          linearTeam // set when a listed team was chosen
	skipReason    string     // why registration-dependent steps are skipped
	bridge        *nodeBridge
	bridgeCleanup func()
	group         *

	// runInitInteractive interviews, applies the answers, and prints the summary.
	// Any error, including an answer that never arrives, prints what was already
	// applied before the error is returned (and the command exits non-zero).
	Group
}

func (g *Group) runInitInteractive(c *cobra.Command, dir string, force bool, opts initOptions) error {
	if opts.applied == nil {
		opts.applied = &initAppliedSteps{}
	}
	if opts.overrides.States == nil {
		opts.overrides.States = map[string]string{}
	}
	if opts.overrides.Labels == nil {
		opts.overrides.Labels = map[string]string{}
	}
	repoDir := dir
	if repoDir == "" {
		repoDir, _ = os.Getwd()
	}
	repoDir, absErr := filepath.Abs(repoDir)
	if info, err := os.Stat(repoDir); absErr != nil || err != nil || !info.IsDir() {
		// Nothing to interview about: runInit reports the bad directory exactly
		// as the flag path does.
		opts.linear, opts.mapRoles = g.linearAdminFromEnv(os.Getenv), true
		return g.runInit(c.OutOrStdout(), dir, force, opts)
	}
	ctx := c.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	// The project file is loaded and the branch checked before the interview
	// writes anything.
	if err := g.prepareInitProject(ctx, repoDir, &opts); err != nil {
		return err
	}
	iv := &initInterview{group: g,
		cmd: c, ctx: ctx, out: c.OutOrStdout(), errOut: c.ErrOrStderr(),
		p: g.initNewPrompter(c), getenv: os.Getenv,
		dir: dir, repoDir: repoDir, force: force, opts: opts, steps: opts.applied,
	}
	defer func() {
		if iv.bridgeCleanup != nil {
			iv.bridgeCleanup()
		}
	}()
	if iv.opts.project != nil {
		// Asked from inside the runInit step, which holds mu: release it
		// while the question waits, as every other question does.
		iv.opts.ProjectTrust.Confirm = func(q string) (bool, error) {
			iv.mu.Unlock()
			defer iv.mu.Lock()
			return iv.confirm(q, false)
		}
	}
	stop := g.initWatchInterrupt(iv)
	defer stop()
	if err := iv.run(); err != nil {
		iv.mu.Lock()
		report := iv.steps.stoppedReport()
		iv.mu.Unlock()
		_, _ = io.WriteString(iv.errOut, report)
		return err
	}
	return nil
}

func (iv *initInterview) run() error {
	iv.p.Say("boss init: press Enter to accept the default in brackets. Each question names the flag that answers it in a non-interactive run.")
	for _, ask := range []func() error{
		iv.askRepo, iv.askLinearKey, iv.askSentry, iv.storeCredentials,
		iv.askTeam, iv.askRoles, iv.askFilter, iv.askMerge, iv.askCrons,
	} {
		if err := ask(); err != nil {
			return err
		}
	}
	iv.opts.resolvedRepo = iv.repo
	// Registration and credentials were applied above; runInit must not redo them.
	iv.opts.repo = initRepoOptions{}
	iv.opts.mapRoles = true
	if iv.linearKey != "" {
		iv.opts.linear = iv.group.initNewLinearAdmin(iv.linearKey)
	} else {
		iv.opts.linear = nil
	}
	if err := iv.step(func() error { return iv.group.runInit(iv.out, iv.dir, iv.force, iv.opts) }); err != nil {
		return err
	}
	_, _ = io.WriteString(iv.out, iv.summary())
	return nil
}

// step runs one write with mu held.
func (iv *initInterview) step(fn func() error) error {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	return fn()
}

// --- Prompt helpers -------------------------------------------------------

func promptErr(err error) error {
	if errors.Is(err, io.EOF) {
		return errors.New("input closed before the interview finished")
	}
	return err
}

func (iv *initInterview) ask(question, def string) (string, error) {
	answer, err := iv.p.Ask(question, def)
	if err != nil {
		return "", promptErr(err)
	}
	return strings.TrimSpace(answer), nil
}

func (iv *initInterview) confirm(question string, def bool) (bool, error) {
	answer, err := iv.p.Confirm(question, def)
	if err != nil {
		return false, promptErr(err)
	}
	return answer, nil
}

func (iv *initInterview) askSecret(question string) (string, error) {
	answer, err := iv.p.AskSecret(question)
	if err != nil {
		return "", promptErr(err)
	}
	return strings.TrimSpace(answer), nil
}

func (iv *initInterview) local() bool {
	return remoteURL(iv.cmd) == "" && hostDestination(iv.cmd) == ""
}

// --- 1. Repository --------------------------------------------------------

func (iv *initInterview) askRepo() error {
	o := &iv.opts.repo
	if !iv.local() {
		iv.skipReason = "--remote / --host"
		iv.p.Say("Repository registration, credential storage and cron jobs need a local daemon and path; with --remote / --host they are skipped (config-only init).")
		return nil
	}
	// Credential flags (--linear-key-stdin, --store-env-keys, ...) answer their
	// questions, so read them first, exactly as the flag path does.
	if err := o.readCredentials(iv.cmd.InOrStdin(), iv.getenv); err != nil {
		return err
	}
	main, err := MainCheckout(iv.ctx, iv.repoDir)
	if err != nil {
		if o.requested() {
			return err
		}
		iv.skipReason = "not a git repository"
		iv.p.Say("%s is not a git repository: registration, credential storage and cron jobs are skipped.", iv.repoDir)
		return nil
	}
	c, err := iv.group.newClient(iv.cmd)
	if err != nil {
		if o.requested() {
			return errors.New("could not connect to local bossd")
		}
		iv.skipReason = "local bossd unreachable"
		iv.p.Say("Local bossd is not reachable: registration, credential storage and cron jobs are skipped.")
		return nil
	}
	iv.client = c
	repo, err := FindInitRepo(iv.ctx, c, main)
	if err != nil {
		return err
	}
	if repo != nil {
		iv.repo = repo
		iv.steps.RepoID = repo.GetId()
		iv.p.Say("Repository %s is registered with bossd (id %s).", repoLabel(repo), repo.GetId())
		if note := trustedIgnoredNote(o.trusted, repo); note != "" {
			iv.p.Say("%s", strings.TrimRight(note, "\n"))
		}
		return iv.noteConfigLocation()
	}
	if o.noRegister {
		return errors.New("repository is not registered")
	}
	register := o.register
	if !register {
		if register, err = iv.confirm(fmt.Sprintf("Register %s with bossd? (--register / --no-register)", main), true); err != nil {
			return err
		}
	}
	if !register {
		iv.skipReason = "repository not registered"
		iv.p.Say("Not registered: credential storage and cron jobs are skipped (they need a repository id).")
		return nil
	}
	if err := iv.step(func() error {
		repo, err = resolveInitRepo(iv.ctx, c, iv.repoDir, initRegistration{allow: true, trusted: o.trusted}, iv.steps)
		return err
	}); err != nil {
		return err
	}
	iv.repo = repo
	iv.p.Say("Registered %s with bossd (id %s).", repoLabel(repo), repo.GetId())
	return iv.noteConfigLocation()
}

func (iv *initInterview) noteConfigLocation() error {
	if note := configLocationNote(iv.ctx, iv.repoDir, iv.repo); note != "" {
		iv.p.Say("%s", strings.TrimRight(note, "\n"))
	}
	return nil
}

func repoLabel(r *pb.Repo) string {
	if name := r.GetDisplayName(); name != "" {
		return name
	}
	return r.GetLocalPath()
}

// --- 2. Linear key --------------------------------------------------------

// askLinearKey settles the key: a flag's key, the stored key, LINEAR_API_KEY,
// or one typed in. Every candidate is validated with Viewer before it is used,
// and an empty answer skips every Linear question that follows.
func (iv *initInterview) askLinearKey() error {
	iv.storedKey = iv.repo.GetLinearApiKey()
	if flagged := iv.opts.repo.linearKey; flagged != "" {
		if !iv.acceptLinearKey(flagged) {
			return errors.New("linear key validation failed; stored key unchanged")
		}
		return nil
	}
	type offer struct{ key, question string }
	var offers []offer
	if iv.storedKey != "" {
		offers = append(offers, offer{iv.storedKey, "A Linear API key is already stored for this repository. Reuse it?"})
	}
	if env := strings.TrimSpace(iv.getenv("LINEAR_API_KEY")); env != "" && env != iv.storedKey {
		offers = append(offers, offer{env, "Use LINEAR_API_KEY from the environment? (--store-env-keys)"})
	}
	for _, o := range offers {
		use, err := iv.confirm(o.question, true)
		if err != nil {
			return err
		}
		if use && iv.acceptLinearKey(o.key) {
			return nil
		}
	}
	for {
		key, err := iv.askSecret("Linear API key (input hidden; Enter skips every Linear question; --linear-key-stdin)")
		if err != nil {
			return err
		}
		if key == "" {
			iv.p.Say("No Linear key: the team, state, label and filter questions are skipped.")
			return nil
		}
		if iv.acceptLinearKey(key) {
			return nil
		}
	}
}

// acceptLinearKey validates key with Viewer and, on success, makes it the key
// this run stores and maps with. A failure is reported by class only.
func (iv *initInterview) acceptLinearKey(key string) bool {
	admin := iv.group.initNewLinearAdmin(key)
	if admin == nil {
		return false
	}
	viewer, err := admin.Viewer(iv.ctx)
	if err != nil {
		iv.p.Say("That Linear key could not be validated (%s).", linearErrorClass(err, "request failed"))
		return false
	}
	iv.linearKey, iv.viewer = key, viewer
	iv.p.Say("Linear key belongs to %s in workspace %q.", viewerDisplay(viewer), viewer.Organization)
	return true
}

// validateLinearKey is the initOptions.validateLinearKey seam wired to the
// Linear client: a key is stored only once Viewer accepts it.
func (g *Group) validateLinearKey(ctx context.Context, key string) (string, error) {
	admin := g.initNewLinearAdmin(key)
	if admin == nil {
		return "", errors.New("no Linear key")
	}
	viewer, err := admin.Viewer(ctx)
	if err != nil {
		return "", err
	}
	return viewerDisplay(viewer), nil
}

// --- 3. Sentry ------------------------------------------------------------

// askSentry asks for the Sentry token and organization as a pair. It needs a
// registration to store them on.
func (iv *initInterview) askSentry() error {
	o := &iv.opts.repo
	if iv.repo == nil || o.sentryToken != "" {
		return nil
	}
	if stored := iv.repo.GetSentryApiKey(); stored != "" {
		keep, err := iv.confirm(fmt.Sprintf("Sentry credentials are stored for this repository (org %q). Keep them?", iv.repo.GetSentryOrg()), true)
		if err != nil || keep {
			return err
		}
	}
	var token string
	if env := strings.TrimSpace(iv.getenv("SENTRY_AUTH_TOKEN")); env != "" {
		use, err := iv.confirm("Use SENTRY_AUTH_TOKEN from the environment? (--store-env-keys)", true)
		if err != nil {
			return err
		}
		if use {
			token = env
		}
	}
	if token == "" {
		var err error
		if token, err = iv.askSecret("Sentry auth token (input hidden; Enter skips Sentry; --sentry-token-stdin)"); err != nil {
			return err
		}
		if token == "" {
			return nil
		}
	}
	org := o.sentryOrg
	for org == "" {
		var err error
		if org, err = iv.ask("Sentry organization slug (--sentry-org)", iv.repo.GetSentryOrg()); err != nil {
			return err
		}
	}
	o.sentryToken, o.sentryOrg = token, org
	return nil
}

// storeCredentials writes the Linear key and Sentry pair in one update, the
// same single UpdateRepo the flag path makes, with read-back.
func (iv *initInterview) storeCredentials() error {
	if iv.repo == nil {
		return nil
	}
	o := iv.opts.repo
	o.linearKey = iv.linearKey
	iv.opts.validateLinearKey = iv.group.validateLinearKey
	return iv.step(func() error {
		return storeInitCredentials(iv.ctx, iv.client, iv.repo, o, iv.opts.validateLinearKey, iv.steps, iv.out)
	})
}

// --- 4. Team and role mapping --------------------------------------------

// askTeam confirms the Linear team: a single visible team is pre-selected,
// several are listed to choose from, and --team answers the question.
func (iv *initInterview) askTeam() error {
	if iv.linearKey == "" {
		return nil
	}
	admin := iv.group.initNewLinearAdmin(iv.linearKey)
	teams, hasNext, err := admin.ListTeams(iv.ctx)
	if err != nil {
		iv.p.Say("Linear teams could not be listed (%s); no team is pinned.", linearErrorClass(err, "listing failed"))
		return nil
	}
	if flag := strings.TrimSpace(iv.opts.team); flag != "" {
		// --team answers the question; it still has to name a listed team for
		// the role questions to have states and labels to offer.
		t, ok, err := iv.resolveTeam(flag, &teamListing{teams: teams, hasNextPage: hasNext})
		if err != nil {
			return err
		}
		if ok {
			iv.team = t
		}
		return nil
	}
	if len(teams) == 0 {
		iv.p.Say("No Linear teams are visible to this key; no team is pinned.")
		return nil
	}
	def := ""
	if len(teams) == 1 && !hasNext {
		def = teams[0].Name
	} else {
		iv.p.Say("Linear teams visible to this key:")
		for i, t := range teams {
			iv.p.Say("  %d. %s", i+1, teamDisplay(t))
		}
		if hasNext {
			iv.p.Say("  (more teams exist; only the ones listed can be verified)")
		}
	}
	for {
		answer, err := iv.ask("Linear team (--team)", def)
		if err != nil {
			return err
		}
		t, ok, err := iv.matchTeam(&teamListing{teams: teams, hasNextPage: hasNext}, answer)
		if err != nil {
			return err
		}
		if ok {
			iv.team = t
			// Accepting the single pre-selected team is what the flag path
			// detects on its own, so it leaves --team unset.
			if def == "" || t.Name != def {
				iv.opts.team = t.Name
			}
			return nil
		}
		iv.p.Say("%q is not one of the listed teams.", answer)
	}
}

func (iv *initInterview) resolveTeam(answer string, listing *teamListing) (linearTeam, bool, error) {
	if iv.bridge == nil {
		bridge, cleanup, err := newNodeBridge()
		if err != nil {
			return linearTeam{}, false, err
		}
		iv.bridge, iv.bridgeCleanup = bridge, cleanup
	}
	_, team, verified, err := resolveListedTeam(iv.bridge, answer, listing)
	return team, verified, err
}

// matchTeam handles list indices locally; JS resolves every named answer.
func (iv *initInterview) matchTeam(listing *teamListing, answer string) (linearTeam, bool, error) {
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(listing.teams) {
		return listing.teams[n-1], true, nil
	}
	if strings.TrimSpace(answer) == "" {
		return linearTeam{}, false, nil
	}
	team, verified, err := iv.resolveTeam(answer, listing)
	return team, verified, err
}

// askRoles asks for each state role and each label role, with the matched
// candidate as the default, then offers to create the labels still missing.
// Only an answer that differs from its default becomes an override, exactly as
// if that --state/--label had been passed.
func (iv *initInterview) askRoles() error {
	if iv.team.ID == "" {
		return nil
	}
	admin := iv.group.initNewLinearAdmin(iv.linearKey)
	// Seed from the existing config only when runInit will too: --force
	// without --merge replaces the file, so its mappings are not the defaults
	// runInit resolves, and an answer equal to them would be dropped.
	seeded := iv.opts.overrides
	if !iv.force || iv.opts.mergeExisting {
		if existing, err := os.ReadFile(filepath.Join(iv.repoDir, ConfigFilename)); err == nil {
			seeded = seedOverridesFromExisting(existing, seeded)
		}
	}
	states, err := admin.ListStates(iv.ctx, iv.team.ID)
	if err != nil {
		return fmt.Errorf("%w; nothing written", err)
	}
	iv.p.Say("Workflow states of %s: %s", teamDisplay(iv.team), stateNames(states))
	chosen := map[string]linearState{}
	for _, role := range linearStateRoles {
		if name, flagged := iv.opts.overrides.States[role]; flagged {
			if st, ok := pickState(states, name); ok {
				chosen[role] = st
			}
			continue
		}
		want := seeded.States[role]
		if want == "" {
			want = linearStateDefaults[role]
		}
		def := ""
		if st, ok := pickState(states, want); ok {
			def = st.Name
		}
		for {
			answer, err := iv.ask(fmt.Sprintf("State for %s (--state %s=<name>)", role, role), def)
			if err != nil {
				return err
			}
			st, ok := pickState(states, answer)
			if !ok {
				iv.p.Say("%q is not a workflow state of %s.", answer, teamDisplay(iv.team))
				continue
			}
			if role == "planned" && sameState(st, chosen["unplanned"]) {
				iv.p.Say("planned cannot be the unplanned state (%q): boss-build would pick up unplanned work.", st.Name)
				continue
			}
			chosen[role] = st
			if st.Name != def {
				iv.opts.overrides.States[role] = st.Name
			}
			break
		}
	}

	labels, err := admin.FindLabels(iv.ctx, iv.team.ID, linearLabelLookupNames(seeded))
	if err != nil {
		return fmt.Errorf("%w; nothing written", err)
	}
	for _, role := range linearLabelRoles {
		if _, flagged := iv.opts.overrides.Labels[role]; flagged {
			continue
		}
		def := seeded.Labels[role]
		if def == "" {
			def = linearLabelDefaults[role]
		}
		if l, ok := pickLabel(labels, def); ok {
			def = l.Name
		}
		answer, err := iv.ask(fmt.Sprintf("Label for %s (--label %s=<name>)", role, role), def)
		if err != nil {
			return err
		}
		if answer != "" && answer != def {
			iv.opts.overrides.Labels[role] = answer
		}
	}
	return iv.offerLabelCreation(admin, states, seeded)
}

// offerLabelCreation maps the answers the way runInit will and, when labels are
// still missing, asks to create them, naming the target workspace and team.
func (iv *initInterview) offerLabelCreation(admin linearAdmin, states []linearState, seeded linearOverrides) error {
	if iv.opts.createLabels {
		return nil
	}
	final := overlayOverrides(seeded, iv.opts.overrides)
	mapping, err := planLinearRoles(iv.ctx, admin, iv.team, states, final)
	if err != nil {
		return fmt.Errorf("%w; nothing written", err)
	}
	if len(mapping.Missing) == 0 {
		return nil
	}
	names := make([]string, 0, len(mapping.Missing))
	for _, m := range mapping.Missing {
		names = append(names, m.Name)
	}
	create, err := iv.confirm(fmt.Sprintf("Create the missing labels %s in workspace %q, team %s? (--create-labels)",
		strings.Join(names, ", "), iv.viewer.Organization, teamDisplay(iv.team)), true)
	if err != nil {
		return err
	}
	iv.opts.createLabels = create
	return nil
}

// --- 5. Filter ------------------------------------------------------------

// askFilter shows who `me` resolves to (warning for an app/bot key) and offers
// the "only my tickets" filter, committed to .boss-skills.json.
func (iv *initInterview) askFilter() error {
	if iv.linearKey == "" || iv.opts.overrides.AssigneeMe {
		return nil
	}
	iv.p.Say("\"Only my tickets\" resolves `me` to the Linear user %s.", viewerDisplay(iv.viewer))
	if iv.viewer.App {
		iv.p.Say("Warning: this Linear key belongs to an app/bot user, so `me` is that bot, not you.")
	}
	me, err := iv.confirm("Only pick up tickets assigned to you? Writes selection.assignees.include: [\"me\"] to .boss-skills.json (--assignee-me)", false)
	if err != nil {
		return err
	}
	iv.opts.overrides.AssigneeMe = me
	return nil
}

// --- 6. Config ------------------------------------------------------------

// askMerge confirms merging into an existing config. Declining stops the
// interview here with today's refusal, before any cron question.
func (iv *initInterview) askMerge() error {
	if iv.force || iv.opts.mergeExisting {
		return nil
	}
	target := filepath.Join(iv.repoDir, ConfigFilename)
	if _, err := os.Lstat(target); err != nil {
		return nil
	}
	merge, err := iv.confirm(fmt.Sprintf("%s already exists. Merge the detected config and these answers into it? (--merge; no leaves it untouched and stops)", target), true)
	if err != nil {
		return err
	}
	if !merge {
		return fmt.Errorf("%s already exists; left untouched", target)
	}
	iv.opts.mergeExisting = true
	return nil
}

// --- 7. Crons -------------------------------------------------------------

// askCrons asks, per factory stage, whether to create its job, offering only
// agents the daemon can run, and asks before updating an existing job.
func (iv *initInterview) askCrons() error {
	cron := &iv.opts.cron
	if iv.repo == nil || iv.client == nil || cron.requested() {
		return nil
	}
	if projectSelectsCronsHere(iv.opts.project) {
		iv.p.Say("Cron jobs for this daemon come from .boss-project.json; the pipeline cron questions are skipped.")
		return nil
	}
	eligibility, err := initCronAgentEligibility(iv.ctx, iv.client, iv.group.NewInitCronEnv().ServicePath())
	if err != nil {
		iv.p.Say("Cron setup skipped: %s.", err)
		return nil
	}
	var eligible []string
	for _, e := range eligibility {
		if e.Eligible() {
			eligible = append(eligible, e.Name)
		} else {
			iv.p.Say("%s cannot run pipeline cron jobs: %s.", e.Name, e.reason())
		}
	}
	if len(eligible) == 0 {
		iv.p.Say("Cron setup skipped: no agent can run pipeline cron jobs.")
		return nil
	}
	if cron.agent == "" {
		if len(eligible) == 1 {
			iv.p.Say("Pipeline cron jobs run under %s, the only eligible agent.", eligible[0])
		} else {
			for {
				answer, err := iv.ask(fmt.Sprintf("Agent for the pipeline cron jobs: %s (--agent)", strings.Join(eligible, " or ")), eligible[0])
				if err != nil {
					return err
				}
				answer = strings.ToLower(answer)
				if slices.Contains(eligible, answer) {
					// The first eligible agent is what --cron picks on its own.
					if answer != eligible[0] {
						cron.agent = answer
					}
					break
				}
				iv.p.Say("%q is not an eligible agent.", answer)
			}
		}
	}
	jobs, err := iv.client.ListCronJobs(iv.ctx, iv.repo.GetId())
	if err != nil {
		return errors.New("could not list the repository's cron jobs")
	}
	var stages, existing []string
	for _, st := range factoryCronStages {
		create, err := iv.confirm(fmt.Sprintf("Create the %s cron job (%s, every run gated)? (--cron %s)", st.name, st.schedule, st.stage), true)
		if err != nil {
			return err
		}
		if !create {
			continue
		}
		stages = append(stages, st.stage)
		if job, _ := findStageJob(jobs, st); job != nil {
			existing = append(existing, job.GetName())
		}
	}
	if len(existing) > 0 && !cron.updateConsent {
		update, err := iv.confirm(fmt.Sprintf("%s already exist(s). Update prompt, gate and agent where they differ from the standard definition? Schedule, timezone and enabled state never change. (--update-crons)",
			strings.Join(existing, ", ")), false)
		if err != nil {
			return err
		}
		cron.updateConsent = update
	}
	cron.stages = stages
	return cron.normalize()
}

// --- Summary --------------------------------------------------------------

// summary is printed after a successful run. The cron report, skew warnings and
// harness MCP declarations are already above it; it gathers what was done and
// what is left.
func (iv *initInterview) summary() string {
	s := iv.steps
	var b strings.Builder
	b.WriteString("\nSummary\n")
	line := func(label, value string) { fmt.Fprintf(&b, "  %-16s %s\n", label+":", value) }

	switch {
	case iv.repo == nil:
		line("Repository", "not registered ("+iv.skipReason+")")
	case s.Registered:
		line("Repository", s.registrationText()+"; "+repoLabel(iv.repo)+", registered now")
	default:
		line("Repository", repoLabel(iv.repo)+" (id "+iv.repo.GetId()+"), already registered")
	}

	switch {
	case s.LinearStored:
		text := s.linearText()
		if s.LinearVerified {
			text += "; validated (" + viewerDisplay(iv.viewer) + ")"
		}
		line("Linear key", text)
	case iv.linearKey == "":
		line("Linear key", "skipped")
	case iv.repo != nil && iv.linearKey == iv.storedKey:
		line("Linear key", "already stored, validated ("+viewerDisplay(iv.viewer)+")")
	default:
		line("Linear key", "validated, used for this run only (not stored)")
	}

	switch {
	case s.SentryStored:
		line("Sentry", s.sentryText()+" (org "+iv.opts.repo.sentryOrg+")")
	case iv.repo.GetSentryApiKey() != "":
		line("Sentry", "already stored (org "+iv.repo.GetSentryOrg()+")")
	default:
		line("Sentry", "skipped")
	}

	line("Overrides", overridesSummary(s.Overrides))
	if len(s.LabelsCreated) > 0 {
		line("Labels created", s.labelsText())
	} else {
		line("Labels created", "none")
	}
	if s.ConfigPath != "" {
		line("Config", s.configText())
	} else {
		line("Config", "unchanged")
	}

	if len(s.Crons) == 0 {
		line("Cron jobs", "none created or updated")
	} else {
		next := iv.nextFireTimes()
		b.WriteString("  Cron jobs:\n")
		for _, c := range s.Crons {
			when := "next fire time not reported by the daemon"
			if t, ok := next[c.JobID]; ok {
				when = "next fire " + t
			}
			fmt.Fprintf(&b, "    %s; %s\n", c.appliedText(), when)
		}
	}
	if len(s.Project) > 0 {
		b.WriteString("  Project config:\n")
		for _, change := range s.Project {
			fmt.Fprintf(&b, "    - %s\n", projectChangeText(change))
		}
	}
	if len(s.CronWarnings) > 0 {
		b.WriteString("  Warnings:\n")
		for _, w := range s.CronWarnings {
			writeSummaryItem(&b, w)
		}
	}

	b.WriteString("  Left to do:\n")
	left := []string{}
	if s.ConfigPath != "" {
		left = append(left, "commit "+filepath.Base(s.ConfigPath)+" and merge it to the base branch: cron sessions run in fresh worktrees off it")
	}
	left = append(left, "declare the tracker MCP server to each coding-agent harness you use (declarations printed above; boss init never writes those files)")
	left = append(left, s.CronLeftToDo...)
	for _, item := range left {
		writeSummaryItem(&b, item)
	}
	return b.String()
}

// writeSummaryItem writes one wrapped bullet nested under a summary heading.
func writeSummaryItem(b *strings.Builder, item string) {
	for i, line := range wrapText(item) {
		prefix := "    - "
		if i > 0 {
			prefix = "      "
		}
		b.WriteString(prefix + line + "\n")
	}
}

// nextFireTimes reads each cron job's next run time back from the daemon.
func (iv *initInterview) nextFireTimes() map[string]string {
	out := map[string]string{}
	if iv.client == nil || iv.repo == nil {
		return out
	}
	jobs, err := iv.client.ListCronJobs(iv.ctx, iv.repo.GetId())
	if err != nil {
		return out
	}
	for _, j := range jobs {
		if j.GetNextRunAt() != nil {
			out[j.GetId()] = j.GetNextRunAt().AsTime().Local().Format("2006-01-02 15:04 MST")
		}
	}
	return out
}

func overridesSummary(ov linearOverrides) string {
	var parts []string
	if ov.Team != "" {
		parts = append(parts, "team "+ov.Team)
	}
	for _, role := range sortedOverrideKeys(ov.States) {
		parts = append(parts, fmt.Sprintf("states.%s=%q", role, ov.States[role]))
	}
	for _, role := range sortedOverrideKeys(ov.Labels) {
		parts = append(parts, fmt.Sprintf("labels.%s=%q", role, ov.Labels[role]))
	}
	if ov.AssigneeMe {
		parts = append(parts, "assignee filter: me")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// --- Interrupts -----------------------------------------------------------

// watchInitInterrupt routes SIGINT during the interview to handleInitInterrupt.
// The terminal state is captured first so a Ctrl-C inside a masked read (echo
// off) cannot leave the terminal without echo.
func watchInitInterrupt(iv *initInterview) func() {
	restore := func() {}
	fd := int(os.Stdin.Fd())
	if state, err := term.GetState(fd); err == nil {
		restore = func() { _ = term.Restore(fd, state) }
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt)
	quit := make(chan struct{})
	done := safego.Go(log.Logger, func() {
		handleInitInterrupt(sigs, quit, iv, restore, os.Exit)
	})
	return func() {
		signal.Stop(sigs)
		close(quit)
		<-done
	}
}

// handleInitInterrupt waits for SIGINT (or quit). On the first signal it
// restores the terminal, waits for a running write to finish so the applied
// list is accurate, prints it, and exits 130. A second signal while a write is
// still running exits at once, saying that write's outcome is unknown.
func handleInitInterrupt(sigs <-chan os.Signal, quit <-chan struct{}, iv *initInterview, restore func(), exit func(int)) {
	select {
	case <-quit:
		return
	case <-sigs:
	}
	restore()
	_, _ = io.WriteString(iv.errOut, "\n")
	locked := safego.Go(log.Logger, func() { iv.mu.Lock() })
	select {
	case <-locked:
		_, _ = io.WriteString(iv.errOut, iv.steps.stoppedReport())
		iv.mu.Unlock()
	case <-sigs:
		_, _ = io.WriteString(iv.errOut, "boss init interrupted again while a step was still running; that step's outcome is unknown.\nRe-run `boss init` to finish.\n")
	}
	// exit skips main's deferred teardown, so close a --host tunnel here or
	// its ssh child and socket directory outlive the process.
	iv.group.Config.ShutdownHostTunnel()
	exit(130)
}
