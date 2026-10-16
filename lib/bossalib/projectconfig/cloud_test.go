package projectconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"google.golang.org/protobuf/encoding/protojson"
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

	t.Run("a retire-only webhook is never matched, disabled or pruned", func(t *testing.T) {
		// bosso marks an orphaned endpoint is_retire_only and withholds its URL
		// from an owner who may only retire it (BOS-1593). Its identity is
		// unknown, so it is not this file's row to disable or delete -- even
		// under --prune, and even carrying this project's marker.
		withheld := liveWebhook("", cloudMarker)
		withheld.Id, withheld.IsRetireOnly = "wh-orphan", true
		f := &fakeCloud{webhooks: []*pb.SessionWebhook{withheld}}
		cfg := cloudConfig(t, `{"webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"]}]}`)
		for _, prune := range []bool{false, true} {
			p := planOf(t, cfg, f, CloudOptions{CanMintSecrets: true, Prune: prune})
			changeOf(t, p, ResourceWebhook, "https://hooks.example.com/a", ActionCreate)
			if len(p.Changes) != 1 {
				t.Fatalf("prune=%v: the retire-only webhook was planned:\n%s", prune, p.Text())
			}
			if !slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, "wh-orphan") }) {
				t.Fatalf("prune=%v: no warning names the retire-only webhook: %q", prune, p.Warnings)
			}
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

