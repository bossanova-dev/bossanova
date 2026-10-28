package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type githubRepository struct{ Repository, Host string }
type githubAdmin interface {
	Check(context.Context, string, string) (githubRepository, error)
	ListLabels(context.Context, string) ([]string, error)
	CreateLabel(context.Context, string, string) error
	Viewer(context.Context, string) (string, error)
}
type ghGitHubAdmin struct{}

func (a *ghGitHubAdmin) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func (a *ghGitHubAdmin) Check(ctx context.Context, dir, repository string) (githubRepository, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return githubRepository{}, fmt.Errorf("gh-missing: install GitHub CLI 2.88.0 or newer and add gh to PATH")
	}
	data, err := a.run(ctx, dir, "--version")
	if err != nil {
		return githubRepository{}, fmt.Errorf("gh-outdated: install GitHub CLI 2.88.0 or newer")
	}
	if !supportedGitHubVersion(data) {
		return githubRepository{}, fmt.Errorf("gh-outdated: upgrade GitHub CLI to 2.88.0 or newer")
	}
	host := "github.com"
	if repository != "" {
		parsed, err := parseGitHubRepository(repository)
		if err != nil {
			return githubRepository{}, err
		}
		host = parsed.Host
	}
	// Discover the checkout's host without requiring a successful repository API call.
	if repository == "" {
		cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
		cmd.Dir = dir
		cmd.WaitDelay = 5 * time.Second
		if remote, err := cmd.Output(); err == nil {
			value := strings.TrimSpace(string(remote))
			if u, err := url.Parse(value); err == nil && u.Host != "" {
				host = u.Hostname()
			} else if before, _, ok := strings.Cut(value, ":"); ok {
				if _, h, ok := strings.Cut(before, "@"); ok {
					host = h
				}
			}
		}
	}
	if _, err := a.run(ctx, dir, "auth", "status", "--hostname", host); err != nil {
		return githubRepository{}, fmt.Errorf("unauthenticated: run gh auth login --hostname %s", host)
	}
	args := []string{"repo", "view"}
	if repository != "" {
		args = append(args, repository)
	}
	args = append(args, "--json", "nameWithOwner,url,hasIssuesEnabled")
	data, err = a.run(ctx, dir, args...)
	if err != nil {
		return githubRepository{}, fmt.Errorf("repository-unavailable: check the repository name and gh account access")
	}
	var repo struct {
		NameWithOwner    string `json:"nameWithOwner"`
		URL              string `json:"url"`
		HasIssuesEnabled bool   `json:"hasIssuesEnabled"`
	}
	if json.Unmarshal(data, &repo) != nil || repo.NameWithOwner == "" {
		return githubRepository{}, fmt.Errorf("repository-unavailable: gh returned no repository")
	}
	if !repo.HasIssuesEnabled {
		return githubRepository{}, fmt.Errorf("issues-disabled: enable Issues in the repository settings")
	}
	if u, err := url.Parse(repo.URL); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	repository = repo.NameWithOwner
	if host != "github.com" {
		repository = host + "/" + repository
	}
	return githubRepository{Repository: repository, Host: host}, nil
}
func (a *ghGitHubAdmin) ListLabels(ctx context.Context, repository string) ([]string, error) {
	data, err := a.run(ctx, "", "label", "list", "--repo", repository, "--limit", "1000", "--json", "name")
	if err != nil {
		return nil, fmt.Errorf("list GitHub labels failed; check gh repository access")
	}
	var labels []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &labels); err != nil {
		return nil, fmt.Errorf("unreadable GitHub labels")
	}
	if len(labels) >= 1000 {
		return nil, fmt.Errorf("GitHub label listing truncated at 1000; nothing written")
	}
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	return names, nil
}
func (a *ghGitHubAdmin) CreateLabel(ctx context.Context, repository, label string) error {
	if _, err := a.run(ctx, "", "label", "create", label, "--repo", repository); err == nil {
		return nil
	}
	labels, err := a.ListLabels(ctx, repository)
	if err == nil {
		for _, name := range labels {
			if strings.EqualFold(name, label) {
				return nil
			}
		}
	}
	return fmt.Errorf("create GitHub label %q failed; check gh repository write access", label)
}
func (a *ghGitHubAdmin) Viewer(ctx context.Context, host string) (string, error) {
	data, err := a.run(ctx, "", "api", "--hostname", host, "user", "--jq", ".login")
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("read gh login failed; run gh auth login --hostname %s", host)
	}
	return strings.TrimSpace(string(data)), nil
}

