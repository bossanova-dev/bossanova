package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/recurser/boss/api/cli/skills"
	"github.com/recurser/boss/internal/accountflow"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/projectconfig"
)

// `boss project diff` and `boss project apply` converge this daemon onto the
// committed .boss-project.json: the repo registration and settings, its
// BOSS_* secrets, and the crons the file places on this daemon. The engine is
// projectconfig.ObserveLocal / PlanLocal / ApplyLocal; this file adds the file
// lookup, this daemon's identity and the trust gate. --cloud adds a second
// leg (project_cloud.go): the caller's cloud triggers, the organization's
// session webhooks and the crons the file places on the caller's other daemons
// join the same plan, trust gate and report, and are written after the local
// leg.

// projectExitError carries a specific process exit status. An empty message
// exits without printing anything.
type projectExitError struct {
	code int
	err  error
}

func (e *projectExitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *projectExitError) Unwrap() error { return e.err }

// errNotGitCheckout is loadProjectFile's failure outside a git checkout.
var errNotGitCheckout = errors.New("not inside a git checkout")

// errProjectFileMissing is the failure for a checkout with no project file.
var errProjectFileMissing = errors.New("no " + projectconfig.FileName + " at the checkout's top level; write one with `boss project export --write`")

// projectApplyEnv is every host dependency of diff and apply. Tests replace
// it whole through newProjectApplyEnv.
type projectApplyEnv struct {
	identity   func(projectExportEnv) (projectconfig.DaemonIdentity, string)
	export     projectExportEnv
	isTerminal func() bool
	// confirm asks one yes/no question on the command's streams.
	confirm func(cmd *cobra.Command, question string) (bool, error)
	getenv  func(string) string
	// branch is the checkout's current branch; empty when detached or unknown.
	branch func(ctx context.Context, dir string) string
	// originHead is the remote's default branch when nothing else names one.
	originHead func(ctx context.Context, dir string) string
	cronEnv    func() skills.CronEnv
	// cloudApply dials bosso for --cloud; a missing login fails before any
	// request is sent.
	cloudApply func(cmd *cobra.Command) (projectCloudApplyClient, error)
}

func (g *Group) defaultProjectApplyEnv() projectApplyEnv {
	return projectApplyEnv{
		identity:   localApplyIdentity,
		export:     g.newProjectExportEnv(),
		isTerminal: func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		confirm: func(cmd *cobra.Command, question string) (bool, error) {
			return accountflow.NewIOPrompter(cmd.InOrStdin(), cmd.OutOrStdout()).Confirm(question, false)
		},
		getenv: os.Getenv,
		branch: gitCurrentBranch,
		originHead: func(ctx context.Context, dir string) string {
			return skills.DefaultCronBaseBranch(ctx, dir, &pb.Repo{})
		},
		cronEnv:    skills.DefaultCronEnv,
		cloudApply: g.defaultProjectCloudApply,
	}
}

func gitCurrentBranch(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "--short", "-q", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (g *Group) projectDiffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show what applying .boss-project.json would change on this daemon",
		Long: "Show what applying .boss-project.json would change on this daemon. Read-only.\n\n" +
			"With --cloud the plan also covers your cloud triggers, the organization's session\n" +
			"webhooks and the crons the file places on your other daemons (needs boss login and a\n" +
			"paid plan).\n\n" +
			"With --exit-code the command exits 1 when the daemon has not converged on the file,\n" +
			"2 on an error, and 0 otherwise.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProjectDiff(cmd, g.newProjectApplyEnv())
		},
	}
	cmd.Flags().String("dir", ".", "Checkout whose top-level "+projectconfig.FileName+" is read")
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Print the plan as JSON")
	cmd.Flags().Bool("exit-code", false, "Exit 1 when the plan is not converged, 2 on error")
	cmd.Flags().Bool("cloud", false, "Also plan your cloud triggers and the organization's session webhooks (needs boss login)")
	cmd.Flags().String("org", "", "Organization id for --cloud (default: cloud.organization, else the repository's organization, else your only one)")
	return cmd
}

