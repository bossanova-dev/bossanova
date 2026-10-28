package skills

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/recurser/boss/internal/domain/display"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// initAppliedSteps records applied writes separately from successful read-back.
// The interactive installer prints it when a run stops early and builds its
// summary from it, so every write boss init makes lands here.
type initAppliedSteps struct {
	Registered, LinearStored, SentryStored bool
	LinearVerified, SentryVerified         bool
	// RepoID is the registration the run resolved or created.
	RepoID string
	// LabelsCreated names each Linear label created in the team.
	LabelsCreated []string
	GitHubViewer  string
	GitHubMissing []string
	// ConfigPath is set once the config file has been written (created,
	// replaced or merged); an unchanged merge leaves it empty.
	ConfigPath string
	StartSlow  bool
	// Overrides is the tracker override set the config write used.
	Overrides linearOverrides
	// Crons is every factory cron stage outcome, including partial runs, with
	// the warnings and follow-ups cron setup reported.
	Crons                      []initCronStageResult
	CronWarnings, CronLeftToDo []string
	// Project is every .boss-project.json change applied; ProjectRerun names
	// the command that finishes a refused or failed project step.
	Project      []string
	ProjectRerun string
}

// applied lists every write the run made, in the order the steps run. Read-back
// is reported alongside each credential so a stored-but-unverified write is not
// mistaken for a verified one.
func (s *initAppliedSteps) applied() []string {
	var items []string
	if s.Registered {
		items = append(items, s.registrationText())
	}
	if s.LinearStored {
		items = append(items, s.linearText())
	}
	if s.SentryStored {
		items = append(items, s.sentryText())
	}
	if len(s.LabelsCreated) > 0 {
		items = append(items, s.labelsText())
	}
	if s.ConfigPath != "" {
		items = append(items, s.configText())
	}
	for _, c := range s.Crons {
		switch c.Outcome {
		case cronCreated, cronDisabledByPreflight, cronUpdated:
			items = append(items, c.appliedText())
		case cronLeftAlone, cronUnchanged:
			// Nothing was written for this stage.
		}
	}
	for _, p := range s.Project {
		items = append(items, projectChangeText(p))
	}
	return items
}

func (s *initAppliedSteps) registrationText() string {
	text := "repository registered with bossd"
	if s.RepoID != "" {
		text += " (id " + s.RepoID + ")"
	}
	return text
}

func readBackText(ok bool) string {
	if ok {
		return " (read back)"
	}
	return " (not read back)"
}

func (s *initAppliedSteps) linearText() string {
	return "Linear key stored" + readBackText(s.LinearVerified)
}

func (s *initAppliedSteps) sentryText() string {
	return "Sentry credentials stored" + readBackText(s.SentryVerified)
}

func (s *initAppliedSteps) labelsText() string {
	tracker := "Linear"
	if s.Overrides.Tracker == "github" {
		tracker = "GitHub"
	}
	return tracker + " labels created: " + strings.Join(s.LabelsCreated, ", ")
}

func (s *initAppliedSteps) configText() string { return "config written: " + s.ConfigPath }

func projectChangeText(change string) string { return "project config: " + change }

func (c initCronStageResult) appliedText() string {
	return "cron job " + c.Name + " " + c.outcomeText()
}

// outcomeText is shared by the closing summary and interrupted-run report.
func (c initCronStageResult) outcomeText() string {
	state := "disabled"
	if c.Enabled {
		state = "enabled"
	}
	switch c.Outcome {
	case cronCreated:
		return "created (" + state + ")"
	case cronDisabledByPreflight:
		return "created disabled (preflight failed)"
	case cronUpdated:
		return "updated (" + state + ")"
	case cronUnchanged:
		return "already matches"
	case cronLeftAlone:
		return "differs, left untouched"
	}
	return string(c.Outcome)
}

// stoppedReport is what an interrupted or failed run prints: what it already
// applied, and that a re-run finishes the job (every step converges).
func (s *initAppliedSteps) stoppedReport() string {
	var b strings.Builder
	b.WriteString("\nboss init stopped before finishing.\n")
	if items := s.applied(); len(items) > 0 {
		b.WriteString("Applied so far:\n")
		for _, item := range items {
			b.WriteString("  - " + item + "\n")
		}
	} else {
		b.WriteString("Nothing was applied.\n")
	}
	b.WriteString("Re-run `boss init` to finish.\n")
	if s.ProjectRerun != "" {
		b.WriteString("Or run `" + s.ProjectRerun + "` to apply .boss-project.json on its own.\n")
	}
	return b.String()
}

type initRepoOptions struct {
	register, noRegister, linearStdin, sentryStdin, storeEnv bool
	// trusted registers a new repository as trusted (Repo.is_trusted).
	trusted                bool
	sentryOrg              string
	tracker                string
	linearKey, sentryToken string
}