// TestPlanCloudEquivalence pins the complete observable plan and its private
// write payloads before the class pipelines are refactored. Expectations were
// captured on 991e8b228 with unchanged production code, then reviewed alongside
// the existing focused planner tests.
func TestPlanCloudEquivalence(t *testing.T) {
	const hook = `{"url":"https://hooks.example.com/a","eventTypes":["session.merged"]}`
	const github = `{"name":"deploy","type":"github","github":{"eventTypes":["push"]},"launch":{"promptTemplate":"go"},"placement":{"mode":"first-available"}}`
	triggerConfig := `{"triggers":[` + httpTrigger + `]}`
	webhookConfig := `{"webhooks":[` + hook + `]}`
	bothConfig := `{"triggers":[` + httpTrigger + `],"webhooks":[` + hook + `]}`
	secretConfig := `{"webhooks":[{"url":"https://hooks.example.com/a","eventTypes":["session.merged"],"secret":{"env":"BOSS_HOOK_SECRET"}}]}`
	placedConfig := strings.Replace(triggerConfig, `"mode": "first-available"`, `"mode":"specific-daemon","daemon":"laptop"`, 1)
	mint := CloudOptions{CanMintSecrets: true}
	rotate := CloudOptions{CanMintSecrets: true, RotateSecrets: true}
	skipRotate := CloudOptions{RotateSecrets: true}
	resolved := CloudOptions{CanMintSecrets: true, RotateSecrets: true, Secrets: EnvResolver(func(string) string { return "fixture-secret" })}
	own := func() *fakeCloud {
		return &fakeCloud{triggers: []*pb.Trigger{liveTrigger("deploy", cloudMarker, cloudOrigin)}, webhooks: []*pb.SessionWebhook{liveWebhook("https://hooks.example.com/a", cloudMarker)}}
	}
	disabled := own()
	disabled.triggers[0].IsEnabled = false
	disabled.webhooks[0].IsEnabled = false
	foreign := own()
	foreign.triggers[0].ManagedBy = "boss-project:other"
	foreign.webhooks[0].ManagedBy = "boss-project:other"
	adopt := own()
	adopt.triggers[0].ManagedBy = ""
	adopt.webhooks[0].ManagedBy = ""
	twins := own()
	twins.triggers = append(twins.triggers, liveTrigger("deploy", "", cloudOrigin))
	twins.webhooks = append(twins.webhooks, liveWebhook("https://hooks.example.com/a", ""))
	ambiguous := own()
	ambiguous.triggers = append(ambiguous.triggers, proto.CloneOf(ambiguous.triggers[0]))
	ambiguous.webhooks = append(ambiguous.webhooks, proto.CloneOf(ambiguous.webhooks[0]))
	launchUpdate := own()
	launchUpdate.triggers[0].Launch.SkillName = "old"
	sensitiveUpdate := own()
	sensitiveUpdate.triggers[0].Launch.PromptTemplate = "old"
	sensitiveUpdate.triggers[0].Gate = &pb.TriggerGate{ScriptPath: "old.sh"}
	webhookUpdate := own()
	webhookUpdate.webhooks[0].Description = "old"
	webhookUpdate.webhooks[0].EventTypes = []string{"session.passing"}
	retired := &fakeCloud{webhooks: []*pb.SessionWebhook{{Id: "retired", ManagedBy: cloudMarker, IsRetireOnly: true}}}
	mixed := own()
	mixed.triggers = append(mixed.triggers, liveTrigger("old", cloudMarker, cloudOrigin))
	mixed.webhooks = append(mixed.webhooks, liveWebhook("https://hooks.example.com/old", cloudMarker))
	mixedConfig := `{"triggers":[` + strings.ReplaceAll(httpTrigger, "deploy", "z-new") + `,` + httpTrigger + `,` + strings.ReplaceAll(httpTrigger, "deploy", "a-new") + `],"webhooks":[` + strings.ReplaceAll(hook, "/a", "/z-new") + `,` + hook + `]}`
	richConfig := `{"triggers":[` + strings.Replace(httpTrigger, `"placement"`, `"gate":{"scriptPath":"scripts/gate.sh"},"filters":[{"field":"body.ref","operator":"equals","values":["main"]}],"payloadFields":["body.ref"],"placement"`, 1) + `],"webhooks":[` + strings.Replace(hook, `"eventTypes"`, `"description":"build events","eventTypes"`, 1) + `]}`

	cases := []struct {
		name, cloud string
		live        *fakeCloud
		opts        CloudOptions
		unsupported bool
	}{
		{"rich-create", richConfig, &fakeCloud{}, mint, false},
		{"mixed-order", mixedConfig, mixed, rotate, false},
		{"create-both", bothConfig, &fakeCloud{}, mint, false},
		{"create-github", `{"triggers":[` + github + `]}`, &fakeCloud{}, mint, false},
		{"create-cli-only", bothConfig, &fakeCloud{}, CloudOptions{}, false},
		{"unchanged", bothConfig, own(), mint, false},
		{"non-sensitive-update", triggerConfig, launchUpdate, mint, false},
		{"sensitive-update", triggerConfig, sensitiveUpdate, mint, false},
		{"webhook-update", webhookConfig, webhookUpdate, mint, false},
		{"explicit-enable", strings.ReplaceAll(bothConfig, `"type": "http"`, `"type": "http", "enabled":true`), disabled, mint, false},
		{"webhook-enable", strings.Replace(webhookConfig, `"eventTypes"`, `"enabled":true,"eventTypes"`, 1), disabled, mint, false},
		{"adopt", bothConfig, adopt, mint, false},
		{"foreign", bothConfig, foreign, mint, false},
		{"ambiguous", bothConfig, ambiguous, mint, false},
		{"type-change", `{"triggers":[` + github + `]}`, own(), mint, false},
		{"unsupported", triggerConfig, own(), mint, true},
		{"unmatched-placement", placedConfig, own(), mint, false},
		{"ambiguous-placement", placedConfig, &fakeCloud{daemons: []*pb.DaemonInfo{{DaemonId: "a", Hostname: "laptop"}, {DaemonId: "b", Hostname: "laptop"}}}, mint, false},
		{"daemon-read-error", placedConfig, &fakeCloud{listDaemonsErr: errors.New("fixture read failure")}, mint, false},
		{"disabled-unmanaged", bothConfig, disabled, mint, false},
		{"other-origin", triggerConfig, &fakeCloud{triggers: []*pb.Trigger{liveTrigger("deploy", "", "https://github.com/acme/other")}}, mint, false},
		{"prefer-own", bothConfig, twins, mint, false},
		{"drop-disable", `{"triggers":[],"webhooks":[]}`, own(), mint, false},
		{"drop-delete", `{"triggers":[],"webhooks":[]}`, own(), CloudOptions{Prune: true}, false},
		{"drop-disabled", `{"triggers":[],"webhooks":[]}`, disabled, mint, false},
		{"rotation-minted", bothConfig, own(), rotate, false},
		{"rotation-skipped", bothConfig, own(), skipRotate, false},
		{"owner-denied", bothConfig, &fakeCloud{listWebhooksErr: connect.NewError(connect.CodePermissionDenied, errors.New("owner required"))}, mint, false},
		{"retire-only", webhookConfig, retired, mint, false},
		{"resolved-create", secretConfig, &fakeCloud{}, resolved, false},
		{"unresolved-create", secretConfig, &fakeCloud{}, CloudOptions{Secrets: EnvResolver(func(string) string { return "" })}, false},
		{"nil-resolver-create", secretConfig, &fakeCloud{}, mint, false},
		{"resolved-rotation", secretConfig, own(), resolved, false},
		{"unresolved-rotation", secretConfig, own(), skipRotate, false},
	}
	want := map[string]string{
		"rich-create":          `{"changes":[{"resource":"trigger","identity":"deploy","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"type","old":"","new":"http"},{"field":"repoOriginUrl","old":"","new":"https://github.com/acme/widget"},{"field":"launch.promptTemplate","old":"","new":"Deploy {{event.type}}"},{"field":"placement","old":"","new":"first-available"},{"field":"gate.scriptPath","old":"","new":"scripts/gate.sh"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"eventTypes","old":"","new":"[\"session.merged\"]"},{"field":"enabled","old":"","new":"true"},{"field":"description","old":"","new":"build events"}],"needsConfirmation":true}],"ops":[{"change":0,"class":"triggers","createTrigger":{"concurrencyPolicy":"TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING","filters":[{"field":"body.ref","values":["main"]}],"gate":{"scriptPath":"scripts/gate.sh"},"http":{"allowedMethods":["POST"],"dedupWindowSeconds":300,"idempotencyHeader":"Idempotency-Key"},"isEnabled":true,"launch":{"isZeroOutput":false,"promptTemplate":"Deploy {{event.type}}","shouldRunSetupCommand":true},"managedBy":"boss-project:widget","name":"deploy","organizationId":"org-1","payloadFields":["body.ref"],"placement":{"mode":"TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE"},"repoOriginUrl":"https://github.com/acme/widget","triggerType":"http"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"trigger \"deploy\"","marker":"boss-project:widget","name":"deploy","rank":0,"rotateTrigger":"","wantURL":""},{"change":1,"class":"webhooks","createWebhook":{"description":"build events","eventTypes":["session.merged"],"isEnabled":true,"managedBy":"boss-project:widget","organizationId":"org-1","url":"https://hooks.example.com/a"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"webhook https://hooks.example.com #878390d4","marker":"boss-project:widget","name":"https://hooks.example.com/a","rank":0,"rotateTrigger":"","wantURL":"https://hooks.example.com/a"}],"text":"create trigger \"deploy\" (needs confirmation): bosso mints the signing secret and it is printed once; type: \"\" -\u003e \"http\"; repoOriginUrl: \"\" -\u003e \"https://github.com/acme/widget\"; launch.promptTemplate: \"\" -\u003e \"Deploy {{event.type}}\"; placement: \"\" -\u003e \"first-available\"; gate.scriptPath: \"\" -\u003e \"scripts/gate.sh\"; enabled: \"\" -\u003e \"true\"\ncreate webhook \"https://hooks.example.com/a\" (needs confirmation): bosso mints the signing secret and it is printed once; eventTypes: \"\" -\u003e \"[\\\"session.merged\\\"]\"; enabled: \"\" -\u003e \"true\"; description: \"\" -\u003e \"build events\"\n","warnings":null}`,
		"mixed-order":          `{"changes":[{"resource":"trigger","identity":"z-new","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"type","old":"","new":"http"},{"field":"repoOriginUrl","old":"","new":"https://github.com/acme/widget"},{"field":"launch.promptTemplate","old":"","new":"Deploy {{event.type}}"},{"field":"placement","old":"","new":"first-available"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true},{"resource":"trigger","identity":"deploy","action":"unchanged"},{"resource":"trigger","identity":"deploy","action":"rotate","detail":"bosso mints a new signing secret and it is printed once","fields":[{"field":"secret","old":"\u003csecret\u003e","new":"\u003csecret\u003e","secret":true}],"needsConfirmation":true},{"resource":"trigger","identity":"a-new","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"type","old":"","new":"http"},{"field":"repoOriginUrl","old":"","new":"https://github.com/acme/widget"},{"field":"launch.promptTemplate","old":"","new":"Deploy {{event.type}}"},{"field":"placement","old":"","new":"first-available"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true},{"resource":"trigger","identity":"old","action":"disable","detail":"no longer in the file"},{"resource":"webhook","identity":"https://hooks.example.com/z-new","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"eventTypes","old":"","new":"[\"session.merged\"]"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"rotate","detail":"bosso mints a new signing secret and it is printed once","fields":[{"field":"secret","old":"\u003csecret\u003e","new":"\u003csecret\u003e","secret":true}],"needsConfirmation":true},{"resource":"webhook","identity":"https://hooks.example.com/old","action":"disable","detail":"no longer in the file"}],"ops":[{"change":3,"class":"triggers","createTrigger":{"concurrencyPolicy":"TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING","http":{"allowedMethods":["POST"],"dedupWindowSeconds":300,"idempotencyHeader":"Idempotency-Key"},"isEnabled":true,"launch":{"isZeroOutput":false,"promptTemplate":"Deploy {{event.type}}","shouldRunSetupCommand":true},"managedBy":"boss-project:widget","name":"a-new","organizationId":"org-1","placement":{"mode":"TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE"},"repoOriginUrl":"https://github.com/acme/widget","triggerType":"http"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"trigger \"a-new\"","marker":"boss-project:widget","name":"a-new","rank":0,"rotateTrigger":"","wantURL":""},{"change":0,"class":"triggers","createTrigger":{"concurrencyPolicy":"TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING","http":{"allowedMethods":["POST"],"dedupWindowSeconds":300,"idempotencyHeader":"Idempotency-Key"},"isEnabled":true,"launch":{"isZeroOutput":false,"promptTemplate":"Deploy {{event.type}}","shouldRunSetupCommand":true},"managedBy":"boss-project:widget","name":"z-new","organizationId":"org-1","placement":{"mode":"TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE"},"repoOriginUrl":"https://github.com/acme/widget","triggerType":"http"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"trigger \"z-new\"","marker":"boss-project:widget","name":"z-new","rank":0,"rotateTrigger":"","wantURL":""},{"change":2,"class":"triggers","deleteTrigger":"","deleteWebhook":"","domain":"","label":"trigger \"deploy\"","marker":"","name":"deploy","rank":1,"rotateTrigger":"tr-deploy","wantURL":""},{"change":4,"class":"triggers","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"","name":"old","rank":2,"rotateTrigger":"","updateTrigger":{"id":"tr-old","isEnabled":false},"wantURL":""},{"change":5,"class":"webhooks","createWebhook":{"eventTypes":["session.merged"],"isEnabled":true,"managedBy":"boss-project:widget","organizationId":"org-1","url":"https://hooks.example.com/z-new"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"webhook https://hooks.example.com #e2c02917","marker":"boss-project:widget","name":"https://hooks.example.com/z-new","rank":0,"rotateTrigger":"","wantURL":"https://hooks.example.com/z-new"},{"change":7,"class":"webhooks","deleteTrigger":"","deleteWebhook":"","domain":"","label":"webhook https://hooks.example.com #878390d4","marker":"","name":"https://hooks.example.com/a","rank":1,"rotateTrigger":"","rotateWebhook":{"id":"wh-https://hooks.example.com/a","organizationId":"org-1"},"wantURL":""},{"change":8,"class":"webhooks","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"","name":"https://hooks.example.com/old","rank":2,"rotateTrigger":"","updateWebhook":{"id":"wh-https://hooks.example.com/old","isEnabled":false,"organizationId":"org-1"},"wantURL":""}],"text":"create trigger \"a-new\" (needs confirmation): bosso mints the signing secret and it is printed once; type: \"\" -\u003e \"http\"; repoOriginUrl: \"\" -\u003e \"https://github.com/acme/widget\"; launch.promptTemplate: \"\" -\u003e \"Deploy {{event.type}}\"; placement: \"\" -\u003e \"first-available\"; enabled: \"\" -\u003e \"true\"\nrotate trigger \"deploy\" (needs confirmation): bosso mints a new signing secret and it is printed once; secret: \u003csecret\u003e -\u003e \u003csecret\u003e\ndisable trigger \"old\": no longer in the file\ncreate trigger \"z-new\" (needs confirmation): bosso mints the signing secret and it is printed once; type: \"\" -\u003e \"http\"; repoOriginUrl: \"\" -\u003e \"https://github.com/acme/widget\"; launch.promptTemplate: \"\" -\u003e \"Deploy {{event.type}}\"; placement: \"\" -\u003e \"first-available\"; enabled: \"\" -\u003e \"true\"\nrotate webhook \"https://hooks.example.com/a\" (needs confirmation): bosso mints a new signing secret and it is printed once; secret: \u003csecret\u003e -\u003e \u003csecret\u003e\ndisable webhook \"https://hooks.example.com/old\": no longer in the file\ncreate webhook \"https://hooks.example.com/z-new\" (needs confirmation): bosso mints the signing secret and it is printed once; eventTypes: \"\" -\u003e \"[\\\"session.merged\\\"]\"; enabled: \"\" -\u003e \"true\"\nwarning: trigger \"old\" is no longer in the file, so it is disabled; apply with --prune to delete it\nwarning: webhook https://hooks.example.com is no longer in the file, so it is disabled; apply with --prune to delete it\n","warnings":["trigger \"old\" is no longer in the file, so it is disabled; apply with --prune to delete it","webhook https://hooks.example.com is no longer in the file, so it is disabled; apply with --prune to delete it"]}`,
		"adopt":                `{"changes":[{"resource":"trigger","identity":"deploy","action":"adopt"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"adopt"}],"ops":[{"change":0,"class":"triggers","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"boss-project:widget","name":"deploy","rank":0,"rotateTrigger":"","updateTrigger":{"id":"tr-deploy","managedBy":"boss-project:widget"},"wantURL":""},{"change":1,"class":"webhooks","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"boss-project:widget","name":"https://hooks.example.com/a","rank":0,"rotateTrigger":"","updateWebhook":{"id":"wh-https://hooks.example.com/a","managedBy":"boss-project:widget","organizationId":"org-1"},"wantURL":""}],"text":"adopt trigger \"deploy\"\nadopt webhook \"https://hooks.example.com/a\"\n","warnings":null}`,
		"ambiguous-placement":  `{"changes":[{"resource":"trigger","identity":"deploy","action":"conflict","reason":"ambiguous","detail":"several of your daemons match the daemons entry \"laptop\" (a, b)"}],"ops":null,"text":"conflict trigger \"deploy\" [ambiguous]: several of your daemons match the daemons entry \"laptop\" (a, b)\n","warnings":null}`,
		"ambiguous":            `{"changes":[{"resource":"trigger","identity":"deploy","action":"conflict","reason":"ambiguous"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"conflict","reason":"ambiguous"}],"ops":null,"text":"conflict trigger \"deploy\" [ambiguous]\nconflict webhook \"https://hooks.example.com/a\" [ambiguous]\n","warnings":null}`,
		"create-both":          `{"changes":[{"resource":"trigger","identity":"deploy","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"type","old":"","new":"http"},{"field":"repoOriginUrl","old":"","new":"https://github.com/acme/widget"},{"field":"launch.promptTemplate","old":"","new":"Deploy {{event.type}}"},{"field":"placement","old":"","new":"first-available"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"eventTypes","old":"","new":"[\"session.merged\"]"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true}],"ops":[{"change":0,"class":"triggers","createTrigger":{"concurrencyPolicy":"TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING","http":{"allowedMethods":["POST"],"dedupWindowSeconds":300,"idempotencyHeader":"Idempotency-Key"},"isEnabled":true,"launch":{"isZeroOutput":false,"promptTemplate":"Deploy {{event.type}}","shouldRunSetupCommand":true},"managedBy":"boss-project:widget","name":"deploy","organizationId":"org-1","placement":{"mode":"TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE"},"repoOriginUrl":"https://github.com/acme/widget","triggerType":"http"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"trigger \"deploy\"","marker":"boss-project:widget","name":"deploy","rank":0,"rotateTrigger":"","wantURL":""},{"change":1,"class":"webhooks","createWebhook":{"eventTypes":["session.merged"],"isEnabled":true,"managedBy":"boss-project:widget","organizationId":"org-1","url":"https://hooks.example.com/a"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"webhook https://hooks.example.com #878390d4","marker":"boss-project:widget","name":"https://hooks.example.com/a","rank":0,"rotateTrigger":"","wantURL":"https://hooks.example.com/a"}],"text":"create trigger \"deploy\" (needs confirmation): bosso mints the signing secret and it is printed once; type: \"\" -\u003e \"http\"; repoOriginUrl: \"\" -\u003e \"https://github.com/acme/widget\"; launch.promptTemplate: \"\" -\u003e \"Deploy {{event.type}}\"; placement: \"\" -\u003e \"first-available\"; enabled: \"\" -\u003e \"true\"\ncreate webhook \"https://hooks.example.com/a\" (needs confirmation): bosso mints the signing secret and it is printed once; eventTypes: \"\" -\u003e \"[\\\"session.merged\\\"]\"; enabled: \"\" -\u003e \"true\"\n","warnings":null}`,
		"create-cli-only":      `{"changes":[{"resource":"trigger","identity":"deploy","action":"skip","reason":"create-via-cli","detail":"creating an HTTP trigger mints a signing secret that only the CLI can show"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"skip","reason":"create-via-cli","detail":"a webhook created without a secret gets one bosso mints, which only the CLI can show"}],"ops":null,"text":"skip trigger \"deploy\" [create-via-cli]: creating an HTTP trigger mints a signing secret that only the CLI can show\nskip webhook \"https://hooks.example.com/a\" [create-via-cli]: a webhook created without a secret gets one bosso mints, which only the CLI can show\n","warnings":null}`,
		"create-github":        `{"changes":[{"resource":"trigger","identity":"deploy","action":"create","fields":[{"field":"type","old":"","new":"github"},{"field":"repoOriginUrl","old":"","new":"https://github.com/acme/widget"},{"field":"launch.promptTemplate","old":"","new":"go"},{"field":"placement","old":"","new":"first-available"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true}],"ops":[{"change":0,"class":"triggers","createTrigger":{"concurrencyPolicy":"TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING","github":{"eventTypes":["push"]},"isEnabled":true,"launch":{"isZeroOutput":false,"promptTemplate":"go","shouldRunSetupCommand":true},"managedBy":"boss-project:widget","name":"deploy","organizationId":"org-1","placement":{"mode":"TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE"},"repoOriginUrl":"https://github.com/acme/widget","triggerType":"github"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"trigger \"deploy\"","marker":"boss-project:widget","name":"deploy","rank":0,"rotateTrigger":"","wantURL":""}],"text":"create trigger \"deploy\" (needs confirmation); type: \"\" -\u003e \"github\"; repoOriginUrl: \"\" -\u003e \"https://github.com/acme/widget\"; launch.promptTemplate: \"\" -\u003e \"go\"; placement: \"\" -\u003e \"first-available\"; enabled: \"\" -\u003e \"true\"\n","warnings":null}`,
		"daemon-read-error":    `{"changes":[{"resource":"trigger","identity":"deploy","action":"skip","reason":"daemon-unmatched","detail":"your daemons could not be listed: fixture read failure"}],"ops":null,"text":"skip trigger \"deploy\" [daemon-unmatched]: your daemons could not be listed: fixture read failure\n","warnings":null}`,
		"disabled-unmanaged":   `{"changes":[{"resource":"trigger","identity":"deploy","action":"unchanged"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"}],"ops":null,"text":"warning: trigger \"deploy\" is disabled and the file does not set enabled, so it stays disabled\nwarning: webhook https://hooks.example.com is disabled and the file does not set enabled, so it stays disabled\n","warnings":["trigger \"deploy\" is disabled and the file does not set enabled, so it stays disabled","webhook https://hooks.example.com is disabled and the file does not set enabled, so it stays disabled"]}`,
		"drop-delete":          `{"changes":[{"resource":"trigger","identity":"deploy","action":"delete","detail":"no longer in the file"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"delete","detail":"no longer in the file"}],"ops":[{"change":0,"class":"triggers","deleteTrigger":"tr-deploy","deleteWebhook":"","domain":"","label":"","marker":"","name":"deploy","rank":3,"rotateTrigger":"","wantURL":""},{"change":1,"class":"webhooks","deleteTrigger":"","deleteWebhook":"wh-https://hooks.example.com/a","domain":"","label":"","marker":"","name":"https://hooks.example.com/a","rank":3,"rotateTrigger":"","wantURL":""}],"text":"delete trigger \"deploy\": no longer in the file\ndelete webhook \"https://hooks.example.com/a\": no longer in the file\n","warnings":null}`,
		"drop-disable":         `{"changes":[{"resource":"trigger","identity":"deploy","action":"disable","detail":"no longer in the file"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"disable","detail":"no longer in the file"}],"ops":[{"change":0,"class":"triggers","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"","name":"deploy","rank":2,"rotateTrigger":"","updateTrigger":{"id":"tr-deploy","isEnabled":false},"wantURL":""},{"change":1,"class":"webhooks","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"","name":"https://hooks.example.com/a","rank":2,"rotateTrigger":"","updateWebhook":{"id":"wh-https://hooks.example.com/a","isEnabled":false,"organizationId":"org-1"},"wantURL":""}],"text":"disable trigger \"deploy\": no longer in the file\ndisable webhook \"https://hooks.example.com/a\": no longer in the file\nwarning: trigger \"deploy\" is no longer in the file, so it is disabled; apply with --prune to delete it\nwarning: webhook https://hooks.example.com is no longer in the file, so it is disabled; apply with --prune to delete it\n","warnings":["trigger \"deploy\" is no longer in the file, so it is disabled; apply with --prune to delete it","webhook https://hooks.example.com is no longer in the file, so it is disabled; apply with --prune to delete it"]}`,
		"drop-disabled":        `{"changes":[{"resource":"trigger","identity":"deploy","action":"unchanged"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"}],"ops":null,"text":"","warnings":null}`,
		"explicit-enable":      `{"changes":[{"resource":"trigger","identity":"deploy","action":"update","fields":[{"field":"enabled","old":"false","new":"true"}]},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"}],"ops":[{"change":0,"class":"triggers","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"boss-project:widget","name":"deploy","rank":0,"rotateTrigger":"","updateTrigger":{"id":"tr-deploy","isEnabled":true},"wantURL":""}],"text":"update trigger \"deploy\"; enabled: \"false\" -\u003e \"true\"\nwarning: webhook https://hooks.example.com is disabled and the file does not set enabled, so it stays disabled\n","warnings":["webhook https://hooks.example.com is disabled and the file does not set enabled, so it stays disabled"]}`,
		"foreign":              `{"changes":[{"resource":"trigger","identity":"deploy","action":"conflict","reason":"managed-by-other","detail":"your trigger \"deploy\" is managed by \"boss-project:other\""},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"conflict","reason":"managed-by-other","detail":"this URL is managed by \"boss-project:other\"; both projects claim it"}],"ops":null,"text":"conflict trigger \"deploy\" [managed-by-other]: your trigger \"deploy\" is managed by \"boss-project:other\"\nconflict webhook \"https://hooks.example.com/a\" [managed-by-other]: this URL is managed by \"boss-project:other\"; both projects claim it\n","warnings":null}`,
		"nil-resolver-create":  `{"changes":[{"resource":"webhook","identity":"https://hooks.example.com/a","action":"skip","reason":"secret-unresolved","detail":"resolve via the CLI"}],"ops":null,"text":"skip webhook \"https://hooks.example.com/a\" [secret-unresolved]: resolve via the CLI\n","warnings":null}`,
		"non-sensitive-update": `{"changes":[{"resource":"trigger","identity":"deploy","action":"update","fields":[{"field":"launch","old":"{\"isZeroOutput\":false,\"promptTemplate\":\"Deploy {{event.type}}\",\"shouldRunSetupCommand\":true,\"skillName\":\"old\"}","new":"{\"isZeroOutput\":false,\"promptTemplate\":\"Deploy {{event.type}}\",\"shouldRunSetupCommand\":true}"}]}],"ops":[{"change":0,"class":"triggers","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"boss-project:widget","name":"deploy","rank":0,"rotateTrigger":"","updateTrigger":{"id":"tr-deploy","launch":{"isZeroOutput":false,"promptTemplate":"Deploy {{event.type}}","shouldRunSetupCommand":true}},"wantURL":""}],"text":"update trigger \"deploy\"; launch: \"{\\\"isZeroOutput\\\":false,\\\"promptTemplate\\\":\\\"Deploy {{event.type}}\\\",\\\"shouldRunSetupCommand\\\":true,\\\"skillName\\\":\\\"old\\\"}\" -\u003e \"{\\\"isZeroOutput\\\":false,\\\"promptTemplate\\\":\\\"Deploy {{event.type}}\\\",\\\"shouldRunSetupCommand\\\":true}\"\n","warnings":null}`,
		"other-origin":         `{"changes":[{"resource":"trigger","identity":"deploy","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"type","old":"","new":"http"},{"field":"repoOriginUrl","old":"","new":"https://github.com/acme/widget"},{"field":"launch.promptTemplate","old":"","new":"Deploy {{event.type}}"},{"field":"placement","old":"","new":"first-available"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true}],"ops":[{"change":0,"class":"triggers","createTrigger":{"concurrencyPolicy":"TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING","http":{"allowedMethods":["POST"],"dedupWindowSeconds":300,"idempotencyHeader":"Idempotency-Key"},"isEnabled":true,"launch":{"isZeroOutput":false,"promptTemplate":"Deploy {{event.type}}","shouldRunSetupCommand":true},"managedBy":"boss-project:widget","name":"deploy","organizationId":"org-1","placement":{"mode":"TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE"},"repoOriginUrl":"https://github.com/acme/widget","triggerType":"http"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"trigger \"deploy\"","marker":"boss-project:widget","name":"deploy","rank":0,"rotateTrigger":"","wantURL":""}],"text":"create trigger \"deploy\" (needs confirmation): bosso mints the signing secret and it is printed once; type: \"\" -\u003e \"http\"; repoOriginUrl: \"\" -\u003e \"https://github.com/acme/widget\"; launch.promptTemplate: \"\" -\u003e \"Deploy {{event.type}}\"; placement: \"\" -\u003e \"first-available\"; enabled: \"\" -\u003e \"true\"\n","warnings":null}`,
		"owner-denied":         `{"changes":[{"resource":"trigger","identity":"deploy","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"type","old":"","new":"http"},{"field":"repoOriginUrl","old":"","new":"https://github.com/acme/widget"},{"field":"launch.promptTemplate","old":"","new":"Deploy {{event.type}}"},{"field":"placement","old":"","new":"first-available"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"skip","reason":"owner-required","detail":"session webhooks can only be changed by an active owner of the organization"}],"ops":[{"change":0,"class":"triggers","createTrigger":{"concurrencyPolicy":"TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING","http":{"allowedMethods":["POST"],"dedupWindowSeconds":300,"idempotencyHeader":"Idempotency-Key"},"isEnabled":true,"launch":{"isZeroOutput":false,"promptTemplate":"Deploy {{event.type}}","shouldRunSetupCommand":true},"managedBy":"boss-project:widget","name":"deploy","organizationId":"org-1","placement":{"mode":"TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE"},"repoOriginUrl":"https://github.com/acme/widget","triggerType":"http"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"trigger \"deploy\"","marker":"boss-project:widget","name":"deploy","rank":0,"rotateTrigger":"","wantURL":""}],"text":"create trigger \"deploy\" (needs confirmation): bosso mints the signing secret and it is printed once; type: \"\" -\u003e \"http\"; repoOriginUrl: \"\" -\u003e \"https://github.com/acme/widget\"; launch.promptTemplate: \"\" -\u003e \"Deploy {{event.type}}\"; placement: \"\" -\u003e \"first-available\"; enabled: \"\" -\u003e \"true\"\nskip webhook \"https://hooks.example.com/a\" [owner-required]: session webhooks can only be changed by an active owner of the organization\n","warnings":null}`,
		"prefer-own":           `{"changes":[{"resource":"trigger","identity":"deploy","action":"unchanged"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"}],"ops":null,"text":"","warnings":null}`,
		"resolved-create":      `{"changes":[{"resource":"webhook","identity":"https://hooks.example.com/a","action":"create","detail":"signing secret from $BOSS_HOOK_SECRET","fields":[{"field":"eventTypes","old":"","new":"[\"session.merged\"]"},{"field":"enabled","old":"","new":"true"},{"field":"secret","old":"\u003csecret\u003e","new":"\u003csecret\u003e","secret":true}],"needsConfirmation":true}],"ops":[{"change":0,"class":"webhooks","createWebhook":{"eventTypes":["session.merged"],"isEnabled":true,"managedBy":"boss-project:widget","organizationId":"org-1","secret":"fixture-secret","url":"https://hooks.example.com/a"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"boss-project:widget","name":"https://hooks.example.com/a","rank":0,"rotateTrigger":"","wantURL":"https://hooks.example.com/a"}],"text":"create webhook \"https://hooks.example.com/a\" (needs confirmation): signing secret from $BOSS_HOOK_SECRET; eventTypes: \"\" -\u003e \"[\\\"session.merged\\\"]\"; enabled: \"\" -\u003e \"true\"; secret: \u003csecret\u003e -\u003e \u003csecret\u003e\n","warnings":null}`,
		"resolved-rotation":    `{"changes":[{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"rotate","detail":"new signing secret from $BOSS_HOOK_SECRET","fields":[{"field":"secret","old":"\u003csecret\u003e","new":"\u003csecret\u003e","secret":true}],"needsConfirmation":true}],"ops":[{"change":1,"class":"webhooks","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"","name":"https://hooks.example.com/a","rank":1,"rotateTrigger":"","rotateWebhook":{"id":"wh-https://hooks.example.com/a","organizationId":"org-1","secret":"fixture-secret"},"wantURL":""}],"text":"rotate webhook \"https://hooks.example.com/a\" (needs confirmation): new signing secret from $BOSS_HOOK_SECRET; secret: \u003csecret\u003e -\u003e \u003csecret\u003e\n","warnings":null}`,
		"retire-only":          `{"changes":[{"resource":"webhook","identity":"https://hooks.example.com/a","action":"create","detail":"bosso mints the signing secret and it is printed once","fields":[{"field":"eventTypes","old":"","new":"[\"session.merged\"]"},{"field":"enabled","old":"","new":"true"}],"needsConfirmation":true}],"ops":[{"change":0,"class":"webhooks","createWebhook":{"eventTypes":["session.merged"],"isEnabled":true,"managedBy":"boss-project:widget","organizationId":"org-1","url":"https://hooks.example.com/a"},"deleteTrigger":"","deleteWebhook":"","domain":"","label":"webhook https://hooks.example.com #878390d4","marker":"boss-project:widget","name":"https://hooks.example.com/a","rank":0,"rotateTrigger":"","wantURL":"https://hooks.example.com/a"}],"text":"create webhook \"https://hooks.example.com/a\" (needs confirmation): bosso mints the signing secret and it is printed once; eventTypes: \"\" -\u003e \"[\\\"session.merged\\\"]\"; enabled: \"\" -\u003e \"true\"\nwarning: cloud.webhooks: left as is: session webhook retired is listed without its URL because its creator is no longer an owner of the organization; it can only be disabled or deleted, by id\n","warnings":["cloud.webhooks: left as is: session webhook retired is listed without its URL because its creator is no longer an owner of the organization; it can only be disabled or deleted, by id"]}`,
		"rotation-minted":      `{"changes":[{"resource":"trigger","identity":"deploy","action":"unchanged"},{"resource":"trigger","identity":"deploy","action":"rotate","detail":"bosso mints a new signing secret and it is printed once","fields":[{"field":"secret","old":"\u003csecret\u003e","new":"\u003csecret\u003e","secret":true}],"needsConfirmation":true},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"rotate","detail":"bosso mints a new signing secret and it is printed once","fields":[{"field":"secret","old":"\u003csecret\u003e","new":"\u003csecret\u003e","secret":true}],"needsConfirmation":true}],"ops":[{"change":1,"class":"triggers","deleteTrigger":"","deleteWebhook":"","domain":"","label":"trigger \"deploy\"","marker":"","name":"deploy","rank":1,"rotateTrigger":"tr-deploy","wantURL":""},{"change":3,"class":"webhooks","deleteTrigger":"","deleteWebhook":"","domain":"","label":"webhook https://hooks.example.com #878390d4","marker":"","name":"https://hooks.example.com/a","rank":1,"rotateTrigger":"","rotateWebhook":{"id":"wh-https://hooks.example.com/a","organizationId":"org-1"},"wantURL":""}],"text":"rotate trigger \"deploy\" (needs confirmation): bosso mints a new signing secret and it is printed once; secret: \u003csecret\u003e -\u003e \u003csecret\u003e\nrotate webhook \"https://hooks.example.com/a\" (needs confirmation): bosso mints a new signing secret and it is printed once; secret: \u003csecret\u003e -\u003e \u003csecret\u003e\n","warnings":null}`,
		"rotation-skipped":     `{"changes":[{"resource":"trigger","identity":"deploy","action":"unchanged"},{"resource":"trigger","identity":"deploy","action":"skip","reason":"create-via-cli","detail":"rotating a signing secret mints one that only the CLI can show"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"skip","reason":"create-via-cli","detail":"rotating without a resolvable secret mints one that only the CLI can show"}],"ops":null,"text":"skip trigger \"deploy\" [create-via-cli]: rotating a signing secret mints one that only the CLI can show\nskip webhook \"https://hooks.example.com/a\" [create-via-cli]: rotating without a resolvable secret mints one that only the CLI can show\n","warnings":null}`,
		"sensitive-update":     `{"changes":[{"resource":"trigger","identity":"deploy","action":"update","fields":[{"field":"launch","old":"{\"isZeroOutput\":false,\"promptTemplate\":\"old\",\"shouldRunSetupCommand\":true}","new":"{\"isZeroOutput\":false,\"promptTemplate\":\"Deploy {{event.type}}\",\"shouldRunSetupCommand\":true}"},{"field":"gate","old":"{\"scriptPath\":\"old.sh\"}","new":""}],"needsConfirmation":true}],"ops":[{"change":0,"class":"triggers","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"boss-project:widget","name":"deploy","rank":0,"rotateTrigger":"","updateTrigger":{"gate":{},"id":"tr-deploy","launch":{"isZeroOutput":false,"promptTemplate":"Deploy {{event.type}}","shouldRunSetupCommand":true}},"wantURL":""}],"text":"update trigger \"deploy\" (needs confirmation); launch: \"{\\\"isZeroOutput\\\":false,\\\"promptTemplate\\\":\\\"old\\\",\\\"shouldRunSetupCommand\\\":true}\" -\u003e \"{\\\"isZeroOutput\\\":false,\\\"promptTemplate\\\":\\\"Deploy {{event.type}}\\\",\\\"shouldRunSetupCommand\\\":true}\"; gate: \"{\\\"scriptPath\\\":\\\"old.sh\\\"}\" -\u003e \"\"\n","warnings":null}`,
		"type-change":          `{"changes":[{"resource":"trigger","identity":"deploy","action":"conflict","reason":"type-change","detail":"the trigger is \"http\" and the file says \"github\"; delete it and re-apply to change its type"}],"ops":null,"text":"conflict trigger \"deploy\" [type-change]: the trigger is \"http\" and the file says \"github\"; delete it and re-apply to change its type\n","warnings":null}`,
		"unchanged":            `{"changes":[{"resource":"trigger","identity":"deploy","action":"unchanged"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"}],"ops":null,"text":"","warnings":null}`,
		"unmatched-placement":  `{"changes":[{"resource":"trigger","identity":"deploy","action":"skip","reason":"daemon-unmatched","detail":"none of your daemons matches the daemons entry \"laptop\""}],"ops":null,"text":"skip trigger \"deploy\" [daemon-unmatched]: none of your daemons matches the daemons entry \"laptop\"\n","warnings":null}`,
		"unresolved-create":    `{"changes":[{"resource":"webhook","identity":"https://hooks.example.com/a","action":"skip","reason":"secret-unresolved","detail":"$BOSS_HOOK_SECRET is not set; the webhook is not created with a different secret"}],"ops":null,"text":"skip webhook \"https://hooks.example.com/a\" [secret-unresolved]: $BOSS_HOOK_SECRET is not set; the webhook is not created with a different secret\n","warnings":null}`,
		"unresolved-rotation":  `{"changes":[{"resource":"webhook","identity":"https://hooks.example.com/a","action":"unchanged"},{"resource":"webhook","identity":"https://hooks.example.com/a","action":"skip","reason":"create-via-cli","detail":"rotating without a resolvable secret mints one that only the CLI can show"}],"ops":null,"text":"skip webhook \"https://hooks.example.com/a\" [create-via-cli]: rotating without a resolvable secret mints one that only the CLI can show\n","warnings":null}`,
		"unsupported":          `{"changes":[{"resource":"trigger","identity":"deploy","action":"skip","reason":"unsupported-value","detail":"a value in this trigger is not one this boss understands"}],"ops":null,"text":"skip trigger \"deploy\" [unsupported-value]: a value in this trigger is not one this boss understands\n","warnings":null}`,
		"webhook-enable":       `{"changes":[{"resource":"webhook","identity":"https://hooks.example.com/a","action":"update","fields":[{"field":"enabled","old":"false","new":"true"}]}],"ops":[{"change":0,"class":"webhooks","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"boss-project:widget","name":"https://hooks.example.com/a","rank":0,"rotateTrigger":"","updateWebhook":{"id":"wh-https://hooks.example.com/a","isEnabled":true,"organizationId":"org-1"},"wantURL":""}],"text":"update webhook \"https://hooks.example.com/a\"; enabled: \"false\" -\u003e \"true\"\n","warnings":null}`,
		"webhook-update":       `{"changes":[{"resource":"webhook","identity":"https://hooks.example.com/a","action":"update","fields":[{"field":"description","old":"old","new":""},{"field":"eventTypes","old":"[\"session.passing\"]","new":"[\"session.merged\"]"}]}],"ops":[{"change":0,"class":"webhooks","deleteTrigger":"","deleteWebhook":"","domain":"","label":"","marker":"boss-project:widget","name":"https://hooks.example.com/a","rank":0,"rotateTrigger":"","updateWebhook":{"description":"","eventTypes":["session.merged"],"id":"wh-https://hooks.example.com/a","organizationId":"org-1","shouldReplaceEventTypes":true},"wantURL":""}],"text":"update webhook \"https://hooks.example.com/a\"; description: \"old\" -\u003e \"\"; eventTypes: \"[\\\"session.passing\\\"]\" -\u003e \"[\\\"session.merged\\\"]\"\n","warnings":null}`,
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cloudConfig(t, tc.cloud)
			if tc.unsupported {
				cfg.Cloud.Triggers[0].unsupported = true
			}
			p := planOf(t, cfg, tc.live, tc.opts)
			got := cloudPlanSnapshot(t, p)
			if got != want[tc.name] {
				t.Fatalf("plan changed\ngot:  %s\nwant: %s", got, want[tc.name])
			}
		})
	}
}

