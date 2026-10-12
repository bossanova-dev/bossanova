package projectconfig

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"google.golang.org/protobuf/proto"
)

// FactoryClassifier recognises a live cron job as a factory stage. ok is true
// only when the job's prompt is exactly that stage's prompt for the job's
// agent; anything else exports as a plain cron.
type FactoryClassifier interface {
	Classify(job *pb.CronJob) (stage string, ok bool)
}

// ExportInput is the live state Export turns into a file.
type ExportInput struct {
	// Project is an explicit slug; empty falls back to ExistingProject, then
	// to the repository name from the canonical origin URL.
	Project         string
	ExistingProject string
	Repo            *pb.Repo
	CronJobs        []*pb.CronJob
	// Daemon is the local daemon; an empty ID exports a name-only entry.
	Daemon       DaemonIdentity
	CheckoutPath string
	HomeDir      string
	Classifier   FactoryClassifier
	// Cloud is nil when cloud state was not requested.
	Cloud *CloudInput
}

// CloudInput is the organization-scoped state export reads with a login.
type CloudInput struct {
	OrganizationID string
	Triggers       []*pb.Trigger
	Webhooks       []*pb.SessionWebhook
}

// defaultDaemonName names the daemon entry when no display name is known.
const defaultDaemonName = "local"

// Export converts live state into a Config. Secrets appear only as BOSS_*
// references, and only when the live value is set; their values are never
// read beyond emptiness. The result always validates; Export fails instead of
// returning a file Decode or Validate would reject.
func Export(in ExportInput) (*Config, []string, error) {
	if in.Repo == nil {
		return nil, nil, errors.New("export: no repository")
	}
	origin := vcs.CanonicalRepoURL(in.Repo.GetOriginUrl())
	if origin == "" {
		return nil, nil, errors.New("export: the repository has no usable origin URL")
	}
	var warnings []string
	project, err := exportProject(in, origin)
	if err != nil {
		return nil, nil, err
	}
	cfg := &Config{
		Version: SchemaVersion,
		Project: project,
		Repo:    exportRepo(in.Repo, origin),
	}

	daemon := Daemon{Name: strings.TrimSpace(in.Daemon.DisplayName)}
	if daemon.Name == "" {
		daemon.Name = defaultDaemonName
		warnings = append(warnings, fmt.Sprintf("daemons: the local daemon's display name is unknown; the entry is named %q", defaultDaemonName))
	}
	if id := strings.TrimSpace(in.Daemon.ID); id != "" {
		daemon.DaemonID = ptr(id)
	} else {
		warnings = append(warnings, fmt.Sprintf("daemons[%s]: the local daemon id is unknown; the entry will match daemons by name only", daemon.Name))
	}
	if p := homeRelative(in.CheckoutPath, in.HomeDir); p != "" {
		daemon.Path = ptr(p)
	}
	cfg.Daemons = []Daemon{daemon}

	cfg.Crons = []Cron{}
	for _, job := range in.CronJobs {
		c, w := ExportCron(job, in.Classifier)
		cfg.Crons = append(cfg.Crons, c)
		warnings = append(warnings, w...)
	}

	if in.Cloud != nil {
		cloud := &Cloud{Triggers: []Trigger{}, Webhooks: []Webhook{}}
		if org := strings.TrimSpace(in.Cloud.OrganizationID); org != "" {
			cloud.Organization = ptr(org)
		}
		for _, t := range in.Cloud.Triggers {
			if vcs.CanonicalRepoURL(t.GetRepoOriginUrl()) != origin {
				continue
			}
			tr, w := ExportTrigger(t, cfg.Daemons)
			cloud.Triggers = append(cloud.Triggers, tr)
			warnings = append(warnings, w...)
		}
		for _, wh := range in.Cloud.Webhooks {
			out, w := ExportWebhook(wh)
			cloud.Webhooks = append(cloud.Webhooks, out)
			warnings = append(warnings, w...)
		}
		cfg.Cloud = cloud
	}

	if err := Validate(cfg); err != nil {
		return nil, warnings, fmt.Errorf("export: live state does not form a valid project config:\n%w", err)
	}
	return cfg, warnings, nil
}

