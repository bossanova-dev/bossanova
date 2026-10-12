package projectconfig

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// --- fakeCloud's fleet and remote-cron surface -------------------------------

func (f *fakeCloud) ListFleet(_ context.Context, org string) ([]*pb.DaemonInfo, []*pb.DaemonRef, error) {
	f.log = append(f.log, "list-fleet "+org)
	return f.own, f.peers, nil
}

func (f *fakeCloud) ListReposAggregated(_ context.Context, org string) ([]*pb.AggregatedRepo, error) {
	f.log = append(f.log, "list-repos "+org)
	return f.repos, nil
}

func (f *fakeCloud) ListCronJobsWithDaemon(_ context.Context, org string) ([]*pb.CronJobWithDaemon, error) {
	f.log = append(f.log, "list-crons "+org)
	out := make([]*pb.CronJobWithDaemon, 0, len(f.cronRows))
	for _, r := range f.cronRows {
		out = append(out, proto.CloneOf(r))
	}
	return out, nil
}

// cronEcho is the marker a daemon stores: none on a daemon too old to know it.
func (f *fakeCloud) cronEcho(daemonID, marker string) string {
	if f.oldDaemons[daemonID] {
		return ""
	}
	return marker
}

func (f *fakeCloud) cronRow(daemonID, id string) *pb.CronJob {
	for _, r := range f.cronRows {
		if r.GetDaemonId() == daemonID && r.GetJob().GetId() == id {
			return r.GetJob()
		}
	}
	return nil
}

func (f *fakeCloud) CreateCronJobOn(_ context.Context, daemonID string, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	if err := f.call("create-cron " + daemonID + " " + req.GetName()); err != nil {
		return nil, err
	}
	job := &pb.CronJob{Id: f.id("cj"), RepoId: req.GetRepoId(), Name: req.GetName(), Prompt: req.GetPrompt(), Schedule: req.GetSchedule(),
		Timezone: req.GetTimezone(), IsEnabled: req.GetIsEnabled(), AgentName: req.GetAgentName(), Model: req.GetModel(),
		GateCommand: req.GetGateCommand(), ManagedBy: f.cronEcho(daemonID, req.GetManagedBy())}
	f.cronRows = append(f.cronRows, &pb.CronJobWithDaemon{Job: job, DaemonId: daemonID})
	return proto.CloneOf(job), nil
}

func (f *fakeCloud) UpdateCronJobOn(_ context.Context, daemonID string, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	job := f.cronRow(daemonID, req.GetId())
	if err := f.call("update-cron " + daemonID + " " + job.GetName()); err != nil {
		return nil, err
	}
	if job == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such cron job on this daemon"))
	}
	if req.Name != nil {
		job.Name = req.GetName()
	}
	if req.Prompt != nil {
		job.Prompt = req.GetPrompt()
	}
	if req.Schedule != nil {
		job.Schedule = req.GetSchedule()
	}
	if req.GateCommand != nil {
		job.GateCommand = req.GetGateCommand()
	}
	if req.AgentName != nil {
		job.AgentName = req.GetAgentName()
	}
	if req.IsEnabled != nil {
		job.IsEnabled = req.GetIsEnabled()
	}
	if req.ManagedBy != nil {
		job.ManagedBy = f.cronEcho(daemonID, req.GetManagedBy())
	}
	return proto.CloneOf(job), nil
}

func (f *fakeCloud) DeleteCronJobOn(_ context.Context, daemonID, id string) error {
	job := f.cronRow(daemonID, id)
	if err := f.call("delete-cron " + daemonID + " " + job.GetName()); err != nil {
		return err
	}
	f.cronRows = slices.DeleteFunc(f.cronRows, func(r *pb.CronJobWithDaemon) bool { return r.GetJob() == job })
	return nil
}

// --- Fixtures ----------------------------------------------------------------

// localDaemon is the daemon the local leg writes; its entry is "laptop".
var localDaemon = DaemonIdentity{ID: "d-local", DisplayName: "laptop"}