func (g *Group) projectApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Converge this daemon onto .boss-project.json",
		Long: "Converge this daemon onto .boss-project.json: register the repo (never re-registering\n" +
			"one), update its settings, set its BOSS_* secrets from the environment (never cleared or\n" +
			"printed), and create, update or adopt the crons the file places on this daemon, marking\n" +
			"them managed by the project. A managed cron the file dropped is disabled, or deleted with\n" +
			"--prune; crons the file never managed are left alone.\n\n" +
			"With --cloud the same plan also converges your own cloud triggers and, for an organization\n" +
			"owner, the organization's session webhooks, marking them managed by the project. It also\n" +
			"converges the crons the file places on your other daemons through the cloud; another\n" +
			"member's daemon, an offline daemon and a daemon too old to store the marker are skipped and\n" +
			"reported. Nothing is written on any leg unless the paid-plan check passes first. A signing\n" +
			"secret bosso mints is printed once to stderr and never to stdout or --json; --rotate-secrets\n" +
			"rotates the secrets of the project's HTTP triggers and webhooks.\n\n" +
			"Before any write the checkout must be on the repo's default branch (or pass\n" +
			"--allow-branch). A plan that sets a cron prompt or gate, creates a trigger or webhook,\n" +
			"changes a trigger's prompt or gate, rotates a secret, or grants repo trust is confirmed on a terminal and\n" +
			"needs --yes elsewhere.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProjectApply(cmd, g.newProjectApplyEnv())
		},
	}
	cmd.Flags().String("dir", ".", "Checkout whose top-level "+projectconfig.FileName+" is applied")
	cmd.Flags().Bool("dry-run", false, "Print the plan and write nothing")
	cmd.Flags().Bool("prune", false, "Delete managed crons (and, with --cloud, triggers and webhooks) the file dropped instead of disabling them")
	cmd.Flags().Bool("yes", false, "Apply changes that need confirmation without asking")
	cmd.Flags().Bool("cloud", false, "Also converge your cloud triggers and the organization's session webhooks (needs boss login)")
	cmd.Flags().String("org", "", "Organization id for --cloud (default: cloud.organization, else the repository's organization, else your only one)")
	cmd.Flags().Bool("rotate-secrets", false, "With --cloud, rotate the signing secrets of the project's HTTP triggers and webhooks")
	cmd.Flags().Bool("allow-branch", false, "Apply from a checkout that is not on the repo's default branch")
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Print the plan and the apply result as JSON")
	return cmd
}

// loadedProject is a decoded, valid project file.
type loadedProject struct {
	cfg      *projectconfig.Config
	path     string
	checkout string // the top level of the checkout holding the file
	warnings []string
}

// loadProjectFile reads <top level of dir>/.boss-project.json. found is
// false when the checkout has no such file.
func loadProjectFile(ctx context.Context, dir string) (proj *loadedProject, found bool, err error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", dir, errNotGitCheckout)
	}
	top := strings.TrimSpace(string(out))
	path := filepath.Join(top, projectconfig.FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", path, err)
	}
	cfg, warnings, err := projectconfig.Decode(data)
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	if err := projectconfig.Validate(cfg); err != nil {
		return nil, true, fmt.Errorf("%s is not a valid project config:\n%w", path, err)
	}
	return &loadedProject{cfg: cfg, path: path, checkout: top, warnings: warnings}, true, nil
}

// projectRun is one planned run against the local daemon, plus the cloud
// leg when --cloud asked for one.
type projectRun struct {
	proj  *loadedProject
	c     Client
	state *projectconfig.LocalState
	plan  projectconfig.LocalPlan
	cloud *projectCloudRun
}

// hasWrites reports whether either leg writes anything.
func (r *projectRun) hasWrites() bool {
	return r.plan.HasWrites() || (r.cloud != nil && r.cloud.plan.HasWrites())
}

// needsConfirmation reports whether either leg writes a sensitive change.
func (r *projectRun) needsConfirmation() bool {
	return r.plan.NeedsConfirmation() || (r.cloud != nil && r.cloud.plan.NeedsConfirmation())
}

// renderedPlan is the plan both legs render as one.
func (r *projectRun) renderedPlan() projectconfig.Plan {
	return combinedPlan(r.plan, r.cloud)
}

