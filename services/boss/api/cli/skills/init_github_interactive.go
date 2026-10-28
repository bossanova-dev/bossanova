package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (iv *initInterview) askTracker() error {
	if !iv.cmd.Flags().Changed("tracker") {
		for {
			tracker, err := iv.ask("Tracker: linear or github (--tracker)", iv.opts.tracker)
			if err != nil {
				return err
			}
			if tracker == "linear" || tracker == "github" {
				iv.opts.tracker = tracker
				break
			}
			iv.p.Say("Choose linear or github.")
		}
	}
	iv.opts.overrides.Tracker = iv.opts.tracker
	iv.opts.repo.tracker = iv.opts.tracker
	if iv.opts.tracker == "github" && (iv.cmd.Flags().Changed("team") || iv.opts.repo.linearStdin) {
		return errors.New("--team and --linear-key-stdin require --tracker linear")
	}
	if iv.opts.tracker == "linear" && iv.cmd.Flags().Changed("github-repo") {
		return errors.New("--github-repo requires --tracker github")
	}
	return iv.opts.repo.readCredentials(iv.cmd.InOrStdin(), iv.getenv)
}

func (iv *initInterview) askGitHubRepo() error {
	if iv.opts.tracker != "github" {
		return nil
	}
	if iv.opts.github == nil {
		iv.opts.github = iv.group.initNewGitHubAdmin()
	}
	repo := iv.opts.githubRepo
	if repo == "" {
		if existing, err := os.ReadFile(filepath.Join(iv.repoDir, ConfigFilename)); err == nil && (!iv.force || iv.opts.mergeExisting) {
			repo = seedOverridesFromExisting(existing, iv.opts.overrides).Repository
		}
		resolved, err := iv.opts.github.Check(iv.ctx, iv.repoDir, repo)
		if err != nil {
			return err
		}
		repo, err = iv.ask("GitHub repository ([HOST/]OWNER/REPO; --github-repo)", resolved.Repository)
		if err != nil {
			return err
		}
	}
	resolved, err := iv.opts.github.Check(iv.ctx, iv.repoDir, repo)
	if err != nil {
		return err
	}
	viewer, err := iv.opts.github.Viewer(iv.ctx, resolved.Host)
	if err != nil {
		return err
	}
	iv.opts.githubRepo, iv.opts.overrides.Repository = resolved.Repository, resolved.Repository
	iv.steps.GitHubViewer = viewer
	iv.p.Say("GitHub Issues %s uses gh login %s; no tracker MCP server is needed.", resolved.Repository, viewer)
	return nil
}

func (iv *initInterview) askGitHubRoles() error {
	if iv.opts.tracker != "github" {
		return nil
	}
	defaults, err := githubRoleDefaults()
	if err != nil {
		return err
	}
	seeded := iv.opts.overrides
	if !iv.force || iv.opts.mergeExisting {
		if existing, err := os.ReadFile(filepath.Join(iv.repoDir, ConfigFilename)); err == nil {
			seeded = seedOverridesFromExisting(existing, seeded)
		}
	}
	labels, err := iv.opts.github.ListLabels(iv.ctx, iv.opts.githubRepo)
	if err != nil {
		return err
	}
	iv.p.Say("GitHub repository labels: %s", strings.Join(labels, ", "))
	for _, entry := range defaults.Required {
		if entry.Kind == "ranking" {
			continue
		}
		values, seeds, flag := iv.opts.overrides.States, seeded.States, "state"
		if entry.Kind == "pipeline" {
			values, seeds, flag = iv.opts.overrides.Labels, seeded.Labels, "label"
		}
		if _, set := values[entry.Role]; set {
			continue
		}
		def := seeds[entry.Role]
		if def == "" {
			def = entry.Name
		}
		for _, label := range labels {
			if strings.EqualFold(def, label) {
				def = label
				break
			}
		}
		answer, err := iv.ask(fmt.Sprintf("GitHub %s for %s (--%s %s=<name>)", entry.Kind, entry.Role, flag, entry.Role), def)
		if err != nil {
			return err
		}
		if answer != "" {
			values[entry.Role] = answer
		}
	}
	// The same mapping validates flag and interview answers before any label writes.
	mapping, err := runGitHubMapping(iv.ctx, iv.opts.github, iv.repoDir, overlayOverrides(seeded, iv.opts.overrides), false)
	if err != nil {
		return err
	}
	if len(mapping.MissingLabels) == 0 || iv.opts.createLabels {
		return nil
	}
	iv.opts.createLabels, err = iv.confirm(fmt.Sprintf("Create the %d missing labels %s in %s? (--create-labels)", len(mapping.MissingLabels), strings.Join(mapping.MissingLabels, ", "), iv.opts.githubRepo), true)
	return err
}

func (iv *initInterview) askGitHubFilter() error {
	if iv.opts.overrides.AssigneeMe {
		return nil
	}
	iv.p.Say("\"Only my tickets\" resolves `me` to gh login %s.", iv.steps.GitHubViewer)
	me, err := iv.confirm("Only pick up tickets assigned to you? Writes selection.assignees.include: [\"me\"] to .boss-skills.json (--assignee-me)", false)
	iv.opts.overrides.AssigneeMe = me
	return err
}