// fleetConfig decodes a project file with the given daemons and crons arrays
// and no cloud section.
func fleetConfig(t *testing.T, daemons, crons string) *Config {
	t.Helper()
	cfg, _ := mustDecode(t, []byte(`{"version": 1, "project": "widget",
  "repo": {"originUrl": "git@github.com:acme/widget.git"},
  "daemons": `+daemons+`, "crons": `+crons+`}`))
	if err := Validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return cfg
}

const fleetDaemons = `[{"name": "laptop", "daemonId": "d-local", "path": "~/src/widget"},
  {"name": "server", "daemonId": "d-server", "path": "~/src/widget"},
  {"name": "box", "daemonId": "d-box", "path": "~/src/widget"}]`

// newFleet is the caller's fleet: this daemon plus two online daemons, all
// serving the repo.
func newFleet() *fakeCloud {
	return &fakeCloud{
		own: []*pb.DaemonInfo{
			{DaemonId: "d-local", Hostname: "laptop", Online: true},
			{DaemonId: "d-server", Hostname: "server", Online: true},
			{DaemonId: "d-box", Hostname: "box", Online: true},
		},
		repos: []*pb.AggregatedRepo{{OriginUrl: cloudOrigin, Daemons: []*pb.DaemonRepoRef{
			{DaemonId: "d-local", RepoId: "r-local"}, {DaemonId: "d-server", RepoId: "r-server"}, {DaemonId: "d-box", RepoId: "r-box"},
		}}},
	}
}

func cronRowOn(daemonID, repoID, name, prompt, schedule, marker string) *pb.CronJobWithDaemon {
	return &pb.CronJobWithDaemon{DaemonId: daemonID, Job: &pb.CronJob{Id: "cj-" + daemonID + "-" + name, RepoId: repoID, Name: name,
		Prompt: prompt, Schedule: schedule, IsEnabled: true, ManagedBy: marker}}
}

