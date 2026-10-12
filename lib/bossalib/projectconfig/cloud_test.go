package projectconfig

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"google.golang.org/protobuf/proto"
)

const (
	cloudMarker = "boss-project:widget"
	cloudOrg    = "org-1"
	cloudOrigin = "https://github.com/acme/widget"
)

// cloudConfig decodes a one-daemon project file with the given cloud section.
func cloudConfig(t *testing.T, cloud string) *Config {
	t.Helper()
	cfg, _ := mustDecode(t, []byte(`{"version": 1, "project": "widget",
  "repo": {"originUrl": "git@github.com:acme/widget.git"},
  "daemons": [{"name": "laptop"}, {"name": "server", "daemonId": "d-server"}],
  "cloud": `+cloud+`}`))
	if err := Validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return cfg
}

// fakeCloud is an in-memory bosso: it stores triggers and webhooks the way
// the server does (defaults filled, marker echoed), records every call in one
// ordered log shared with the secret sink, and fails the calls failOn names.
type fakeCloud struct {
	triggers []*pb.Trigger
	webhooks []*pb.SessionWebhook
	daemons  []*pb.DaemonInfo

	// The remote-cron fleet (cloud_crons_test.go): own daemons, peer refs,
	// aggregated repos and every daemon's cron rows. oldDaemons drop the
	// marker on a cron write.
	own        []*pb.DaemonInfo
	peers      []*pb.DaemonRef
	repos      []*pb.AggregatedRepo
	cronRows   []*pb.CronJobWithDaemon
	oldDaemons map[string]bool

	listTriggersErr error
	listWebhooksErr error
	listDaemonsErr  error
	failOn          map[string]error
	dropMarker      bool

	log    []string
	nextID int
}

func (f *fakeCloud) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeCloud) call(entry string) error {
	f.log = append(f.log, entry)
	return f.failOn[entry]
}