// planLocalProject observes this daemon and plans the file against it.
func planLocalProject(ctx context.Context, cmd *cobra.Command, env projectApplyEnv, proj *loadedProject, prune bool) (*projectRun, error) {
	c, err := env.export.newClient(cmd)
	if err != nil {
		return nil, errors.New("could not connect to local bossd")
	}
	opts, main, warnings, err := localProjectOptions(ctx, env, proj, prune)
	if err != nil {
		return nil, err
	}

	var factory projectconfig.FactoryResolver
	if hasFactoryCron(proj.cfg) {
		r, err := skills.NewFactoryCronResolver(ctx, c, filepath.Join(main, skills.ConfigFilename), env.cronEnv())
		if err != nil {
			warnings = append(warnings, "pipeline cron jobs cannot be resolved: "+err.Error())
		} else {
			factory = r
		}
	}

	state, err := projectconfig.ObserveLocal(ctx, proj.cfg, c, factory, opts)
	if err != nil {
		return nil, err
	}
	plan := projectconfig.PlanLocal(proj.cfg, state, opts)
	plan.Warnings = append(warnings, plan.Warnings...)
	return &projectRun{proj: proj, c: c, state: state, plan: plan}, nil
}

// localProjectOptions gives planning and the pre-write branch check the same
// daemon identity and target-checkout resolution.
func localProjectOptions(ctx context.Context, env projectApplyEnv, proj *loadedProject, prune bool) (projectconfig.LocalOptions, string, []string, error) {
	main, err := skills.MainCheckout(ctx, proj.checkout)
	if err != nil {
		return projectconfig.LocalOptions{}, "", nil, err
	}
	identity, identityWarning := env.identity(env.export)
	home, _ := env.export.homeDir()
	warnings := append([]string(nil), proj.warnings...)
	if identityWarning != "" {
		warnings = append(warnings, identityWarning)
	}

	opts := projectconfig.LocalOptions{
		Daemon:       identity,
		CheckoutPath: main,
		HomeDir:      home,
		Prune:        prune,
		Secrets:      projectconfig.EnvResolver(env.getenv),
		MainCheckout: skills.MainCheckout,
	}
	return opts, main, warnings, nil
}

// observeProjectBranchState reads only daemon state: factory resolution needs
// the skills config that init has not written yet. Failures retain the
// pre-check's file/origin-HEAD fallback.
func observeProjectBranchState(ctx context.Context, cmd *cobra.Command, env projectApplyEnv, proj *loadedProject) *projectconfig.LocalState {
	c, err := env.export.newClient(cmd)
	if err != nil {
		return nil
	}
	opts, _, _, err := localProjectOptions(ctx, env, proj, false)
	if err != nil {
		return nil
	}
	state, err := projectconfig.ObserveLocal(ctx, proj.cfg, c, nil, opts)
	if err != nil {
		return nil
	}
	return state
}

func hasFactoryCron(cfg *projectconfig.Config) bool {
	for _, c := range cfg.Crons {
		if c.IsFactory() {
			return true
		}
	}
	return false
}

// projectTrust is the write gate: the checkout must be on the repo's default
// branch unless allowBranch, and a plan setting a cron prompt or gate needs a
// terminal confirmation or yes. It returns nil when the run may write; a
// plan with nothing to write always may.
type projectTrust struct {
	yes, allowBranch bool
	// confirm overrides env.confirm (boss init asks through its interview).
	confirm func(question string) (bool, error)
}

// onDefaultBranch reports whether the checkout is on the repo's default
// branch. An unknown branch on either side (detached HEAD, no origin HEAD)
// counts as off it, so two empty names never pass the trust gate.
func onDefaultBranch(have, want string) bool {
	return have != "" && want != "" && have == want
}

// projectDefaultBranch resolves the trust gate's default branch from the
// file, registration, daemon detection, then the checkout's origin HEAD.
// A nil state retains the file and origin HEAD fallback.
func projectDefaultBranch(ctx context.Context, env projectApplyEnv, proj *loadedProject, state *projectconfig.LocalState) string {
	if want := state.DefaultBranch(proj.cfg); want != "" {
		return want
	}
	return env.originHead(ctx, proj.checkout)
}