// cronChange returns the plan's one cron change for identity on daemon.
func cronChange(t *testing.T, p CloudPlan, daemon, identity string) Change {
	t.Helper()
	var found []Change
	for _, c := range p.Changes {
		if c.Resource == ResourceCron && c.Daemon == daemon && c.Identity == identity {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one cron %q change on %q, got %+v\nplan:\n%s", identity, daemon, found, p.Text())
	}
	return found[0]
}

// remoteOp returns the remote-cron write applying the change for identity on
// daemon.
func remoteOp(t *testing.T, p CloudPlan, daemon, identity string) cloudOp {
	t.Helper()
	for _, op := range p.ops {
		c := p.Changes[op.change]
		if op.class == CloudClassRemoteCrons && c.Daemon == daemon && c.Identity == identity {
			return op
		}
	}
	t.Fatalf("no remote cron write for %q on %q\nplan:\n%s", identity, daemon, p.Text())
	return cloudOp{}
}

func assertNoRemoteOps(t *testing.T, p CloudPlan) {
	t.Helper()
	for _, op := range p.ops {
		if op.class == CloudClassRemoteCrons {
			t.Fatalf("unexpected remote cron write to %s for %q\nplan:\n%s", op.daemonID, p.Changes[op.change].Identity, p.Text())
		}
	}
}

func fleetOpts() CloudOptions {
	return CloudOptions{CanMintSecrets: true, LocalDaemon: localDaemon, RemoteFactory: HostFreeFactory(testStages)}
}

var testStages = []FactoryStage{
	{Stage: "plan", Skill: "boss-plan", DefaultSchedule: "*/15 * * * *"},
	{Stage: "build", Skill: "boss-build", DefaultSchedule: "*/15 * * * *"},
}

// --- Plan --------------------------------------------------------------------

func TestPlanCloudCrons(t *testing.T) {
	t.Run("own online daemon serving the repo: create aimed at its id and repo, then unchanged", func(t *testing.T) {
		f := newFleet()
		cfg := fleetConfig(t, fleetDaemons, `[{"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *", "daemons": ["server"]}]`)
		p := planOf(t, cfg, f, fleetOpts())
		c := cronChange(t, p, "server", "nightly")
		if c.Action != ActionCreate || !c.NeedsConfirmation || !p.NeedsConfirmation() {
			t.Fatalf("change %+v, want a create needing confirmation", c)
		}
		op := remoteOp(t, p, "server", "nightly")
		if op.daemonID != "d-server" || op.cron.createCron.GetRepoId() != "r-server" || op.cron.createCron.GetManagedBy() != cloudMarker || op.cron.marker != cloudMarker {
			t.Fatalf("create aimed at %q with %+v", op.daemonID, op.cron.createCron)
		}
		if _, err := ApplyCloud(context.Background(), f, f, p); err != nil {
			t.Fatalf("apply: %v", err)
		}
		again := planOf(t, cfg, f, fleetOpts())
		if c := cronChange(t, again, "server", "nightly"); c.Action != ActionUnchanged || again.HasWrites() || !again.Converged() {
			t.Fatalf("second plan:\n%s", again.Text())
		}
	})

	t.Run("the local daemon is never fanned out to", func(t *testing.T) {
		f := newFleet()
		// The fleet reports this daemon under a hostname that names another,
		// unpinned entry: only its id keeps it out of the fan-out.
		f.own[0].Hostname = "alias"
		f.cronRows = []*pb.CronJobWithDaemon{cronRowOn("d-local", "r-local", "stale", "/stale", "0 1 * * *", cloudMarker)}
		daemons := `[{"name": "laptop", "path": "~/w"}, {"name": "server", "daemonId": "d-server", "path": "~/w"}, {"name": "alias", "path": "~/w"}]`
		cfg := fleetConfig(t, daemons, `[{"name": "mine", "prompt": "/mine", "schedule": "0 3 * * *", "daemons": ["laptop"]},
		  {"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *", "daemons": ["server", "alias"]}]`)
		p := planOf(t, cfg, f, CloudOptions{CanMintSecrets: true, LocalDaemon: localDaemon, Prune: true})
		for _, c := range p.Changes {
			if c.Daemon == "laptop" {
				t.Fatalf("the local daemon's entry reached the cloud plan: %+v", c)
			}
		}
		if c := cronChange(t, p, "alias", "nightly"); c.Action != ActionSkip || c.Reason != ReasonDaemonUnmatched {
			t.Fatalf("alias: %+v", c)
		}
		for _, op := range p.ops {
			if op.daemonID == "d-local" {
				t.Fatalf("a remote write targets the local daemon: %+v", op)
			}
		}
	})

	t.Run("a file naming only this daemon reads no fleet", func(t *testing.T) {
		f := newFleet()
		cfg := fleetConfig(t, `[{"name": "laptop", "daemonId": "d-local", "path": "~/src/widget"}]`, `[{"name": "mine", "prompt": "/mine", "schedule": "0 3 * * *"}]`)
		p := planOf(t, cfg, f, fleetOpts())
		if len(p.Changes) != 0 || slices.ContainsFunc(f.log, func(e string) bool { return strings.HasPrefix(e, "list-fleet") }) {
			t.Fatalf("plan %s\nlog %q", p.Text(), f.log)
		}
	})

	t.Run("last cron removed from a remote entry disables its own-marker row, deleted under prune", func(t *testing.T) {
		f := newFleet()
		f.cronRows = []*pb.CronJobWithDaemon{
			cronRowOn("d-server", "r-server", "dropped", "/d", "0 3 * * *", cloudMarker),
			cronRowOn("d-server", "r-server", "hand", "/h", "0 3 * * *", ""),
		}
		cfg := fleetConfig(t, fleetDaemons, `[{"name": "mine", "prompt": "/mine", "schedule": "0 3 * * *", "daemons": ["laptop"]}]`)
		p := planOf(t, cfg, f, fleetOpts())
		if c := cronChange(t, p, "server", "dropped"); c.Action != ActionDisable {
			t.Fatalf("dropped: %+v", c)
		}
		for _, c := range p.Changes {
			if c.Daemon == "box" || (c.Daemon == "server" && c.Identity == "hand" && c.Action != ActionUnchanged && c.Action != ActionSkip) {
				t.Fatalf("unexpected change %+v\nplan:\n%s", c, p.Text())
			}
		}
		opts := fleetOpts()
		opts.Prune = true
		if c := cronChange(t, planOf(t, cfg, f, opts), "server", "dropped"); c.Action != ActionDelete {
			t.Fatalf("pruned: %+v", c)
		}
	})

	t.Run("unknown local id skips every remote cron and reads no fleet", func(t *testing.T) {
		f := newFleet()
		cfg := fleetConfig(t, fleetDaemons, `[{"name": "mine", "prompt": "/mine", "schedule": "0 3 * * *", "daemons": ["laptop"]},
		  {"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *", "daemons": ["server", "box"]}]`)
		opts := fleetOpts()
		opts.LocalDaemon = DaemonIdentity{DisplayName: "laptop"}
		p := planOf(t, cfg, f, opts)
		for _, d := range []string{"server", "box"} {
			if c := cronChange(t, p, d, "nightly"); c.Action != ActionSkip || c.Reason != ReasonLocalDaemonUnidentified {
				t.Fatalf("%s: %+v", d, c)
			}
		}
		assertNoRemoteOps(t, p)
		if slices.ContainsFunc(f.log, func(e string) bool { return strings.HasPrefix(e, "list-fleet") }) {
			t.Fatalf("fleet read with an unknown local id: %q", f.log)
		}
	})

	t.Run("no local daemon plans every own online daemon; the CLI default still skips an unidentified one", func(t *testing.T) {
		cfg := fleetConfig(t, fleetDaemons, `[{"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *", "daemons": ["laptop", "server", "box"]}]`)

		f := newFleet()
		opts := fleetOpts()
		opts.LocalDaemon = DaemonIdentity{}
		opts.NoLocalDaemon = true
		p := planOf(t, cfg, f, opts)
		for d, id := range map[string]string{"laptop": "d-local", "server": "d-server", "box": "d-box"} {
			if c := cronChange(t, p, d, "nightly"); c.Action != ActionCreate {
				t.Fatalf("%s: %+v", d, c)
			}
			if op := remoteOp(t, p, d, "nightly"); op.daemonID != id {
				t.Fatalf("%s: create aimed at %q", d, op.daemonID)
			}
		}

		cli := newFleet()
		opts = fleetOpts()
		opts.LocalDaemon = DaemonIdentity{}
		p = planOf(t, cfg, cli, opts)
		for _, d := range []string{"laptop", "server", "box"} {
			if c := cronChange(t, p, d, "nightly"); c.Action != ActionSkip || c.Reason != ReasonLocalDaemonUnidentified {
				t.Fatalf("%s without NoLocalDaemon: %+v", d, c)
			}
		}
		assertNoRemoteOps(t, p)
	})

	t.Run("fleet classification", func(t *testing.T) {
		f := newFleet()
		f.own = append(f.own,
			&pb.DaemonInfo{DaemonId: "d-off", Hostname: "off", Online: false},
			&pb.DaemonInfo{DaemonId: "d-twin1", Hostname: "twin", Online: true},
			&pb.DaemonInfo{DaemonId: "d-twin2", Hostname: "twin", Online: true},
			&pb.DaemonInfo{DaemonId: "d-bare", Hostname: "bare", Online: true},
		)
		f.peers = []*pb.DaemonRef{{DaemonId: "d-peer", Hostname: "peer"}}
		daemons := `[{"name": "laptop", "daemonId": "d-local", "path": "~/w"},
		  {"name": "off", "daemonId": "d-off", "path": "~/w"},
		  {"name": "twin", "path": "~/w"},
		  {"name": "peer", "daemonId": "d-peer", "path": "~/w"},
		  {"name": "ghost", "daemonId": "d-ghost", "path": "~/w"},
		  {"name": "bare", "daemonId": "d-bare", "path": "~/w"}]`
		cfg := fleetConfig(t, daemons, `[{"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *", "daemons": ["off", "twin", "peer", "ghost", "bare"]}]`)
		p := planOf(t, cfg, f, fleetOpts())
		want := map[string][2]string{
			"off":   {string(ActionSkip), ReasonDaemonOffline},
			"twin":  {string(ActionConflict), ReasonAmbiguous},
			"peer":  {string(ActionSkip), ReasonNotYourDaemon},
			"ghost": {string(ActionSkip), ReasonDaemonUnmatched},
			"bare":  {string(ActionSkip), ReasonRepoNotRegistered},
		}
		for d, w := range want {
			if c := cronChange(t, p, d, "nightly"); string(c.Action) != w[0] || c.Reason != w[1] {
				t.Fatalf("%s: %s [%s], want %s [%s]", d, c.Action, c.Reason, w[0], w[1])
			}
		}
		assertNoRemoteOps(t, p)
		if !p.Blocked() || p.Converged() {
			t.Fatal("skips and conflicts must block convergence")
		}
	})

	t.Run("remote rows follow the local cron rules", func(t *testing.T) {
		f := newFleet()
		f.cronRows = []*pb.CronJobWithDaemon{
			cronRowOn("d-server", "r-server", "nightly", "/old", "0 3 * * *", ""),
			cronRowOn("d-server", "r-server", "foreign", "/f", "0 3 * * *", "boss-project:other"),
			cronRowOn("d-server", "r-server", "dropped", "/d", "0 3 * * *", cloudMarker),
			cronRowOn("d-server", "r-server", "my build", "/boss-build", "*/15 * * * *", ""),
			cronRowOn("d-server", "r-other", "elsewhere", "/x", "0 3 * * *", cloudMarker),
			cronRowOn("d-box", "r-box", "nightly", "/nightly", "0 3 * * *", cloudMarker),
		}
		cfg := fleetConfig(t, fleetDaemons, `[{"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *", "daemons": ["server", "box"]},
		  {"name": "foreign", "prompt": "/f", "schedule": "0 3 * * *", "daemons": ["server"]},
		  {"name": "factory-build", "factoryStage": "build", "daemons": ["server"]}]`)
		p := planOf(t, cfg, f, fleetOpts())

		if c := cronChange(t, p, "server", "nightly"); c.Action != ActionAdopt {
			t.Fatalf("unmanaged same name: %+v", c)
		}
		if req := remoteOp(t, p, "server", "nightly").cron.updateCron; req.GetManagedBy() != cloudMarker || req.GetPrompt() != "/nightly" {
			t.Fatalf("adoption request %+v", req)
		}
		if c := cronChange(t, p, "server", "foreign"); c.Action != ActionConflict || c.Reason != ReasonManagedByOther {
			t.Fatalf("foreign marker: %+v", c)
		}
		if c := cronChange(t, p, "server", "dropped"); c.Action != ActionDisable {
			t.Fatalf("dropped own row: %+v", c)
		}
		if req := remoteOp(t, p, "server", "dropped").cron.updateCron; req.IsEnabled == nil || req.GetIsEnabled() || req.ManagedBy != nil {
			t.Fatalf("disable request %+v", req)
		}
		if c := cronChange(t, p, "server", "factory-build"); c.Action != ActionAdopt || c.Detail == "" {
			t.Fatalf("hand-named /boss-build row: %+v", c)
		}
		if c := cronChange(t, p, "box", "nightly"); c.Action != ActionUnchanged {
			t.Fatalf("box: %+v", c)
		}
		for _, c := range p.Changes {
			if c.Identity == "elsewhere" {
				t.Fatalf("a row of another repo on the daemon was planned: %+v", c)
			}
		}

		pruned := planOf(t, cfg, f, func() CloudOptions { o := fleetOpts(); o.Prune = true; return o }())
		if c := cronChange(t, pruned, "server", "dropped"); c.Action != ActionDelete {
			t.Fatalf("dropped own row with --prune: %+v", c)
		}
	})

	t.Run("factory crons: no remote create, existing rows converge all but the gate", func(t *testing.T) {
		f := newFleet()
		live := cronRowOn("d-box", "r-box", "factory-build", "/boss-build", "*/15 * * * *", cloudMarker)
		live.Job.GateCommand = `node "$HOME/.claude/skills/boss-build/toolbox/cron-gates/boss-build.mjs"`
		f.cronRows = []*pb.CronJobWithDaemon{live}
		cfg := fleetConfig(t, fleetDaemons, `[{"name": "factory-plan", "factoryStage": "plan", "daemons": ["server"]},
		  {"name": "factory-build", "factoryStage": "build", "schedule": "*/30 * * * *", "daemons": ["box"]}]`)
		p := planOf(t, cfg, f, fleetOpts())
		if c := cronChange(t, p, "server", "factory-plan"); c.Action != ActionSkip || c.Reason != ReasonNeedsLocalApply {
			t.Fatalf("factory cron with no row: %+v", c)
		}
		c := cronChange(t, p, "box", "factory-build")
		if c.Action != ActionUpdate || len(c.Fields) != 1 || c.Fields[0].Field != "schedule" {
			t.Fatalf("factory schedule change: %+v", c)
		}
		req := remoteOp(t, p, "box", "factory-build").cron.updateCron
		if req.GetSchedule() != "*/30 * * * *" || req.GateCommand != nil || req.Prompt != nil {
			t.Fatalf("factory update %+v, want the schedule only", req)
		}
		if !slices.ContainsFunc(p.ops, func(op cloudOp) bool { return op.daemonID == "d-box" }) || slices.ContainsFunc(p.ops, func(op cloudOp) bool { return op.daemonID == "d-server" }) {
			t.Fatal("only the existing factory row may be written")
		}
	})
}

// --- Apply -------------------------------------------------------------------

func TestApplyCloudCrons(t *testing.T) {
	twoDaemons := `[{"name": "a", "prompt": "/a", "schedule": "0 1 * * *", "daemons": ["server"]},
	  {"name": "b", "prompt": "/b", "schedule": "0 2 * * *", "daemons": ["server"]},
	  {"name": "c", "prompt": "/c", "schedule": "0 3 * * *", "daemons": ["box"]}]`

	t.Run("Unavailable stops only that daemon", func(t *testing.T) {
		f := newFleet()
		f.failOn = map[string]error{"create-cron d-server a": connect.NewError(connect.CodeUnavailable, errors.New("daemon not ready"))}
		_, res, err := applyCloud(t, fleetConfig(t, fleetDaemons, twoDaemons), f, fleetOpts())
		if err == nil {
			t.Fatal("a failed write must be an error")
		}
		if got, want := f.writes(), []string{"create-cron d-box c", "create-cron d-server a"}; !slices.Equal(got, want) {
			t.Fatalf("writes %q, want %q", got, want)
		}
		if a, r, _ := failedReason(res, "a"); a != ActionSkip || r != ReasonDaemonOffline {
			t.Fatalf("a: %s [%s]", a, r)
		}
		if len(res.NotReached) != 1 || res.NotReached[0].Identity != "b" || len(res.Applied) != 1 || res.Applied[0].Identity != "c" {
			t.Fatalf("result %+v", res)
		}
	})

	t.Run("a marker-less echo is daemon-too-old for that daemon only", func(t *testing.T) {
		f := newFleet()
		f.oldDaemons = map[string]bool{"d-server": true}
		_, res, err := applyCloud(t, fleetConfig(t, fleetDaemons, twoDaemons), f, fleetOpts())
		if err == nil {
			t.Fatal("a marker-less echo must be an error")
		}
		if a, r, _ := failedReason(res, "a"); a != ActionSkip || r != ReasonDaemonTooOld {
			t.Fatalf("a: %s [%s]", a, r)
		}
		if len(res.NotReached) != 1 || res.NotReached[0].Identity != "b" || len(res.Applied) != 1 || res.Applied[0].Identity != "c" {
			t.Fatalf("result %+v", res)
		}
	})

	t.Run("remote crons run after triggers and webhooks", func(t *testing.T) {
		f := newFleet()
		cfg := fleetConfig(t, fleetDaemons, twoDaemons)
		cfg.Cloud = &Cloud{Triggers: cloudConfig(t, `{"triggers": [`+httpTrigger+`]}`).Cloud.Triggers,
			Webhooks: []Webhook{{URL: "https://hooks.example.com/a", EventTypes: []string{"session.merged"}}}}
		_, _, err := applyCloud(t, cfg, f, fleetOpts())
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		want := []string{"create-trigger deploy", "create-webhook https://hooks.example.com/a", "create-cron d-box c", "create-cron d-server a", "create-cron d-server b"}
		if got := f.writes(); !slices.Equal(got, want) {
			t.Fatalf("writes %q, want %q", got, want)
		}
		for _, r := range f.cronRows {
			if r.GetJob().GetManagedBy() != cloudMarker {
				t.Fatalf("row %q stored without the marker", r.GetJob().GetName())
			}
		}
	})
}