// writes is the log without reads and sink entries.
func (f *fakeCloud) writes() []string {
	var out []string
	for _, e := range f.log {
		if !strings.HasPrefix(e, "list-") && !strings.HasPrefix(e, "secret ") {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeCloud) Secret(label, value string) {
	f.log = append(f.log, "secret "+label+"="+value)
}

func (f *fakeCloud) ListTriggers(_ context.Context, req *pb.ListTriggersRequest) ([]*pb.Trigger, error) {
	f.log = append(f.log, "list-triggers "+req.GetOrganizationId())
	if f.listTriggersErr != nil {
		return nil, f.listTriggersErr
	}
	out := make([]*pb.Trigger, 0, len(f.triggers))
	for _, t := range f.triggers {
		out = append(out, proto.CloneOf(t))
	}
	return out, nil
}

func (f *fakeCloud) ListSessionWebhooks(_ context.Context, org string) ([]*pb.SessionWebhook, error) {
	f.log = append(f.log, "list-webhooks "+org)
	if f.listWebhooksErr != nil {
		return nil, f.listWebhooksErr
	}
	out := make([]*pb.SessionWebhook, 0, len(f.webhooks))
	for _, w := range f.webhooks {
		out = append(out, proto.CloneOf(w))
	}
	return out, nil
}

func (f *fakeCloud) ListDaemons(_ context.Context, org string) ([]*pb.DaemonInfo, error) {
	f.log = append(f.log, "list-daemons "+org)
	return f.daemons, f.listDaemonsErr
}

func (f *fakeCloud) marker(m string) string {
	if f.dropMarker {
		return ""
	}
	return m
}

func (f *fakeCloud) CreateTrigger(_ context.Context, req *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error) {
	if err := f.call("create-trigger " + req.GetName()); err != nil {
		return nil, err
	}
	t := &pb.Trigger{Id: f.id("tr"), OrganizationId: req.GetOrganizationId(), Name: req.GetName(), IsEnabled: req.GetIsEnabled(),
		TriggerType: req.GetTriggerType(), RepoOriginUrl: vcs.CanonicalRepoURL(req.GetRepoOriginUrl()), Launch: storedLaunch(nil, req.GetLaunch()),
		Placement: req.GetPlacement(), ConcurrencyPolicy: req.GetConcurrencyPolicy(), CooldownSeconds: req.GetCooldownSeconds(),
		Filters: req.GetFilters(), PayloadFields: req.GetPayloadFields(), ManagedBy: f.marker(req.GetManagedBy())}
	if g := req.GetGate(); g.GetScriptPath() != "" {
		t.Gate = g
	}
	if t.ConcurrencyPolicy == pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_UNSPECIFIED {
		t.ConcurrencyPolicy = pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING
	}
	resp := &pb.CreateTriggerResponse{}
	switch cfg := req.GetTypeConfig().(type) {
	case *pb.CreateTriggerRequest_Http:
		t.TypeConfig = &pb.Trigger_Http{Http: httpDefaults(cfg.Http)}
		resp.Secret = "minted-" + req.GetName()
	case *pb.CreateTriggerRequest_Github:
		t.TypeConfig = &pb.Trigger_Github{Github: cfg.Github}
	}
	f.triggers = append(f.triggers, t)
	resp.Trigger = proto.CloneOf(t)
	return resp, nil
}

// storedLaunch is the launch bosso stores: an absent launch option keeps the
// stored value (or its create default), and both are always present on read.
func storedLaunch(stored, update *pb.TriggerLaunchSettings) *pb.TriggerLaunchSettings {
	out := cloneOrNew(update)
	if out.ShouldRunSetupCommand == nil {
		out.ShouldRunSetupCommand = proto.Bool(stored == nil || stored.GetShouldRunSetupCommand())
	}
	if out.IsZeroOutput == nil {
		out.IsZeroOutput = proto.Bool(stored != nil && stored.GetIsZeroOutput())
	}
	return out
}

func (f *fakeCloud) trigger(id string) *pb.Trigger {
	for _, t := range f.triggers {
		if t.GetId() == id {
			return t
		}
	}
	return nil
}

func (f *fakeCloud) UpdateTrigger(_ context.Context, req *pb.UpdateTriggerRequest) (*pb.Trigger, error) {
	t := f.trigger(req.GetId())
	verb := "update-trigger "
	if req.IsEnabled != nil && !req.GetIsEnabled() && req.ManagedBy == nil && req.Launch == nil {
		verb = "disable-trigger "
	}
	if err := f.call(verb + t.GetName()); err != nil {
		return nil, err
	}
	if req.Name != nil {
		t.Name = req.GetName()
	}
	if req.IsEnabled != nil {
		t.IsEnabled = req.GetIsEnabled()
	}
	switch cfg := req.GetTypeConfig().(type) {
	case *pb.UpdateTriggerRequest_Http:
		t.TypeConfig = &pb.Trigger_Http{Http: httpDefaults(cfg.Http)}
	case *pb.UpdateTriggerRequest_Github:
		t.TypeConfig = &pb.Trigger_Github{Github: cfg.Github}
	}
	if req.RepoOriginUrl != nil {
		t.RepoOriginUrl = req.GetRepoOriginUrl()
	}
	if req.Launch != nil {
		t.Launch = storedLaunch(t.Launch, req.Launch)
	}
	if req.Placement != nil {
		t.Placement = req.Placement
	}
	if req.ConcurrencyPolicy != nil {
		t.ConcurrencyPolicy = req.GetConcurrencyPolicy()
	}
	if req.CooldownSeconds != nil {
		t.CooldownSeconds = req.GetCooldownSeconds()
	}
	if req.ShouldClearFilters {
		t.Filters = nil
	} else if len(req.Filters) > 0 {
		t.Filters = req.Filters
	}
	if req.ShouldClearPayloadFields {
		t.PayloadFields = nil
	} else if len(req.PayloadFields) > 0 {
		t.PayloadFields = req.PayloadFields
	}
	if req.Gate != nil {
		t.Gate = req.Gate
		if req.Gate.GetScriptPath() == "" {
			t.Gate = nil
		}
	}
	if req.ManagedBy != nil {
		t.ManagedBy = f.marker(req.GetManagedBy())
	}
	out := proto.CloneOf(t)
	if f.dropMarker {
		out.ManagedBy = ""
	}
	return out, nil
}

func (f *fakeCloud) DeleteTrigger(_ context.Context, id string) error {
	t := f.trigger(id)
	if err := f.call("delete-trigger " + t.GetName()); err != nil {
		return err
	}
	f.triggers = slices.DeleteFunc(f.triggers, func(x *pb.Trigger) bool { return x.GetId() == id })
	return nil
}

func (f *fakeCloud) RotateTriggerSecret(_ context.Context, id string) (*pb.RotateTriggerSecretResponse, error) {
	t := f.trigger(id)
	if err := f.call("rotate-trigger " + t.GetName()); err != nil {
		return nil, err
	}
	return &pb.RotateTriggerSecretResponse{Trigger: proto.CloneOf(t), Secret: "rotated-" + t.GetName()}, nil
}

func (f *fakeCloud) webhook(id string) *pb.SessionWebhook {
	for _, w := range f.webhooks {
		if w.GetId() == id {
			return w
		}
	}
	return nil
}

func (f *fakeCloud) CreateSessionWebhook(_ context.Context, req *pb.CreateSessionWebhookRequest) (*pb.CreateSessionWebhookResponse, error) {
	if err := f.call("create-webhook " + req.GetUrl()); err != nil {
		return nil, err
	}
	w := &pb.SessionWebhook{Id: f.id("wh"), OrganizationId: req.GetOrganizationId(), Url: strings.TrimSpace(req.GetUrl()), Description: req.GetDescription(),
		EventTypes: sortedStrings(req.GetEventTypes()), IsEnabled: req.GetIsEnabled(), ManagedBy: f.marker(req.GetManagedBy()), Format: "raw"}
	f.webhooks = append(f.webhooks, w)
	secret := req.GetSecret()
	if req.Secret == nil {
		secret = "minted-webhook"
	}
	return &pb.CreateSessionWebhookResponse{Webhook: proto.CloneOf(w), Secret: secret}, nil
}

func (f *fakeCloud) UpdateSessionWebhook(_ context.Context, req *pb.UpdateSessionWebhookRequest) (*pb.SessionWebhook, error) {
	w := f.webhook(req.GetId())
	verb := "update-webhook "
	if req.IsEnabled != nil && !req.GetIsEnabled() && req.ManagedBy == nil && req.Description == nil {
		verb = "disable-webhook "
	}
	if err := f.call(verb + w.GetUrl()); err != nil {
		return nil, err
	}
	if req.Description != nil {
		w.Description = req.GetDescription()
	}
	if req.GetShouldReplaceEventTypes() {
		w.EventTypes = sortedStrings(req.GetEventTypes())
	}
	if req.IsEnabled != nil {
		w.IsEnabled = req.GetIsEnabled()
	}
	if req.ManagedBy != nil {
		w.ManagedBy = f.marker(req.GetManagedBy())
	}
	return proto.CloneOf(w), nil
}

func (f *fakeCloud) DeleteSessionWebhook(_ context.Context, org, id string) error {
	w := f.webhook(id)
	if err := f.call("delete-webhook " + w.GetUrl() + " in " + org); err != nil {
		return err
	}
	f.webhooks = slices.DeleteFunc(f.webhooks, func(x *pb.SessionWebhook) bool { return x.GetId() == id })
	return nil
}

func (f *fakeCloud) RotateSessionWebhookSecret(_ context.Context, req *pb.RotateSessionWebhookSecretRequest) (*pb.RotateSessionWebhookSecretResponse, error) {
	w := f.webhook(req.GetId())
	if err := f.call("rotate-webhook " + w.GetUrl()); err != nil {
		return nil, err
	}
	secret := req.GetSecret()
	if req.Secret == nil {
		secret = "rotated-webhook"
	}
	return &pb.RotateSessionWebhookSecretResponse{Webhook: proto.CloneOf(w), Secret: secret}, nil
}

var (
	_ CloudApplyReader = (*fakeCloud)(nil)
	_ CloudWriter      = (*fakeCloud)(nil)
	_ SecretSink       = (*fakeCloud)(nil)
)

// planOf observes f and plans cfg against it.
func planOf(t *testing.T, cfg *Config, f *fakeCloud, opts CloudOptions) CloudPlan {
	t.Helper()
	if opts.Org == "" {
		opts.Org = cloudOrg
	}
	st, err := ObserveCloud(context.Background(), cfg, f, opts)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	return PlanCloud(cfg, st, opts)
}

// changeOf returns the plan's one change for resource and identity.
func changeOf(t *testing.T, p CloudPlan, r Resource, identity string, action Action) Change {
	t.Helper()
	var found []Change
	for _, c := range p.Changes {
		if c.Resource == r && c.Identity == identity && (action == "" || c.Action == action) {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s %s %q change, got %+v\nplan:\n%s", action, r, identity, found, p.Text())
	}
	return found[0]
}

// opFor returns the write applying the plan's change for identity.
func opFor(t *testing.T, p CloudPlan, identity string, rank int) cloudOp {
	t.Helper()
	for _, op := range p.ops {
		if p.Changes[op.change].Identity == identity && op.rank == rank {
			return op
		}
	}
	t.Fatalf("no write for %q at rank %d", identity, rank)
	return cloudOp{}
}

const httpTrigger = `{"name": "deploy", "type": "http", "launch": {"promptTemplate": "Deploy {{event.type}}"}, "placement": {"mode": "first-available"}}`

// liveTrigger is a trigger as bosso returns it after creating httpTrigger.
func liveTrigger(name, marker, origin string) *pb.Trigger {
	return &pb.Trigger{Id: "tr-" + name, OrganizationId: cloudOrg, Name: name, IsEnabled: true, TriggerType: "http",
		TypeConfig:        &pb.Trigger_Http{Http: &pb.HttpTriggerConfig{AllowedMethods: []string{"POST"}, IdempotencyHeader: "Idempotency-Key", DedupWindowSeconds: 300}},
		RepoOriginUrl:     origin,
		Launch:            &pb.TriggerLaunchSettings{PromptTemplate: "Deploy {{event.type}}", ShouldRunSetupCommand: proto.Bool(true), IsZeroOutput: proto.Bool(false)},
		Placement:         &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE},
		ConcurrencyPolicy: pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING, ManagedBy: marker}
}

func liveWebhook(url, marker string) *pb.SessionWebhook {
	return &pb.SessionWebhook{Id: "wh-" + url, OrganizationId: cloudOrg, Url: url, EventTypes: []string{"session.merged"}, IsEnabled: true, ManagedBy: marker, Format: "raw"}
}

func TestPlanCloud(t *testing.T) {
	mint := CloudOptions{CanMintSecrets: true}

	t.Run("trigger with no candidate is created with the marker and the file's origin", func(t *testing.T) {
		p := planOf(t, cloudConfig(t, `{"triggers": [`+httpTrigger+`]}`), &fakeCloud{}, mint)
		c := changeOf(t, p, ResourceTrigger, "deploy", ActionCreate)
		if !c.NeedsConfirmation {
			t.Fatal("a trigger create must need confirmation")
		}
		req := opFor(t, p, "deploy", rankWrite).createTrigger
		if req.GetManagedBy() != cloudMarker || req.GetRepoOriginUrl() != cloudOrigin || req.GetOrganizationId() != cloudOrg {
			t.Fatalf("create request %+v", req)
		}
		if !req.GetIsEnabled() || !req.GetLaunch().GetShouldRunSetupCommand() || req.GetLaunch().IsZeroOutput == nil {
			t.Fatalf("create must state every default: %+v", req)
		}
	})

	t.Run("own row equal through the export conversion is unchanged", func(t *testing.T) {
		f := &fakeCloud{triggers: []*pb.Trigger{liveTrigger("deploy", cloudMarker, cloudOrigin)}}
		p := planOf(t, cloudConfig(t, `{"triggers": [`+httpTrigger+`]}`), f, mint)
		changeOf(t, p, ResourceTrigger, "deploy", ActionUnchanged)
		if p.HasWrites() || !p.Converged() {
			t.Fatalf("plan writes:\n%s", p.Text())
		}
		// Export, then plan the exported file: still converged.
		exported, _ := ExportTrigger(f.triggers[0], nil)
		cfg := cloudConfig(t, `{"triggers": []}`)
		cfg.Cloud.Triggers = []Trigger{exported}
		if p := planOf(t, cfg, f, mint); !p.Converged() {
			t.Fatalf("exported file does not converge:\n%s", p.Text())
		}
	})

	t.Run("one differing launch field is an update carrying the whole launch", func(t *testing.T) {
		live := liveTrigger("deploy", cloudMarker, cloudOrigin)
		live.Launch.SkillName = "boss-build"
		live.Launch.IsZeroOutput = proto.Bool(true)
		f := &fakeCloud{triggers: []*pb.Trigger{live}}
		p := planOf(t, cloudConfig(t, `{"triggers": [{"name": "deploy", "type": "http", "launch": {"promptTemplate": "Deploy {{event.type}}", "skillName": "boss-plan", "isZeroOutput": true}, "placement": {"mode": "first-available"}}]}`), f, mint)
		c := changeOf(t, p, ResourceTrigger, "deploy", ActionUpdate)
		if len(c.Fields) != 1 || c.Fields[0].Field != "launch" || c.NeedsConfirmation {
			t.Fatalf("fields %+v confirm %v; want one launch diff that leaves the prompt alone", c.Fields, c.NeedsConfirmation)
		}
		req := opFor(t, p, "deploy", rankWrite).updateTrigger
		want := &pb.TriggerLaunchSettings{PromptTemplate: "Deploy {{event.type}}", SkillName: "boss-plan", ShouldRunSetupCommand: proto.Bool(true), IsZeroOutput: proto.Bool(true)}
		if !proto.Equal(req.GetLaunch(), want) || req.ManagedBy != nil || req.Placement != nil || req.TypeConfig != nil {
			t.Fatalf("update request %+v, want the whole launch %+v and nothing else", req, want)
		}
		// A prompt change needs confirmation.
		p = planOf(t, cloudConfig(t, `{"triggers": [{"name": "deploy", "type": "http", "launch": {"promptTemplate": "New prompt"}, "placement": {"mode": "first-available"}}]}`), &fakeCloud{triggers: []*pb.Trigger{liveTrigger("deploy", cloudMarker, cloudOrigin)}}, mint)
		if c := changeOf(t, p, ResourceTrigger, "deploy", ActionUpdate); !c.NeedsConfirmation {
			t.Fatal("a launch prompt change must need confirmation")
		}
	})

	t.Run("unmarked same-name row on this origin is adopted", func(t *testing.T) {
		f := &fakeCloud{triggers: []*pb.Trigger{liveTrigger("deploy", "", "https://github.com/acme/widget.git")}}
		p := planOf(t, cloudConfig(t, `{"triggers": [`+httpTrigger+`]}`), f, mint)
		changeOf(t, p, ResourceTrigger, "deploy", ActionAdopt)
		req := opFor(t, p, "deploy", rankWrite).updateTrigger
		if req.GetManagedBy() != cloudMarker || req.GetId() != "tr-deploy" {
			t.Fatalf("adopt request %+v", req)
		}
	})

	t.Run("unmarked same-name row on another origin is ignored", func(t *testing.T) {
		f := &fakeCloud{triggers: []*pb.Trigger{liveTrigger("deploy", "", "https://github.com/acme/other")}}
		p := planOf(t, cloudConfig(t, `{"triggers": [`+httpTrigger+`]}`), f, mint)
		changeOf(t, p, ResourceTrigger, "deploy", ActionCreate)
		if len(p.Changes) != 1 {
			t.Fatalf("the other repo's trigger appeared in the plan:\n%s", p.Text())
		}
	})

	t.Run("another project's marker is a conflict", func(t *testing.T) {
		f := &fakeCloud{triggers: []*pb.Trigger{liveTrigger("deploy", "boss-project:other", "https://github.com/acme/other")}}
		p := planOf(t, cloudConfig(t, `{"triggers": [`+httpTrigger+`]}`), f, mint)
		if c := changeOf(t, p, ResourceTrigger, "deploy", ActionConflict); c.Reason != ReasonManagedByOther {
			t.Fatalf("reason %q", c.Reason)
		}
		if p.HasWrites() {
			t.Fatal("a conflict must not write")
		}
	})

	t.Run("own row dropped is disabled, and deleted only under prune", func(t *testing.T) {
		live := []*pb.Trigger{liveTrigger("old", cloudMarker, cloudOrigin), liveTrigger("hand-made", "", cloudOrigin), liveTrigger("theirs", "boss-project:other", cloudOrigin)}
		cfg := cloudConfig(t, `{"triggers": []}`)
		p := planOf(t, cfg, &fakeCloud{triggers: live}, mint)
		changeOf(t, p, ResourceTrigger, "old", ActionDisable)
		if req := opFor(t, p, "old", rankDisable).updateTrigger; req.GetIsEnabled() || req.IsEnabled == nil || req.ManagedBy != nil {
			t.Fatalf("disable request %+v", req)
		}
		if len(p.Changes) != 1 {
			t.Fatalf("unmarked or foreign rows appeared:\n%s", p.Text())
		}
		p = planOf(t, cfg, &fakeCloud{triggers: live}, CloudOptions{CanMintSecrets: true, Prune: true})
		changeOf(t, p, ResourceTrigger, "old", ActionDelete)
		if id := opFor(t, p, "old", rankDelete).deleteTrigger; id != "tr-old" {
			t.Fatalf("delete %q", id)
		}
		if len(p.Changes) != 1 {
			t.Fatalf("prune touched unmarked or foreign rows:\n%s", p.Text())
		}
	})

	t.Run("filters removed in the file clear them", func(t *testing.T) {
		live := liveTrigger("deploy", cloudMarker, cloudOrigin)
		live.Filters = []*pb.TriggerFilter{{Field: "body.ref", Operator: pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS, Values: []string{"main"}}}
		live.PayloadFields = []string{"body.ref"}
		live.Gate = &pb.TriggerGate{ScriptPath: "scripts/gate.sh"}
		p := planOf(t, cloudConfig(t, `{"triggers": [`+httpTrigger+`]}`), &fakeCloud{triggers: []*pb.Trigger{live}}, mint)
		c := changeOf(t, p, ResourceTrigger, "deploy", ActionUpdate)
		req := opFor(t, p, "deploy", rankWrite).updateTrigger
		if !req.GetShouldClearFilters() || len(req.GetFilters()) != 0 || !req.GetShouldClearPayloadFields() || req.Gate == nil || req.GetGate().GetScriptPath() != "" {
			t.Fatalf("update request %+v; want clear flags and an empty gate", req)
		}
		if !c.NeedsConfirmation {
			t.Fatal("a gate change must need confirmation")
		}
	})

	t.Run("placement resolves through the caller's daemons", func(t *testing.T) {
		placed := func(daemon string) string {
			return `{"triggers": [{"name": "deploy", "type": "github", "github": {"eventTypes": ["push"]}, "launch": {"promptTemplate": "go"}, "placement": {"mode": "specific-daemon", "daemon": "` + daemon + `"}}]}`
		}
		f := &fakeCloud{daemons: []*pb.DaemonInfo{{DaemonId: "d-laptop", Hostname: "laptop"}, {DaemonId: "d-server", Hostname: "srv"}}}
		p := planOf(t, cloudConfig(t, placed("laptop")), f, mint)
		changeOf(t, p, ResourceTrigger, "deploy", ActionCreate)
		if got := opFor(t, p, "deploy", rankWrite).createTrigger.GetPlacement().GetDaemonId(); got != "d-laptop" {
			t.Fatalf("by name resolved to %q", got)
		}
		p = planOf(t, cloudConfig(t, placed("server")), f, mint)
		if got := opFor(t, p, "deploy", rankWrite).createTrigger.GetPlacement().GetDaemonId(); got != "d-server" {
			t.Fatalf("by pinned id resolved to %q", got)
		}

		unmatched := &fakeCloud{daemons: []*pb.DaemonInfo{{DaemonId: "d-other", Hostname: "elsewhere"}}}
		p = planOf(t, cloudConfig(t, placed("laptop")), unmatched, mint)
		if c := changeOf(t, p, ResourceTrigger, "deploy", ActionSkip); c.Reason != ReasonDaemonUnmatched {
			t.Fatalf("reason %q", c.Reason)
		}
		twice := &fakeCloud{daemons: []*pb.DaemonInfo{{DaemonId: "d-1", Hostname: "laptop"}, {DaemonId: "d-2", Hostname: "laptop"}}}
		p = planOf(t, cloudConfig(t, placed("laptop")), twice, mint)
		if c := changeOf(t, p, ResourceTrigger, "deploy", ActionConflict); c.Reason != ReasonAmbiguous {
			t.Fatalf("reason %q", c.Reason)
		}
		failing := &fakeCloud{listDaemonsErr: errors.New("boom")}
		p = planOf(t, cloudConfig(t, placed("laptop")), failing, mint)
		if c := changeOf(t, p, ResourceTrigger, "deploy", ActionSkip); c.Reason != ReasonDaemonUnmatched || !strings.Contains(c.Detail, "boom") {
			t.Fatalf("change %+v", c)
		}
		// No trigger names a daemons entry: the daemons are never read.
		quiet := &fakeCloud{}
		planOf(t, cloudConfig(t, `{"triggers": [`+httpTrigger+`]}`), quiet, mint)
		if slices.Contains(quiet.log, "list-daemons "+cloudOrg) {
			t.Fatalf("daemons read without a named placement: %v", quiet.log)
		}
	})

	t.Run("an absent enabled is not managed; an explicit one is", func(t *testing.T) {
		live := liveTrigger("deploy", cloudMarker, cloudOrigin)
		live.IsEnabled = false
		p := planOf(t, cloudConfig(t, `{"triggers": [`+httpTrigger+`]}`), &fakeCloud{triggers: []*pb.Trigger{live}}, mint)
		changeOf(t, p, ResourceTrigger, "deploy", ActionUnchanged)
		if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "stays disabled") {
			t.Fatalf("warnings %q", p.Warnings)
		}
		enabled := strings.Replace(httpTrigger, `"name": "deploy",`, `"name": "deploy", "enabled": true,`, 1)
		p = planOf(t, cloudConfig(t, `{"triggers": [`+enabled+`]}`), &fakeCloud{triggers: []*pb.Trigger{live}}, mint)
		changeOf(t, p, ResourceTrigger, "deploy", ActionUpdate)
		if req := opFor(t, p, "deploy", rankWrite).updateTrigger; !req.GetIsEnabled() || req.IsEnabled == nil {
			t.Fatalf("update %+v, want is_enabled set", req)
		}
	})

	t.Run("a trigger whose type changed is a conflict", func(t *testing.T) {
		p := planOf(t, cloudConfig(t, `{"triggers": [{"name": "deploy", "type": "github", "launch": {"promptTemplate": "go"}, "placement": {"mode": "first-available"}}]}`),
			&fakeCloud{triggers: []*pb.Trigger{liveTrigger("deploy", cloudMarker, cloudOrigin)}}, mint)
		if c := changeOf(t, p, ResourceTrigger, "deploy", ActionConflict); c.Reason != ReasonTypeChange || p.HasWrites() {
			t.Fatalf("change %+v writes %v", c, p.HasWrites())
		}
	})

	t.Run("webhooks: adopt, another project's conflict, disable and prune", func(t *testing.T) {
		f := &fakeCloud{webhooks: []*pb.SessionWebhook{
			liveWebhook("https://hooks.example.com/mine", ""),
			liveWebhook("https://hooks.example.com/theirs", "boss-project:other"),
			liveWebhook("https://hooks.example.com/old", cloudMarker),
			liveWebhook("https://hooks.example.com/hand-made", ""),
		}}
		cfg := cloudConfig(t, `{"webhooks": [
		  {"url": "https://hooks.example.com/mine", "eventTypes": ["session.merged", "session.passing"]},
		  {"url": "https://hooks.example.com/theirs", "eventTypes": ["session.merged"]}]}`)
		p := planOf(t, cfg, f, mint)
		c := changeOf(t, p, ResourceWebhook, "https://hooks.example.com/mine", ActionAdopt)
		if len(c.Fields) != 1 || c.Fields[0].Field != "eventTypes" {
			t.Fatalf("adopt fields %+v", c.Fields)
		}
		req := opFor(t, p, "https://hooks.example.com/mine", rankWrite).updateWebhook
		if req.GetManagedBy() != cloudMarker || !req.GetShouldReplaceEventTypes() || len(req.GetEventTypes()) != 2 || req.GetOrganizationId() != cloudOrg {
			t.Fatalf("adopt request %+v", req)
		}
		if c := changeOf(t, p, ResourceWebhook, "https://hooks.example.com/theirs", ActionConflict); c.Reason != ReasonManagedByOther {
			t.Fatalf("reason %q", c.Reason)
		}
		changeOf(t, p, ResourceWebhook, "https://hooks.example.com/old", ActionDisable)
		if len(p.Changes) != 3 {
			t.Fatalf("the hand-made webhook appeared:\n%s", p.Text())
		}
		p = planOf(t, cfg, f, CloudOptions{CanMintSecrets: true, Prune: true})
		changeOf(t, p, ResourceWebhook, "https://hooks.example.com/old", ActionDelete)
		if len(p.Changes) != 3 {
			t.Fatalf("prune touched an unmarked webhook:\n%s", p.Text())
		}
	})

	t.Run("an owner-refused webhook read skips every webhook while triggers still plan", func(t *testing.T) {
		f := &fakeCloud{listWebhooksErr: connect.NewError(connect.CodePermissionDenied, errors.New("organization owner role required"))}
		cfg := cloudConfig(t, `{"triggers": [`+httpTrigger+`], "webhooks": [
		  {"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"]},
		  {"url": "https://hooks.example.com/b", "eventTypes": ["session.merged"]}]}`)
		p := planOf(t, cfg, f, mint)
		for _, url := range []string{"https://hooks.example.com/a", "https://hooks.example.com/b"} {
			if c := changeOf(t, p, ResourceWebhook, url, ActionSkip); c.Reason != ReasonOwnerRequired {
				t.Fatalf("reason %q", c.Reason)
			}
		}
		changeOf(t, p, ResourceTrigger, "deploy", ActionCreate)
		if !p.Blocked() {
			t.Fatal("a plan with skips is blocked")
		}
		// Any other webhook read failure aborts the observation.
		f.listWebhooksErr = connect.NewError(connect.CodeUnavailable, errors.New("down"))
		if _, err := ObserveCloud(context.Background(), cfg, f, CloudOptions{Org: cloudOrg}); err == nil {
			t.Fatal("an unavailable webhook read must fail the observation")
		}
	})

	t.Run("webhook secrets: unresolved skips, resolved creates with it, absent creates without", func(t *testing.T) {
		cfg := cloudConfig(t, `{"webhooks": [
		  {"url": "https://hooks.example.com/ref", "eventTypes": ["session.merged"], "secret": {"env": "BOSS_HOOK_SECRET"}},
		  {"url": "https://hooks.example.com/minted", "eventTypes": ["session.merged"]}]}`)
		p := planOf(t, cfg, &fakeCloud{}, CloudOptions{CanMintSecrets: true, Secrets: EnvResolver(func(string) string { return "" })})
		if c := changeOf(t, p, ResourceWebhook, "https://hooks.example.com/ref", ActionSkip); c.Reason != ReasonSecretUnresolved {
			t.Fatalf("reason %q", c.Reason)
		}
		changeOf(t, p, ResourceWebhook, "https://hooks.example.com/minted", ActionCreate)
		if req := opFor(t, p, "https://hooks.example.com/minted", rankWrite).createWebhook; req.Secret != nil || req.GetManagedBy() != cloudMarker || !req.GetIsEnabled() {
			t.Fatalf("secret-less create %+v", req)
		}

		resolved := EnvResolver(func(k string) string {
			if k == "BOSS_HOOK_SECRET" {
				return "s3cret-value-from-env"
			}
			return ""
		})
		p = planOf(t, cfg, &fakeCloud{}, CloudOptions{CanMintSecrets: true, Secrets: resolved})
		changeOf(t, p, ResourceWebhook, "https://hooks.example.com/ref", ActionCreate)
		if req := opFor(t, p, "https://hooks.example.com/ref", rankWrite).createWebhook; req.GetSecret() != "s3cret-value-from-env" {
			t.Fatal("the resolved secret is not sent on create")
		}
		data, err := p.JSON()
		if err != nil || strings.Contains(string(data), "s3cret-value-from-env") || strings.Contains(p.Text(), "s3cret-value-from-env") {
			t.Fatalf("the resolved secret reached the plan output (%v)", err)
		}
	})

	t.Run("an invalid project marker plans nothing rather than owning unmarked rows", func(t *testing.T) {
		f := &fakeCloud{
			triggers: []*pb.Trigger{liveTrigger("deploy", "", cloudOrigin), liveTrigger("hand-made", "", cloudOrigin)},
			webhooks: []*pb.SessionWebhook{liveWebhook("https://hooks.example.com/a", ""), liveWebhook("https://hooks.example.com/hand-made", "")},
		}
		cfg := cloudConfig(t, `{"triggers": [`+httpTrigger+`], "webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"]}]}`)
		st, err := ObserveCloud(context.Background(), cfg, f, CloudOptions{Org: cloudOrg})
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		cfg.Project = "Not A Slug!"
		p := PlanCloud(cfg, st, CloudOptions{Org: cloudOrg, CanMintSecrets: true, Prune: true})
		if len(p.Changes) != 0 || len(p.ops) != 0 || p.HasWrites() {
			t.Fatalf("an invalid marker planned work:\n%s", p.Text())
		}
		if len(p.Warnings) != 1 || !strings.HasPrefix(p.Warnings[0], "cloud: planned nothing:") {
			t.Fatalf("warnings %q, want exactly one planned-nothing warning", p.Warnings)
		}
	})

	t.Run("observation fails when the paid-gate read fails, before any write", func(t *testing.T) {
		f := &fakeCloud{listTriggersErr: connect.NewError(connect.CodePermissionDenied, errors.New("a paid plan is required"))}
		cfg := cloudConfig(t, `{"triggers": [`+httpTrigger+`], "webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"]}]}`)
		if _, err := ObserveCloud(context.Background(), cfg, f, CloudOptions{Org: cloudOrg}); err == nil || !strings.Contains(err.Error(), "paid plan") {
			t.Fatalf("observe = %v, want the paid gate's refusal", err)
		}
		if len(f.log) != 1 || len(f.writes()) != 0 {
			t.Fatalf("calls after the refused precheck: %v", f.log)
		}
	})
}

func TestOwnRowPairing(t *testing.T) {
	type row struct{ id, key, marker string }
	rows := []row{{"x-1", "x", cloudMarker}, {"y-1", "y", cloudMarker}, {"x-2", "x", cloudMarker}, {"x-hand", "x", ""}, {"x-other", "x", "boss-project:other"}}
	own := ownRowsByIdentity(rows, cloudMarker, func(r row) string { return r.marker }, func(r row) string { return r.key })
	// Dropped changes arrive in a different order than the rows were observed,
	// and two own rows share identity x: each pairs one-to-one in observed order.
	for _, step := range []struct{ identity, want string }{{"y", "y-1"}, {"x", "x-1"}, {"x", "x-2"}, {"x", ""}, {"y", ""}, {"z", ""}} {
		if got := takeOwnRow(own, step.identity).id; got != step.want {
			t.Fatalf("take %q = %q, want %q", step.identity, got, step.want)
		}
	}

	t.Run("dropped triggers pair with their own row ids regardless of observed order", func(t *testing.T) {
		f := &fakeCloud{triggers: []*pb.Trigger{liveTrigger("zeta", cloudMarker, cloudOrigin), liveTrigger("alpha", cloudMarker, cloudOrigin)}}
		p := planOf(t, cloudConfig(t, `{"triggers": []}`), f, CloudOptions{CanMintSecrets: true, Prune: true})
		for _, name := range []string{"alpha", "zeta"} {
			changeOf(t, p, ResourceTrigger, name, ActionDelete)
			if id := opFor(t, p, name, rankDelete).deleteTrigger; id != "tr-"+name {
				t.Fatalf("delete %q targets %q, want tr-%s", name, id, name)
			}
		}
	})
}
