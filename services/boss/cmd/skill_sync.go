package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/skillinstall"
	"github.com/recurser/bossalib/config"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

// skillSyncMode selects how runSkillSync treats each agent's skill dir.
type skillSyncMode int

const (
	// skillSyncUpdateOnly (`boss skills sync`): EnsureUpdated per agent — a silent
	// no-op on a current tree and NEVER a fresh install into an empty dir.
	skillSyncUpdateOnly skillSyncMode = iota
	// skillSyncInstall (`boss skills install`): refresh a stale tree AND fresh-install
	// an empty one, so it doubles as first-time setup; a no-op when already current.
	skillSyncInstall
	// skillSyncForce (`boss skills install --force`): Extract unconditionally
	// (repair/overwrite), even when current.
	skillSyncForce
)

// skillsCmd exposes the non-interactive skill-refresh surface. Unlike the
// interactive startup prompt (maybeInstallSkills), these subcommands never check
// for a TTY and never emit a [Y/n] prompt, so they are safe to call from scripts,
// cron jobs, or a setup hook to close the dogfood loop after a skill edit + build.
func skillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage installed boss skills",
	}

	var syncAgent string
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Refresh installed boss skills to match this binary (update-only, no prompt)",
		// Reject positional operands: the agent is selected via --agent, and a bare
		// `boss skills sync codex` must error rather than silently ignore "codex" and
		// mutate every agent's global skill dir on PATH.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSkillSync(c.OutOrStdout(), skillSyncUpdateOnly, syncAgent)
		},
	}
	sync.Flags().StringVar(&syncAgent, "agent", "", "Restrict to one agent: claude or codex (default: all on PATH)")

	var force bool
	var installAgent string
	install := &cobra.Command{
		Use:   "install",
		Short: "Install or refresh boss skills (fresh-installs missing trees); --force reinstalls even when current",
		// Reject positional operands: the agent is selected via --agent, and a bare
		// `boss skills install codex` must error rather than silently ignore "codex"
		// and mutate every agent's global skill dir on PATH.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			mode := skillSyncInstall
			if force {
				mode = skillSyncForce
			}
			return runSkillSync(c.OutOrStdout(), mode, installAgent)
		},
	}
	install.Flags().BoolVar(&force, "force", false, "Reinstall (Extract) unconditionally, even when current")
	install.Flags().StringVar(&installAgent, "agent", "", "Restrict to one agent: claude or codex (default: all on PATH)")

	cmd.AddCommand(sync, install)
	return cmd
}

// runSkillSync writes the embedded skill payload into the global skill dir of each
// agent found on PATH, per mode:
//
//   - skillSyncUpdateOnly (sync): EnsureUpdated — refresh a stale tree, no-op when
//     current, and never fresh-install an empty dir (points the caller at `install`).
//   - skillSyncInstall (install): additionally Extract into a not-yet-installed dir,
//     so it works as first-time setup.
//   - skillSyncForce (install --force): Extract unconditionally.
//
// A single agent may be selected via only ("claude"/"codex"); when set, an
// unresolvable/failed agent makes the command exit non-zero, whereas in the
// all-agents mode a per-agent failure is a non-fatal warning. Successful extracts
// record the installed manifest so a later interactive boss does not re-prompt.
func runSkillSync(out io.Writer, mode skillSyncMode, only string) error {
	if only != "" {
		known := false
		for _, t := range skillInstallAgents {
			if t.command == only {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unknown agent %q (want claude or codex)", only)
		}
	}

	manifest, err := libskillinstall.Manifest(skillinstall.SkillsFS)
	if err != nil {
		return fmt.Errorf("compute skill manifest: %w", err)
	}

	// A malformed/unreadable settings.json makes config.Load return default
	// settings alongside an error. Saving those defaults would silently replace
	// the user's real file, so on a load error we still install/refresh the skill
	// trees (the point of the command) but skip all manifest bookkeeping and the
	// save. loadErr gates every recordSkillInstall below.
	settings, loadErr := config.Load()
	if loadErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Warning: skipping skill-manifest bookkeeping; failed to load settings: %v\n", loadErr)
	}
	settingsChanged := false
	var targetErr error

	recordErr := func(err error) {
		if only != "" {
			targetErr = err
		}
	}
	extract := func(target skillInstallAgent, dir, verb string) {
		if err := libskillinstall.Extract(dir, skillinstall.SkillsFS); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Warning: failed to %s %s skills: %v\n", verb, target.command, err)
			recordErr(err)
			return
		}
		_, _ = fmt.Fprintf(out, "boss skills: %s %s (%s)\n", verb, target.command, dir)
		if loadErr == nil && recordSkillInstall(&settings, target.agent, manifest) {
			settingsChanged = true
		}
	}

	for _, target := range skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		if _, err := skillInstallLookPath(target.command); err != nil {
			_, _ = fmt.Fprintf(out, "boss skills: %s not found on PATH, skipping\n", target.command)
			recordErr(fmt.Errorf("%s not found on PATH", target.command))
			continue
		}
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Warning: %s skills dir: %v\n", target.command, err)
			recordErr(err)
			continue
		}

		switch {
		case mode == skillSyncForce:
			extract(target, dir, "reinstalled")
		case mode == skillSyncInstall && !libskillinstall.IsInstalled(dir):
			// install doubles as first-time setup: fresh-install an empty dir.
			extract(target, dir, "installed")
		default:
			// Update-only: refresh a stale tree, no-op when current. sync never
			// fresh-installs (falls through to the not-installed hint below).
			updated, err := libskillinstall.EnsureUpdated(dir, skillinstall.SkillsFS)
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Warning: failed to update %s skills: %v\n", target.command, err)
				recordErr(err)
				continue
			}
			switch {
			case updated:
				_, _ = fmt.Fprintf(out, "boss skills: updated %s (%s)\n", target.command, dir)
				if loadErr == nil && recordSkillInstall(&settings, target.agent, manifest) {
					settingsChanged = true
				}
			case !libskillinstall.IsInstalled(dir):
				// sync is update-only, so "up to date" would be misleading for an
				// empty dir. Point the caller at the fresh-install path instead.
				_, _ = fmt.Fprintf(out, "boss skills: %s not installed (%s); run 'boss skills install' to install\n", target.command, dir)
			default:
				_, _ = fmt.Fprintf(out, "boss skills: %s up to date (%s)\n", target.command, dir)
			}
		}
	}

	if settingsChanged {
		_ = config.Save(settings)
	}
	return targetErr
}