func exportProject(in ExportInput, origin string) (string, error) {
	switch {
	case in.Project != "":
		if !ValidProject(in.Project) {
			return "", fmt.Errorf("export: invalid project %q: want %s", in.Project, projectSlug.String())
		}
		return in.Project, nil
	case ValidProject(in.ExistingProject):
		return in.ExistingProject, nil
	}
	return Slugify(origin[strings.LastIndex(origin, "/")+1:]), nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a repository name into a project slug.
func Slugify(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	if s == "" {
		return "project"
	}
	return s
}

func exportRepo(r *pb.Repo, origin string) Repo {
	out := Repo{OriginURL: origin}
	if v := r.GetDisplayName(); v != "" {
		out.DisplayName = ptr(v)
	}
	if v := r.GetDefaultBaseBranch(); v != "" {
		out.DefaultBaseBranch = ptr(v)
	}
	if r.SetupScript != nil {
		out.SetupScript = ptr(r.GetSetupScript())
	}
	if v := r.GetSentryOrg(); v != "" {
		out.SentryOrg = ptr(v)
	}
	settings := &RepoSettings{
		CanAutoMerge:                    ptr(r.GetCanAutoMerge()),
		CanAutoMergeDependabot:          ptr(r.GetCanAutoMergeDependabot()),
		CanAutoRepair:                   ptr(r.GetCanAutoRepair()),
		ShouldArchiveSessionsAfterMerge: ptr(r.GetShouldArchiveSessionsAfterMerge()),
		CanAutoDeleteBranches:           ptr(r.GetCanAutoDeleteBranches()),
		ShouldKeepBranchesCurrent:       ptr(r.GetShouldKeepBranchesCurrent()),
	}
	if v := r.GetMergeStrategy(); v != "" {
		settings.MergeStrategy = ptr(v)
	}
	out.Settings = settings
	// Only emptiness is read: the value itself is never copied anywhere.
	var secrets RepoSecrets
	if r.GetLinearApiKey() != "" {
		secrets.LinearAPIKey = &SecretRef{Env: LinearAPIKeyEnv}
	}
	if r.GetSentryApiKey() != "" {
		secrets.SentryAuthToken = &SecretRef{Env: SentryAuthTokenEnv}
	}
	if secrets != (RepoSecrets{}) {
		out.Secrets = &secrets
	}
	return out
}

// homeRelative renders p with the home directory as ~/, or "" for no path.
func homeRelative(p, home string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if home != "" {
		if rel, err := filepath.Rel(filepath.Clean(home), p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(p)
}

// EffectiveAgent treats a job's empty agent as claude, as the daemon does.
// It is the single copy of that rule: the CLI's factory classifier matches
// prompts with it, so export and classification cannot drift apart.
func EffectiveAgent(name string) string {
	if name == "" {
		return "claude"
	}
	return name
}

// ExportCron converts one live job. A job the classifier recognises exports
// symbolically (no prompt, no gate); any other job exports as a plain cron,
// and a gate command that is not portable is dropped with a warning.
func ExportCron(job *pb.CronJob, classifier FactoryClassifier) (Cron, []string) {
	var warnings []string
	out := Cron{
		Name:     job.GetName(),
		Agent:    ptr(EffectiveAgent(job.GetAgentName())),
		Schedule: ptr(job.GetSchedule()),
	}
	stage, factory := "", false
	if classifier != nil {
		stage, factory = classifier.Classify(job)
	}
	if factory {
		out.FactoryStage = ptr(stage)
	} else {
		out.Prompt = ptr(job.GetPrompt())
		if gate := strings.TrimSpace(job.GetGateCommand()); gate != "" {
			if portableGateCommand(gate) {
				out.GateCommand = ptr(gate)
			} else {
				warnings = append(warnings, fmt.Sprintf("crons[%s].gateCommand: not exported; it may embed host paths (only ./<path> or node <path> with a repo-relative path is portable), so an apply would run this cron ungated", job.GetName()))
			}
		}
	}
	if v := job.GetModel(); v != "" {
		out.Model = ptr(v)
	}
	if v := job.GetTimezone(); v != "" {
		out.Timezone = ptr(v)
	}
	if !job.GetShouldRunSetupCommand() {
		out.RunSetupCommand = ptr(false)
	}
	if job.GetIsZeroOutput() {
		out.ZeroOutput = ptr(true)
	}
	switch job.GetConcurrencyPolicy() {
	case pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS:
		out.ConcurrencyPolicy = ptr("cancel-in-progress")
	case pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT:
		out.ConcurrencyPolicy = ptr("allow-concurrent")
	default:
		// SKIP is the default, so it is not written.
	}
	if !job.GetIsEnabled() {
		out.Enabled = ptr(false)
	}
	return out, warnings
}

// ExportTrigger converts one live trigger. A trigger pinned to a daemon that
// a daemons entry pins by id names that entry; any other pin keeps the id.
// Output-only fields (ids, endpoint, config version, last invocation) are
// dropped; triggers carry no secret.
func ExportTrigger(t *pb.Trigger, daemons []Daemon) (Trigger, []string) {
	var warnings []string
	out := Trigger{
		Name:   t.GetName(),
		Type:   t.GetTriggerType(),
		Launch: NewProtoObject(cloneOrNew(t.GetLaunch())),
	}
	if c := t.GetHttp(); c != nil {
		out.HTTP = NewProtoObject(proto.CloneOf(c))
	}
	if c := t.GetGithub(); c != nil {
		out.GitHub = NewProtoObject(proto.CloneOf(c))
	}
	for _, f := range t.GetFilters() {
		out.Filters = append(out.Filters, Filter{Msg: proto.CloneOf(f)})
	}
	if len(t.GetPayloadFields()) > 0 {
		out.PayloadFields = append([]string(nil), t.GetPayloadFields()...)
	}
	if g := t.GetGate(); g != nil {
		out.Gate = NewProtoObject(proto.CloneOf(g))
	}
	switch t.GetPlacement().GetMode() {
	case pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE:
		out.Placement = Placement{Mode: PlacementFirstAvailable}
	case pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON:
		out.Placement = Placement{Mode: PlacementSpecificDaemon}
		id := t.GetPlacement().GetDaemonId()
		if name, ok := daemonNameForID(daemons, id); ok {
			out.Placement.Daemon = ptr(name)
		} else {
			out.Placement.DaemonID = ptr(id)
		}
	default:
		out.Placement = Placement{Mode: enumSpelling(t.GetPlacement().GetMode().String(), "TRIGGER_PLACEMENT_MODE_")}
		warnings = append(warnings, fmt.Sprintf("cloud.triggers[%s].placement: unrecognised mode %q exported as-is", t.GetName(), out.Placement.Mode))
	}
	switch p := t.GetConcurrencyPolicy(); p {
	case pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_UNSPECIFIED,
		pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING:
	default:
		out.ConcurrencyPolicy = ptr(enumSpelling(p.String(), "TRIGGER_CONCURRENCY_POLICY_"))
	}
	if v := t.GetCooldownSeconds(); v != 0 {
		out.CooldownSeconds = ptr(v)
	}
	if !t.GetIsEnabled() {
		out.Enabled = ptr(false)
	}
	return out, warnings
}

// ExportWebhook converts one live session webhook. Its signing secret is
// never exported: bosso cannot reveal it, and apply uses one only on create.
func ExportWebhook(w *pb.SessionWebhook) (Webhook, []string) {
	var warnings []string
	out := Webhook{URL: w.GetUrl(), EventTypes: append([]string{}, w.GetEventTypes()...)}
	if v := w.GetDescription(); v != "" {
		out.Description = ptr(v)
	}
	if !w.GetIsEnabled() {
		out.Enabled = ptr(false)
	}
	if webhookURLMayCarryCredential(w.GetUrl()) {
		warnings = append(warnings, fmt.Sprintf("cloud.webhooks[%s].url: carries userinfo or a query string, which may embed a credential; review the file before committing it", webhookHost(w.GetUrl())))
	}
	if f := w.GetFormat(); f != "" && f != "raw" {
		warnings = append(warnings, fmt.Sprintf("cloud.webhooks[%s].format: %q is not part of the v1 file and was not exported", w.GetUrl(), f))
	}
	return out, warnings
}

func daemonNameForID(daemons []Daemon, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	for _, d := range daemons {
		if d.DaemonID != nil && *d.DaemonID == id {
			return d.Name, true
		}
	}
	return "", false
}

// enumSpelling turns FOO_BAR_BAZ (after prefix) into bar-baz form.
func enumSpelling(name, prefix string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, prefix)), "_", "-")
}

func cloneOrNew[T any, PT interface {
	*T
	proto.Message
}](m PT) PT {
	if m == nil {
		return PT(new(T))
	}
	return proto.CloneOf(m)
}

// webhookURLMayCarryCredential reports whether a webhook URL has userinfo or
// a query string, the two places a credential is commonly embedded. The URL is
// the webhook's identity, so export keeps it and warns instead of redacting.
func webhookURLMayCarryCredential(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.User != nil || u.RawQuery != ""
}

// webhookHost names a webhook in a warning without repeating any part of the
// URL that may hold a credential.
func webhookHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "?"
	}
	return u.Scheme + "://" + u.Host
}