func projectBranchCheck(ctx context.Context, env projectApplyEnv, proj *loadedProject, state *projectconfig.LocalState) (have, want string, ok bool) {
	have = env.branch(ctx, proj.checkout)
	want = projectDefaultBranch(ctx, env, proj, state)
	return have, want, onDefaultBranch(have, want)
}

// errProjectNotConfirmed is a declined or unconfirmable plan.
var errProjectNotConfirmed = skills.ErrProjectNotConfirmed

func checkProjectTrust(ctx context.Context, cmd *cobra.Command, env projectApplyEnv, run *projectRun, trust projectTrust) error {
	if !run.hasWrites() {
		return nil
	}
	have, want, onDefault := projectBranchCheck(ctx, env, run.proj, run.state)
	if !onDefault && !trust.allowBranch {
		where := fmt.Sprintf("on branch %q", have)
		if have == "" {
			where = "not on a branch"
		}
		return fmt.Errorf("refusing to apply %s: the checkout is %s, not the repo's default branch %q; nothing was written (re-run with --allow-branch to apply it anyway)", run.proj.path, where, want)
	}
	if !run.needsConfirmation() || trust.yes {
		return nil
	}
	ask := trust.confirm
	if ask == nil && env.isTerminal() {
		ask = func(q string) (bool, error) { return env.confirm(cmd, q) }
	}
	if ask == nil {
		return fmt.Errorf("%w: the plan sets a cron prompt or gate, a trigger or webhook, a secret rotation, or a repo trust grant, which needs confirmation; nothing was written (re-run with --yes to apply it)", errProjectNotConfirmed)
	}
	ok, err := ask("Apply these changes, including the prompts, gates, triggers, webhooks, secrets and repo trust above?")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: declined; nothing was written", errProjectNotConfirmed)
	}
	return nil
}

func rejectProjectTransport(cmd *cobra.Command, verb string) error {
	if remoteURL(cmd) != "" || hostDestination(cmd) != "" {
		return fmt.Errorf("boss project %s converges the local daemon onto a local checkout; --remote and --host are not supported", verb)
	}
	return nil
}

func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func runProjectDiff(cmd *cobra.Command, env projectApplyEnv) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	exitCode, _ := cmd.Flags().GetBool("exit-code")
	fail := func(err error) error {
		if exitCode {
			err = &projectExitError{code: 2, err: err}
		}
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	if err := rejectProjectTransport(cmd, "diff"); err != nil {
		return fail(err)
	}
	ctx := commandContext(cmd)
	dir, _ := cmd.Flags().GetString("dir")
	proj, found, err := loadProjectFile(ctx, dir)
	if err == nil && !found {
		err = errProjectFileMissing
	}
	if err != nil {
		return fail(err)
	}
	cloudFlags := readProjectCloudFlags(cmd)
	if err := checkProjectCloudFlags(proj.cfg, cloudFlags); err != nil {
		return fail(err)
	}
	run, err := planProject(ctx, cmd, env, proj, cloudFlags, false)
	if err != nil {
		return fail(err)
	}
	plan := run.renderedPlan()
	if asJSON {
		data, err := plan.JSON()
		if err != nil {
			return fail(err)
		}
		if _, err := cmd.OutOrStdout().Write(data); err != nil {
			return fail(err)
		}
	} else {
		writeRenderedPlan(cmd.OutOrStdout(), plan, run.cloud != nil)
	}
	if exitCode && !plan.Converged() {
		return &projectExitError{code: 1, err: silentReported()}
	}
	return nil
}

// silentReported is an error that prints nothing and that the --json root
// backstop treats as already reported.
func silentReported() error { return cliutil.ReportedError(errSilent) }

var errSilent = errors.New("")

func writeProjectPlan(w io.Writer, plan projectconfig.LocalPlan) {
	writeRenderedPlan(w, plan.Plan, false)
}

func writeRenderedPlan(w io.Writer, plan projectconfig.Plan, withCloud bool) {
	text := plan.Text()
	if plan.Converged() {
		subject := "This daemon matches "
		if withCloud {
			subject = "This daemon and the cloud match "
		}
		text = subject + projectconfig.FileName + "; nothing to change.\n" + text
	}
	_, _ = io.WriteString(w, text)
}