type githubLabelEntry struct{ Kind, Role, Name string }
type githubDefaults struct {
	States, Labels map[string]string
	Required       []githubLabelEntry
}

func githubRoleDefaults() (githubDefaults, error) {
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		return githubDefaults{}, err
	}
	defer cleanup()
	script := fmt.Sprintf(`import {pathToFileURL} from 'node:url'
const mod=await import(pathToFileURL(%s).href)
const gh=await import(pathToFileURL(%s).href)
const block=mod.withTrackerDefaults(mod.mergeConfig(mod.DEFAULT_CONFIG,{adapters:{tracker:'github'},trackerConfig:{github:{repository:'o/r'}}})).trackerConfig.github
process.stdout.write(JSON.stringify({States:Object.fromEntries(Object.entries(block.states).filter(([role])=>!(role in gh.GITHUB_CLOSING_ROLES))),Labels:block.labels,Required:gh.githubRequiredLabels(block)}))`, jsString(bridge.module), jsString(filepath.Join(bridge.dir, "tracker/github-config.mjs")))
	data, err := bridge.run(script, "read GitHub label defaults")
	if err != nil {
		return githubDefaults{}, err
	}
	var result githubDefaults
	err = json.Unmarshal(data, &result)
	return result, err
}
func parseGitHubRepository(repository string) (githubRepository, error) {
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		return githubRepository{}, err
	}
	defer cleanup()
	data, err := bridge.run(fmt.Sprintf(`import {pathToFileURL} from 'node:url'; const mod=await import(pathToFileURL(%s).href);process.stdout.write(JSON.stringify(mod.parseRepository(%s)))`, jsString(filepath.Join(bridge.dir, "tracker/github-config.mjs")), jsString(repository)), "parse GitHub repository")
	if err != nil {
		return githubRepository{}, err
	}
	var result githubRepository
	err = json.Unmarshal(data, &result)
	return result, err
}
func (ov linearOverrides) trackerName() string {
	if ov.Tracker == "github" {
		return "github"
	}
	return "linear"
}
func (ov linearOverrides) stateDefaults() map[string]string {
	if ov.Tracker == "github" {
		defaults, err := githubRoleDefaults()
		if err == nil {
			return defaults.States
		}
		return nil
	}
	return linearStateDefaults
}
func (ov linearOverrides) labelDefaults() map[string]string {
	if ov.Tracker == "github" {
		defaults, err := githubRoleDefaults()
		if err == nil {
			return defaults.Labels
		}
		return nil
	}
	return linearLabelDefaults
}

type githubMappingResult struct {
	Overrides                    linearOverrides
	Repository, Viewer           string
	MissingLabels, CreatedLabels []string
}

