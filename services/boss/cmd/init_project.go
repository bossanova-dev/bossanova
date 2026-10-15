package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/projectconfig"
)

// boss init applies a committed .boss-project.json through the same engine
// as `boss project apply`, never pruning. The file is loaded and validated,
// and the branch check runs, before init writes anything. The pre-check and
// apply gate share one default-branch resolver. The project step
// itself runs last, after the config write and the factory cron step, so a
// refusal leaves init's other converging work standing. Without the file init
// is unchanged.

// initProjectOverrides are the init flags that take a class of the file for
// this run. They are read before the interview, which clears the repo flags
// once it has applied them.
type initProjectOverrides struct {
	crons, registration, secrets bool
}

// prepareInitProject loads the checkout's project file (absent outside a git
// checkout) and, for a local run that may write, refuses an off-default-branch
// checkout without --allow-branch. It runs once per init, before any write.
func prepareInitProject(ctx context.Context, repoDir string, opts *initOptions) error {
	if opts.projectLoaded {
		return nil
	}
	opts.projectLoaded = true
	proj, found, err := loadProjectFile(ctx, repoDir)
	if errors.Is(err, errNotGitCheckout) || !found {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w; nothing was written", err)
	}
	opts.project = proj
	opts.projectOverrides = initProjectOverrides{
		crons:        opts.cron.requested(),
		registration: opts.repo.noRegister,
		secrets:      opts.repo.linearStdin || opts.repo.sentryStdin || opts.repo.storeEnv || opts.repo.sentryOrg != "",
	}
	if !initRunIsLocal(opts) || opts.projectTrust.allowBranch {
		return nil
	}
	env := newProjectApplyEnv()
	state := observeProjectBranchState(ctx, opts.command, env, proj)
	if _, want, ok := projectBranchCheck(ctx, env, proj, state); !ok {
		return fmt.Errorf("%s is present and the checkout is not on the repo's default branch %q; nothing was written (re-run with --allow-branch to apply it from here)", proj.path, want)
	}
	return nil
}

func initRunIsLocal(opts *initOptions) bool {
	return opts.command != nil && remoteURL(opts.command) == "" && hostDestination(opts.command) == ""
}

// projectAlreadyExistsHint is added to init's refusal of an existing config.
func projectAlreadyExistsHint(opts initOptions) string {
	if opts.project == nil {
		return ""
	}
	return "\n" + projectconfig.FileName + " is present: run `boss project apply` to apply it without re-initialising"
}

// finishInit runs the steps after the config write: factory crons, then the
// project config.
func finishInit(out io.Writer, opts initOptions, repoDir, configPath string, repo *pb.Repo) error {
	if err := runInitCronStep(out, opts, repoDir, configPath, repo); err != nil {
		return err
	}
	return runInitProjectStep(out, opts)
}

// runInitProjectStep applies the project file to the local daemon, minus the
// classes an explicit flag took over. A refusal or a failed write returns an
// error after init's other steps; the flag path prints what was applied and
// the command that finishes the job.
func runInitProjectStep(out io.Writer, opts initOptions) error {
	proj := opts.project
	if proj == nil {
		return nil
	}
	cmd := opts.command
	switch {
	case cmd == nil:
		_, _ = io.WriteString(out, "\nProject config skipped: no daemon connection is available.\n")
		return nil
	case !initRunIsLocal(&opts):
		_, _ = io.WriteString(out, "\nProject config skipped: "+projectconfig.FileName+" applies to a local daemon and a local checkout (--remote / --host has neither).\n")
		return nil
	}
	ctx := commandContext(cmd)
	env := newProjectApplyEnv()
	_, _ = fmt.Fprintf(out, "\nProject config (%s):\n", proj.path)
	run, err := planLocalProject(ctx, cmd, env, proj, false)
	if err != nil {
		_, _ = fmt.Fprintf(out, "Project config skipped: %s.\n", err)
		opts.applied.ProjectRerun = "boss project apply"
		return nil
	}
	run.plan = applyInitOverrides(out, run.plan, opts.projectOverrides)
	writeProjectPlan(out, run.plan)
	if !run.plan.HasWrites() {
		return nil
	}
	stop := func(err error, rerun string) error {
		opts.applied.ProjectRerun = rerun
		if opts.projectTrust.confirm == nil {
			// The interview prints this itself when the run stops.
			_, _ = io.WriteString(out, opts.applied.stoppedReport())
		}
		return err
	}
	if err := checkProjectTrust(ctx, cmd, env, run, opts.projectTrust); err != nil {
		rerun := "boss project apply"
		if errors.Is(err, errProjectNotConfirmed) {
			rerun += " --yes"
		}
		return stop(err, rerun)
	}
	res, err := projectconfig.ApplyLocal(ctx, run.c, run.plan)
	for _, c := range res.Applied {
		opts.applied.Project = append(opts.applied.Project, fmt.Sprintf("%s %s %q", c.Action, c.Resource, c.Identity))
	}
	_, _ = io.WriteString(out, res.Text())
	if err != nil {
		return stop(err, "boss project apply")
	}
	return nil
}

// applyInitOverrides drops the classes an explicit init flag took over, with
// one warning each.
func applyInitOverrides(out io.Writer, plan projectconfig.LocalPlan, o initProjectOverrides) projectconfig.LocalPlan {
	warn := func(msg string) { _, _ = fmt.Fprintf(out, "warning: %s\n", msg) }
	if o.crons {
		warn("--cron overrides the crons in " + projectconfig.FileName + " for this run")
		plan = plan.WithoutClass(projectconfig.ClassCrons, "--cron was passed")
	}
	if o.registration {
		warn("--no-register overrides the repository registration in " + projectconfig.FileName + " for this run")
		plan = plan.WithoutRepoCreate("--no-register was passed")
	}
	if o.secrets {
		warn("the credential flags override the secrets in " + projectconfig.FileName + " for this run")
		plan = plan.WithoutClass(projectconfig.ClassSecrets, "a credential flag was passed")
	}
	return plan
}

// projectSelectsCronsHere reports whether the file places any cron on this
// daemon, so the interview leaves the cron questions to the file.
func projectSelectsCronsHere(proj *loadedProject) bool {
	if proj == nil {
		return false
	}
	identity, _ := applyDaemonIdentity(newProjectApplyEnv().export)
	return len(projectconfig.CronsForDaemon(proj.cfg, identity)) > 0
}