func (o initRepoOptions) requested() bool {
	return o.register || o.noRegister || o.linearStdin || o.sentryStdin || o.storeEnv || o.trusted || o.sentryOrg != ""
}

func (o *initRepoOptions) validateSources(getenv func(string) string) error {
	if o.register && o.noRegister {
		return errors.New("--register and --no-register cannot be combined")
	}
	if o.linearStdin && o.sentryStdin {
		return errors.New("only one of --linear-key-stdin and --sentry-token-stdin may be used")
	}
	if o.storeEnv {
		if o.tracker != "github" {
			o.linearKey = strings.TrimSpace(getenv("LINEAR_API_KEY"))
		}
		o.sentryToken = strings.TrimSpace(getenv("SENTRY_AUTH_TOKEN"))
	}
	if (o.sentryOrg != "") != (o.sentryStdin || o.sentryToken != "") {
		return errors.New("provide both --sentry-org and a Sentry token")
	}
	return nil
}

func (o *initRepoOptions) readCredentials(in io.Reader, getenv func(string) string) error {
	if err := o.validateSources(getenv); err != nil {
		return err
	}
	if o.linearStdin || o.sentryStdin {
		b, err := io.ReadAll(io.LimitReader(in, 64*1024+1))
		if err != nil || len(b) > 64*1024 {
			return errors.New("could not read credential from stdin")
		}
		value := strings.TrimSpace(string(b))
		if value == "" {
			return errors.New("credential on stdin must not be empty")
		}
		if o.linearStdin {
			o.linearKey = value
		} else {
			o.sentryToken = value
		}
	}
	return nil
}

// mainCheckout resolves aliases, subdirectories and linked worktrees consistently.
func MainCheckout(ctx context.Context, dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", errors.New("cannot resolve repository path")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", resolved, "rev-parse", "--path-format=absolute", "--git-common-dir")
	data, err := cmd.Output()
	if err != nil {
		return "", errors.New("registration requires a git repository; config-only init is still available without --register or credential flags")
	}
	common, err := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
	if err != nil {
		return "", errors.New("cannot resolve main checkout")
	}
	return filepath.Dir(common), nil
}

// initRegistration says whether resolveInitRepo may register an unregistered
// repository and whether such a new registration is trusted. trusted applies
// only when the call registers; an existing registration keeps its trust
// (change it with `boss repo update --trusted`).
type initRegistration struct {
	allow   bool
	trusted bool
}

// resolveInitRepo exposes the canonical repo and id to subsequent installer steps.
func resolveInitRepo(ctx context.Context, c Client, dir string, reg initRegistration, steps *initAppliedSteps) (*pb.Repo, error) {
	main, err := MainCheckout(ctx, dir)
	if err != nil {
		return nil, err
	}
	repo, err := FindInitRepo(ctx, c, main)
	if err != nil || repo != nil {
		if repo != nil {
			steps.RepoID = repo.GetId()
		}
		return repo, err
	}
	if !reg.allow {
		return nil, errors.New("repository is not registered")
	}
	validation, err := c.ValidateRepoPath(ctx, main)
	if err != nil || validation == nil || !validation.IsValid {
		return nil, errors.New("repository path validation failed")
	}
	name, branch := display.RepoNameAndBranch(main, validation)
	req := &pb.RegisterRepoRequest{LocalPath: main, DisplayName: name, DefaultBaseBranch: branch}
	if reg.trusted {
		req.IsTrusted = &reg.trusted
	}
	repo, err = c.RegisterRepo(ctx, req)
	if err != nil || repo == nil {
		return nil, errors.New("repository registration failed")
	}
	steps.Registered = true
	steps.RepoID = repo.GetId()
	return repo, nil
}

// findInitRepo returns the registration whose main checkout is main, or nil
// when the repository is not registered.
func FindInitRepo(ctx context.Context, c Client, main string) (*pb.Repo, error) {
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return nil, errors.New("could not list repository registrations")
	}
	for _, repo := range repos {
		path, e := MainCheckout(ctx, repo.LocalPath)
		if e == nil && path == main {
			return repo, nil
		}
	}
	return nil, nil
}

// configLocationNote warns when the config is written somewhere other than the
// registration's main checkout (a linked worktree): the cron gate reads the
// main checkout, so it sees the config only once it reaches it.
func configLocationNote(ctx context.Context, dir string, repo *pb.Repo) string {
	target, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	target, _ = filepath.EvalSymlinks(target)
	main, mainErr := MainCheckout(ctx, repo.LocalPath)
	if mainErr == nil && target != main {
		return "Config is written here; the cron gate reads the main checkout and will see it after it reaches the main checkout or base branch.\n"
	}
	return ""
}