func runGitHubMapping(ctx context.Context, admin githubAdmin, repoDir string, explicit linearOverrides, createLabels bool) (githubMappingResult, error) {
	result := githubMappingResult{Overrides: explicit}
	repo, err := admin.Check(ctx, repoDir, explicit.Repository)
	if err != nil {
		return result, err
	}
	result.Repository = repo.Repository
	result.Overrides.Repository = repo.Repository
	result.Viewer, err = admin.Viewer(ctx, repo.Host)
	if err != nil {
		return result, err
	}
	defaults, err := githubRoleDefaults()
	if err != nil {
		return result, err
	}
	labels, err := admin.ListLabels(ctx, repo.Repository)
	if err != nil {
		return result, err
	}
	if len(labels) >= 1000 {
		return result, fmt.Errorf("GitHub label listing truncated; nothing written")
	}
	if err := result.mapRoles(labels, defaults, explicit); err != nil {
		return result, err
	}
	if createLabels {
		for _, name := range result.MissingLabels {
			if err := admin.CreateLabel(ctx, repo.Repository, name); err != nil {
				return result, err
			}
			result.CreatedLabels = append(result.CreatedLabels, name)
		}
		result.MissingLabels = nil
	}

	return result, nil
}
func writeGitHubMapping(out io.Writer, m githubMappingResult, steps *initAppliedSteps) {
	for _, name := range m.CreatedLabels {
		_, _ = fmt.Fprintf(out, "Created GitHub label %s\n", name)
	}
	if steps != nil {
		steps.LabelsCreated = append(steps.LabelsCreated, m.CreatedLabels...)
	}
	if len(m.MissingLabels) > 0 {
		_, _ = fmt.Fprintf(out, "GitHub labels missing: %s; run boss init --merge --create-labels\n", strings.Join(m.MissingLabels, ", "))
	}
}

// selectedInitTracker resolves the merge default before any credential reads.
func selectedInitTracker(tracker, dir string, merge bool) string {
	if tracker != "" {
		return tracker
	}
	if merge {
		if dir == "" {
			dir, _ = os.Getwd()
		}
		data, _ := os.ReadFile(filepath.Join(dir, ".boss-skills.json"))
		var cfg struct {
			Adapters struct {
				Tracker string `json:"tracker"`
			} `json:"adapters"`
		}
		if json.Unmarshal(data, &cfg) == nil && cfg.Adapters.Tracker != "" {
			return cfg.Adapters.Tracker
		}
	}
	return "linear"
}