// cloudPlanSnapshot includes request presence, optional proto fields, and op
// metadata that rendering deliberately omits. JSON maps sort keys after
// protojson conversion, avoiding protobuf's unstable object key order.
func cloudPlanSnapshot(t *testing.T, p CloudPlan) string {
	t.Helper()
	var ops []map[string]any
	for _, op := range p.ops {
		row := map[string]any{"change": op.change, "class": op.class, "domain": op.domain, "rank": op.rank, "name": op.name, "marker": op.marker, "label": op.label, "wantURL": op.wantURL, "deleteTrigger": op.deleteTrigger, "rotateTrigger": op.rotateTrigger, "deleteWebhook": op.deleteWebhook}
		for name, msg := range map[string]proto.Message{"createTrigger": op.createTrigger, "updateTrigger": op.updateTrigger, "createWebhook": op.createWebhook, "updateWebhook": op.updateWebhook, "rotateWebhook": op.rotateWebhook} {
			if !msg.ProtoReflect().IsValid() {
				continue
			}
			b, err := protojson.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			var v any
			if err := json.Unmarshal(b, &v); err != nil {
				t.Fatal(err)
			}
			row[name] = v
		}
		ops = append(ops, row)
	}
	b, err := json.Marshal(map[string]any{"text": p.Text(), "changes": p.Changes, "warnings": p.Warnings, "ops": ops})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPlanCloudManagedByOtherDetails(t *testing.T) {
	t.Run("trigger", func(t *testing.T) {
		p := planOf(t, cloudConfig(t, `{"triggers":[`+httpTrigger+`]}`), &fakeCloud{triggers: []*pb.Trigger{liveTrigger("deploy", "boss-project:other", cloudOrigin)}}, CloudOptions{})
		c := changeOf(t, p, ResourceTrigger, "deploy", ActionConflict)
		if want := `your trigger "deploy" is managed by "boss-project:other"`; c.Detail != want {
			t.Fatalf("detail %q, want %q", c.Detail, want)
		}
	})
	t.Run("webhook", func(t *testing.T) {
		const url = "https://hooks.example.com/a"
		p := planOf(t, cloudConfig(t, `{"webhooks":[{"url":"`+url+`","eventTypes":["session.merged"]}]}`), &fakeCloud{webhooks: []*pb.SessionWebhook{liveWebhook(url, "boss-project:other")}}, CloudOptions{})
		c := changeOf(t, p, ResourceWebhook, url, ActionConflict)
		if want := `this URL is managed by "boss-project:other"; both projects claim it`; c.Detail != want {
			t.Fatalf("detail %q, want %q", c.Detail, want)
		}
	})
}