func storeInitCredentials(ctx context.Context, c Client, repo *pb.Repo, o initRepoOptions, validate func(context.Context, string) (string, error), steps *initAppliedSteps, out io.Writer) error {
	linear := o.linearKey != "" && o.linearKey != repo.GetLinearApiKey()
	sentry := o.sentryToken != "" && (o.sentryToken != repo.GetSentryApiKey() || o.sentryOrg != repo.GetSentryOrg())
	if !linear && !sentry {
		return nil
	}
	if linear && validate != nil {
		if _, err := validate(ctx, o.linearKey); err != nil {
			return errors.New("linear key validation failed; stored key unchanged")
		}
	}
	req := &pb.UpdateRepoRequest{Id: repo.Id}
	if linear {
		req.LinearKey = &pb.SecretUpdate{Action: pb.SecretAction_SECRET_ACTION_SET, Value: &o.linearKey}
	}
	if sentry {
		req.SentryKey = &pb.SecretUpdate{Action: pb.SecretAction_SECRET_ACTION_SET, Value: &o.sentryToken}
		req.SentryOrg = &o.sentryOrg
	}
	if _, err := c.UpdateRepo(ctx, req); err != nil {
		return errors.New("credentials not updated")
	}
	// The daemon accepted the write even if verification subsequently fails.
	// Do not roll it back: another writer may already have changed these keys.
	if linear {
		steps.LinearStored = true
	}
	if sentry {
		steps.SentryStored = true
	}
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return errors.New("credential update succeeded but could not be verified: read-back failed")
	}
	for _, stored := range repos {
		if stored.Id != repo.Id {
			continue
		}
		if (linear && stored.GetLinearApiKey() != o.linearKey) || (sentry && (stored.GetSentryApiKey() != o.sentryToken || stored.GetSentryOrg() != o.sentryOrg)) {
			return errors.New("credential update succeeded but could not be verified: read-back mismatch")
		}
		if linear {
			steps.LinearVerified = true
			message := "Linear key stored: verified (not validated)\n"
			if validate != nil {
				message = "Linear key stored: verified and validated\n"
			}
			_, _ = io.WriteString(out, message)
		}
		if sentry {
			steps.SentryVerified = true
			_, _ = io.WriteString(out, "Sentry credentials stored: verified\n")
		}
		return nil
	}
	return errors.New("credential update succeeded but could not be verified: repository missing on read-back")
}

func (g *Group) prepareInitRepo(cmd *cobra.Command, dir string, o initRepoOptions, validate func(context.Context, string) (string, error), steps *initAppliedSteps) (*pb.Repo, error) {
	if !o.requested() {
		return nil, nil
	}
	if remoteURL(cmd) != "" || hostDestination(cmd) != "" {
		_, _ = io.WriteString(cmd.OutOrStdout(), "Repository registration and credential storage skipped: these require a local path (--remote / --host uses config-only init).\n")
		return nil, nil
	}
	if err := o.readCredentials(cmd.InOrStdin(), os.Getenv); err != nil {
		return nil, err
	}
	if dir == "" {
		dir = "."
	}
	c, err := g.newClient(cmd)
	if err != nil {
		return nil, errors.New("could not connect to local bossd")
	}
	repo, err := resolveInitRepo(cmd.Context(), c, dir, initRegistration{allow: !o.noRegister, trusted: o.trusted}, steps)
	if err != nil {
		return nil, err
	}
	if !steps.Registered {
		_, _ = io.WriteString(cmd.OutOrStdout(), trustedIgnoredNote(o.trusted, repo))
	}
	_, _ = io.WriteString(cmd.OutOrStdout(), configLocationNote(cmd.Context(), dir, repo))
	if err := storeInitCredentials(cmd.Context(), c, repo, o, validate, steps, cmd.OutOrStdout()); err != nil {
		return repo, err
	}
	return repo, nil
}

// trustedIgnoredNote is the note for --trusted on a repository this run did
// not register: the flag sets trust only at registration, so an untrusted
// registered repository stays untrusted and the operator is told how to trust
// it. It is empty when the flag was not passed or the repo is already trusted.
func trustedIgnoredNote(trusted bool, repo *pb.Repo) string {
	if !trusted || repo.GetIsTrusted() {
		return ""
	}
	return "Repository already registered; --trusted applies only to a new registration. Run " + trustRepoCommand(repo.GetId()) + " to trust it.\n"
}

// trustRepoCommand is the backticked command that trusts a registered
// repository, shared by every note that names the trust remedy.
func trustRepoCommand(id string) string {
	return "`boss repo update " + id + " --trusted`"
}