func parseTrackerOverrides(tracker string, states, labels []string, me bool) (linearOverrides, error) {
	if tracker != "github" {
		ov, err := parseLinearOverrides(states, labels, me)
		ov.Tracker = tracker
		return ov, err
	}
	ov := linearOverrides{Tracker: tracker, States: map[string]string{}, Labels: map[string]string{}, AssigneeMe: me}
	for _, group := range []struct {
		values []string
		dest   map[string]string
	}{{states, ov.States}, {labels, ov.Labels}} {
		for _, value := range group.values {
			role, name, ok := strings.Cut(value, "=")
			if !ok {
				return ov, fmt.Errorf("override %q must be role=name", value)
			}
			group.dest[role] = name
		}
	}
	return ov, ov.Validate()
}
func validateInitTrackerFlags(c *cobra.Command, tracker, repository string, states []string) error {
	if tracker != "github" && tracker != "linear" {
		return fmt.Errorf("--tracker must be linear or github")
	}
	if tracker == "linear" && c.Flags().Changed("github-repo") {
		return fmt.Errorf("--github-repo requires --tracker github")
	}
	if tracker == "github" {
		if c.Flags().Changed("team") || c.Flags().Changed("linear-key-stdin") {
			return fmt.Errorf("--tracker github cannot be combined with --team or --linear-key-stdin")
		}
		for _, state := range states {
			role, _, _ := strings.Cut(state, "=")
			if role == "done" || role == "canceled" || role == "duplicate" {
				return fmt.Errorf("--state %s is a closing role on github and cannot be overridden", role)
			}
		}
		if repository != "" {
			_, err := parseGitHubRepository(repository)
			return err
		}
	}
	return nil
}
func (g *Group) resolveInitTrackerFor(out io.Writer, bridge *nodeBridge, repoDir string, existing []byte, opts *initOptions, detected *detectedConfig) (initTracker, linearOverrides, error) {
	ov := opts.overrides
	if opts.tracker != "" {
		ov.Tracker = opts.tracker
	}
	if opts.githubRepo != "" {
		ov.Repository = opts.githubRepo
	}
	if existing != nil {
		ov = seedOverridesFromExisting(existing, ov)
	}
	if ov.Tracker == "github" {
		admin := opts.github
		if admin == nil {
			admin = g.initNewGitHubAdmin()
		}
		mapping, err := runGitHubMapping(context.Background(), admin, repoDir, ov, opts.createLabels)
		if err != nil {
			return initTracker{}, ov, fmt.Errorf("%w; nothing written", err)
		}
		writeGitHubMapping(out, mapping, opts.applied)
		if existing != nil {
			_, _ = fmt.Fprintf(out, "Tracker: GitHub Issues %s — gh login %s\n", mapping.Repository, mapping.Viewer)
		}
		ov = mapping.Overrides
		opts.applied.GitHubViewer = mapping.Viewer
		opts.applied.GitHubMissing = mapping.MissingLabels
		opts.applied.Overrides = ov
		detected.Adapters = &adaptersBlock{Tracker: "github"}
		detected.TrackerConfig = &trackerBlock{GitHub: &githubTrackerBlock{Repository: mapping.Repository}}
		return initTracker{repository: mapping.Repository, viewer: mapping.Viewer}, ov, nil
	}
	tracker, err := resolveInitTracker(context.Background(), bridge, *opts)
	if err != nil {
		return tracker, ov, err
	}
	if opts.mapRoles && tracker.teamID != "" {
		team := linearTeam{ID: tracker.teamID, Name: tracker.team, Key: tracker.teamKey}
		ov, err = runLinearMapping(context.Background(), out, opts.linear, team, ov, opts.createLabels, opts.applied)
		if err != nil {
			return tracker, ov, fmt.Errorf("%w; nothing written", err)
		}
	} else if opts.createLabels {
		_, _ = fmt.Fprintln(out, "--create-labels: no Linear team was verified with LINEAR_API_KEY, so no labels were looked up or created")
	}
	if ov.Tracker == "linear" && opts.command != nil && !opts.command.Flags().Changed("tracker") {
		var old struct {
			Adapters struct {
				Tracker string `json:"tracker"`
			} `json:"adapters"`
		}
		_ = json.Unmarshal(existing, &old)
		if old.Adapters.Tracker != "github" {
			ov.Tracker = ""
		}
	}
	ov.Team, ov.TeamKey = tracker.team, tracker.teamKey
	opts.applied.Overrides = ov
	if tracker.team != "" {
		detected.TrackerConfig = &trackerBlock{Linear: &linearTrackerBlock{Team: tracker.team, TeamKey: tracker.teamKey}}
	}
	if ov.Tracker != "" {
		detected.Adapters = &adaptersBlock{Tracker: ov.Tracker}
	}
	return tracker, ov, nil
}

func supportedGitHubVersion(data []byte) bool {
	fields := strings.Fields(string(data))
	if len(fields) <= 2 {
		return false
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 3 {
		return false
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	return major > 2 || major == 2 && minor >= 88
}

func (result *githubMappingResult) mapRoles(labels []string, defaults githubDefaults, explicit linearOverrides) error {
	entries := append([]githubLabelEntry{}, defaults.Required...)
	chosen := map[string]string{}
	result.Overrides.States, result.Overrides.Labels = map[string]string{}, map[string]string{}
	for _, entry := range entries {
		want := entry.Name
		dest := result.Overrides.Labels
		baseline := defaults.Labels
		switch entry.Kind {
		case "state":
			dest = result.Overrides.States
			baseline = defaults.States
			if override := explicit.States[entry.Role]; override != "" {
				want = override
			}
		case "pipeline":
			if override := explicit.Labels[entry.Role]; override != "" {
				want = override
			}
		}
		if entry.Kind != "ranking" {
			key := strings.ToLower(want)
			if previous, ok := chosen[key]; ok {
				return fmt.Errorf("GitHub roles %s and %s map to the same label %q", previous, entry.Role, want)
			}
			chosen[key] = entry.Role
		}
		found := false
		for _, name := range labels {
			if strings.EqualFold(name, want) {
				want = name
				found = true
				break
			}
		}
		if entry.Kind != "ranking" && want != baseline[entry.Role] {
			dest[entry.Role] = want
		}
		if found {
			continue
		}
		result.MissingLabels = append(result.MissingLabels, want)
	}
	return nil
}