// planProject plans the local leg and, with --cloud, the cloud leg. The cloud
// leg is observed first: its paid-gate precheck must pass before anything is
// planned, let alone written, on either leg.
func planProject(ctx context.Context, cmd *cobra.Command, env projectApplyEnv, proj *loadedProject, cloudFlags projectCloudFlags, prune bool) (*projectRun, error) {
	var cloud *projectCloudRun
	if cloudFlags.enabled {
		var err error
		if cloud, err = planCloudProject(ctx, cmd, env, proj, cloudFlags, prune); err != nil {
			return nil, err
		}
	}
	run, err := planLocalProject(ctx, cmd, env, proj, prune)
	if err != nil {
		return nil, err
	}
	run.cloud = cloud
	return run, nil
}

// projectApplyReport is apply's --json output. CloudResult is present only
// with --cloud. No minted secret ever appears in it.
type projectApplyReport struct {
	Plan        projectconfig.Plan         `json:"plan"`
	Result      *projectconfig.LocalResult `json:"result"`
	CloudResult *projectconfig.CloudResult `json:"cloudResult,omitempty"`
}

func runProjectApply(cmd *cobra.Command, env projectApplyEnv) error {
	flags := cmd.Flags()
	asJSON, _ := flags.GetBool(cliutil.JSONFlagName)
	dryRun, _ := flags.GetBool("dry-run")
	prune, _ := flags.GetBool("prune")
	var trust projectTrust
	trust.yes, _ = flags.GetBool("yes")
	trust.allowBranch, _ = flags.GetBool("allow-branch")
	if err := rejectProjectTransport(cmd, "apply"); err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	ctx := commandContext(cmd)
	dir, _ := flags.GetString("dir")
	proj, found, err := loadProjectFile(ctx, dir)
	if err == nil && !found {
		err = errProjectFileMissing
	}
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	cloudFlags := readProjectCloudFlags(cmd)
	if err := checkProjectCloudFlags(proj.cfg, cloudFlags); err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	run, err := planProject(ctx, cmd, env, proj, cloudFlags, prune)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	if asJSON && env.isTerminal() && run.needsConfirmation() && !trust.yes && !dryRun {
		// A JSON caller cannot answer a prompt on the same stream.
		env.isTerminal = func() bool { return false }
	}
	plan := run.renderedPlan()
	report := projectApplyReport{Plan: plan}
	finish := func(err error) error {
		if !asJSON {
			return err
		}
		if err != nil {
			return cliutil.EmitJSONReportedFailure(cmd, report, err)
		}
		return cliutil.EmitJSON(cmd, report)
	}
	if !asJSON {
		writeRenderedPlan(cmd.OutOrStdout(), plan, run.cloud != nil)
	}
	if dryRun {
		if !asJSON {
			_, _ = io.WriteString(cmd.OutOrStdout(), "Dry run: nothing was written.\n")
		}
		return finish(nil)
	}
	blocked := run.cloud != nil && run.cloud.plan.Blocked()
	if !run.hasWrites() {
		if blocked {
			return finish(projectCloudBlocked(run.cloud.plan))
		}
		return finish(nil)
	}
	if err := checkProjectTrust(ctx, cmd, env, run, trust); err != nil {
		return finish(err)
	}
	var errs []error
	if run.plan.HasWrites() {
		res, err := projectconfig.ApplyLocal(ctx, run.c, run.plan)
		report.Result = &res
		if !asJSON {
			_, _ = io.WriteString(cmd.OutOrStdout(), res.Text())
		}
		errs = append(errs, err)
	}
	if run.cloud != nil && run.cloud.plan.HasWrites() {
		res, err := projectconfig.ApplyCloud(ctx, run.cloud.c, stderrSecretSink{w: cmd.ErrOrStderr()}, run.cloud.plan)
		report.CloudResult = &res
		if !asJSON {
			_, _ = io.WriteString(cmd.OutOrStdout(), res.Text())
		}
		errs = append(errs, err)
	}
	if blocked {
		errs = append(errs, projectCloudBlocked(run.cloud.plan))
	}
	return finish(errors.Join(errs...))
}
