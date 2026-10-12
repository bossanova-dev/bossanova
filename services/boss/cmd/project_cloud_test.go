package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// fakeProjectOrchestrator is an in-memory bosso for `boss project --cloud`:
// it stores triggers and webhooks with the server's defaults and echoes the
// marker. requests counts every request; writes lists each write in order.
type fakeProjectOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	mu          sync.Mutex
	requests    atomic.Int32
	writes      []string
	triggers    []*pb.Trigger
	webhooks    []*pb.SessionWebhook
	nextID      int
	paidDenied  bool
	ownerDenied bool

	// The cron fan-out fleet: when own is set, ListDaemons serves own and
	// peers, and the cron proxies store rows per daemon.
	own   []*pb.DaemonInfo
	peers []*pb.DaemonRef
	repos []*pb.AggregatedRepo
	crons []*pb.CronJobWithDaemon
}

func (f *fakeProjectOrchestrator) write(entry string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, entry)
}

func (f *fakeProjectOrchestrator) writeLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

func (f *fakeProjectOrchestrator) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeProjectOrchestrator) paidGate() error {
	f.requests.Add(1)
	if f.paidDenied {
		return connect.NewError(connect.CodePermissionDenied, errors.New("cloud access requires a paid plan"))
	}
	return nil
}

func (f *fakeProjectOrchestrator) ownerGate() error {
	if err := f.paidGate(); err != nil {
		return err
	}
	if f.ownerDenied {
		return connect.NewError(connect.CodePermissionDenied, errors.New("organization owner role required"))
	}
	return nil
}

func (f *fakeProjectOrchestrator) ListOrganizations(context.Context, *connect.Request[pb.ListOrganizationsRequest]) (*connect.Response[pb.ListOrganizationsResponse], error) {
	f.requests.Add(1)
	return connect.NewResponse(&pb.ListOrganizationsResponse{Organizations: []*pb.Organization{{Id: "org-1", Name: "Acme"}}}), nil
}

func (f *fakeProjectOrchestrator) GetRepoOrganization(context.Context, *connect.Request[pb.GetRepoOrganizationRequest]) (*connect.Response[pb.GetRepoOrganizationResponse], error) {
	f.requests.Add(1)
	return connect.NewResponse(&pb.GetRepoOrganizationResponse{Mapping: &pb.RepoOrganizationMapping{OrganizationId: "org-1"}}), nil
}

func (f *fakeProjectOrchestrator) ListDaemons(context.Context, *connect.Request[pb.ListDaemonsRequest]) (*connect.Response[pb.ListDaemonsResponse], error) {
	f.requests.Add(1)
	if f.own != nil {
		return connect.NewResponse(&pb.ListDaemonsResponse{Daemons: f.own, SessionDaemons: f.peers}), nil
	}
	return connect.NewResponse(&pb.ListDaemonsResponse{Daemons: []*pb.DaemonInfo{{DaemonId: "d-test", Hostname: "test-daemon"}}}), nil
}

func (f *fakeProjectOrchestrator) ProxyListReposAggregated(context.Context, *connect.Request[pb.ProxyListReposAggregatedRequest]) (*connect.Response[pb.ProxyListReposAggregatedResponse], error) {
	if err := f.paidGate(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.ProxyListReposAggregatedResponse{Repos: f.repos}), nil
}

func (f *fakeProjectOrchestrator) ProxyListCronJobs(context.Context, *connect.Request[pb.ProxyListCronJobsRequest]) (*connect.Response[pb.ProxyListCronJobsResponse], error) {
	if err := f.paidGate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &pb.ProxyListCronJobsResponse{}
	for _, c := range f.crons {
		out.Jobs = append(out.Jobs, proto.CloneOf(c))
	}
	return connect.NewResponse(out), nil
}

// ProxyCreateCronJob stores the row on the named daemon and echoes the marker,
// as a current bossd does.
func (f *fakeProjectOrchestrator) ProxyCreateCronJob(_ context.Context, req *connect.Request[pb.ProxyCreateCronJobRequest]) (*connect.Response[pb.ProxyCreateCronJobResponse], error) {
	if err := f.paidGate(); err != nil {
		return nil, err
	}
	m := req.Msg
	f.write("create-cron " + m.GetDaemonId() + " " + m.GetName() + " " + m.GetManagedBy())
	f.mu.Lock()
	defer f.mu.Unlock()
	job := &pb.CronJob{Id: f.id("cj"), RepoId: m.GetRepoId(), Name: m.GetName(), Prompt: m.GetPrompt(), Schedule: m.GetSchedule(),
		Timezone: m.GetTimezone(), IsEnabled: m.GetIsEnabled(), AgentName: m.GetAgentName(), Model: m.GetModel(),
		GateCommand: m.GetGateCommand(), ManagedBy: m.GetManagedBy()}
	f.crons = append(f.crons, &pb.CronJobWithDaemon{Job: job, DaemonId: m.GetDaemonId()})
	return connect.NewResponse(&pb.ProxyCreateCronJobResponse{Job: proto.CloneOf(job)}), nil
}

func (f *fakeProjectOrchestrator) ProxyUpdateCronJob(_ context.Context, req *connect.Request[pb.ProxyUpdateCronJobRequest]) (*connect.Response[pb.ProxyUpdateCronJobResponse], error) {
	if err := f.paidGate(); err != nil {
		return nil, err
	}
	f.write("update-cron " + req.Msg.GetDaemonId() + " " + req.Msg.GetId())
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this fake creates only"))
}

func (f *fakeProjectOrchestrator) ListTriggers(context.Context, *connect.Request[pb.ListTriggersRequest]) (*connect.Response[pb.ListTriggersResponse], error) {
	if err := f.paidGate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &pb.ListTriggersResponse{}
	for _, t := range f.triggers {
		out.Triggers = append(out.Triggers, proto.CloneOf(t))
	}
	return connect.NewResponse(out), nil
}

func (f *fakeProjectOrchestrator) CreateTrigger(_ context.Context, req *connect.Request[pb.CreateTriggerRequest]) (*connect.Response[pb.CreateTriggerResponse], error) {
	if err := f.paidGate(); err != nil {
		return nil, err
	}
	m := req.Msg
	f.write("create-trigger " + m.GetName())
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &pb.Trigger{Id: f.id("tr"), OrganizationId: m.GetOrganizationId(), Name: m.GetName(), IsEnabled: m.GetIsEnabled(), TriggerType: m.GetTriggerType(),
		RepoOriginUrl: m.GetRepoOriginUrl(), Launch: m.GetLaunch(), Placement: m.GetPlacement(), ConcurrencyPolicy: m.GetConcurrencyPolicy(),
		CooldownSeconds: m.GetCooldownSeconds(), Filters: m.GetFilters(), PayloadFields: m.GetPayloadFields(), Gate: m.GetGate(), ManagedBy: m.GetManagedBy()}
	resp := &pb.CreateTriggerResponse{}
	switch cfg := m.GetTypeConfig().(type) {
	case *pb.CreateTriggerRequest_Http:
		t.TypeConfig = &pb.Trigger_Http{Http: cfg.Http}
		resp.Secret = "bstrg_minted_" + m.GetName()
	case *pb.CreateTriggerRequest_Github:
		t.TypeConfig = &pb.Trigger_Github{Github: cfg.Github}
	}
	f.triggers = append(f.triggers, t)
	resp.Trigger = proto.CloneOf(t)
	return connect.NewResponse(resp), nil
}

func (f *fakeProjectOrchestrator) UpdateTrigger(_ context.Context, req *connect.Request[pb.UpdateTriggerRequest]) (*connect.Response[pb.UpdateTriggerResponse], error) {
	if err := f.paidGate(); err != nil {
		return nil, err
	}
	f.write("update-trigger " + req.Msg.GetId())
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this fake creates only"))
}

func (f *fakeProjectOrchestrator) ListSessionWebhooks(context.Context, *connect.Request[pb.ListSessionWebhooksRequest]) (*connect.Response[pb.ListSessionWebhooksResponse], error) {
	if err := f.ownerGate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &pb.ListSessionWebhooksResponse{}
	for _, w := range f.webhooks {
		out.Webhooks = append(out.Webhooks, proto.CloneOf(w))
	}
	return connect.NewResponse(out), nil
}

func (f *fakeProjectOrchestrator) CreateSessionWebhook(_ context.Context, req *connect.Request[pb.CreateSessionWebhookRequest]) (*connect.Response[pb.CreateSessionWebhookResponse], error) {
	if err := f.ownerGate(); err != nil {
		return nil, err
	}
	m := req.Msg
	f.write("create-webhook " + m.GetUrl())
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &pb.SessionWebhook{Id: f.id("wh"), OrganizationId: m.GetOrganizationId(), Url: m.GetUrl(), Description: m.GetDescription(),
		EventTypes: m.GetEventTypes(), IsEnabled: m.GetIsEnabled(), ManagedBy: m.GetManagedBy(), Format: "raw"}
	f.webhooks = append(f.webhooks, w)
	secret := m.GetSecret()
	if m.Secret == nil {
		secret = "whsec_minted_webhook"
	}
	return connect.NewResponse(&pb.CreateSessionWebhookResponse{Webhook: proto.CloneOf(w), Secret: secret}), nil
}

// projectCloudFile is a one-daemon project file (no crons) with a cloud
// section.
func (f *applyFixture) projectCloudFile(cloud string) string {
	return strings.TrimSuffix(f.projectFile("", ""), "}\n") + `, "cloud": ` + cloud + "}\n"
}

const projectCloudSection = `{
  "triggers": [
    {"name": "deploy", "type": "http", "launch": {"promptTemplate": "Deploy {{event.type}}"}, "placement": {"mode": "first-available"}},
    {"name": "audit", "type": "github", "github": {"eventTypes": ["push"]}, "launch": {"promptTemplate": "Audit"},
     "placement": {"mode": "specific-daemon", "daemon": "test-daemon"}}
  ],
  "webhooks": [{"url": "https://hooks.example.com/boss", "eventTypes": ["session.merged"]}]
}`

// withFakeCloud points the fixture's --cloud client at fake through a real
// RemoteClient and returns fake.
func (f *applyFixture) withFakeCloud(t *testing.T, fake *fakeProjectOrchestrator) *fakeProjectOrchestrator {
	t.Helper()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.env.cloudApply = func(*cobra.Command) (projectCloudApplyClient, error) { return client.NewRemote(srv.URL, "tok"), nil }
	return fake
}

// runSplit runs `boss project <args>` with stdout and stderr kept apart.
func (f *applyFixture) runSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	prev := newProjectApplyEnv
	newProjectApplyEnv = func() projectApplyEnv { return f.env }
	t.Cleanup(func() { newProjectApplyEnv = prev })
	cmd := projectCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(append(args, "--dir", f.dir))
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func TestProjectApplyCloudConverges(t *testing.T) {
	f := newApplyFixture(t)
	fake := f.withFakeCloud(t, &fakeProjectOrchestrator{})
	f.write(t, f.projectCloudFile(projectCloudSection))

	stdout, stderr, err := f.runSplit(t, "apply", "--cloud", "--yes", "--json")
	if err != nil {
		t.Fatalf("apply --cloud: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	want := []string{"create-trigger audit", "create-trigger deploy", "create-webhook https://hooks.example.com/boss"}
	if got := fake.writeLog(); !slices.Equal(got, want) {
		t.Fatalf("cloud writes %q, want %q", got, want)
	}
	for _, tr := range fake.triggers {
		if tr.GetManagedBy() != projectApplyMarker || tr.GetRepoOriginUrl() != "https://github.com/acme/widget" {
			t.Fatalf("trigger %q stored as %+v", tr.GetName(), tr)
		}
		if tr.GetName() == "audit" && tr.GetPlacement().GetDaemonId() != "d-test" {
			t.Fatalf("audit placement %+v, want the caller's daemon d-test", tr.GetPlacement())
		}
	}
	if fake.webhooks[0].GetManagedBy() != projectApplyMarker {
		t.Fatalf("webhook stored as %+v", fake.webhooks[0])
	}
	if !strings.Contains(stderr, "bstrg_minted_deploy") || !strings.Contains(stderr, "whsec_minted_webhook") {
		t.Fatalf("minted secrets missing from stderr:\n%s", stderr)
	}
	if strings.Contains(stdout, "minted") {
		t.Fatalf("a minted secret reached stdout:\n%s", stdout)
	}
	var report struct {
		Plan        json.RawMessage `json:"plan"`
		CloudResult *struct {
			Applied []json.RawMessage `json:"applied"`
		} `json:"cloudResult"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || report.CloudResult == nil || len(report.CloudResult.Applied) != 3 {
		t.Fatalf("--json report %v:\n%s", err, stdout)
	}

	stdout, stderr, err = f.runSplit(t, "diff", "--cloud", "--exit-code")
	if code := exitCodeOf(err); code != 0 || !strings.Contains(stdout, "nothing to change") {
		t.Fatalf("diff --cloud after apply exits %d: %v\n%s%s", code, err, stdout, stderr)
	}

	cloudBefore, localBefore := len(fake.writeLog()), f.writes()
	if stdout, stderr, err := f.runSplit(t, "apply", "--cloud", "--yes"); err != nil || len(fake.writeLog()) != cloudBefore || f.writes() != localBefore {
		t.Fatalf("second apply wrote %d cloud / %d local: %v\n%s%s", len(fake.writeLog())-cloudBefore, f.writes()-localBefore, err, stdout, stderr)
	}
}

func TestProjectApplyCloudRefusals(t *testing.T) {
	t.Run("not logged in: refusal before any request", func(t *testing.T) {
		f := newApplyFixture(t)
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		mgr := auth.NewManager(emptyTokenStore{}, auth.Config{})
		f.env.cloudApply = func(cmd *cobra.Command) (projectCloudApplyClient, error) {
			remote, err := dialCloudRemote(cmd.Context(), mgr, srv.URL, errProjectApplyNotLoggedIn)
			if err != nil {
				return nil, err
			}
			return remote, nil
		}
		f.write(t, f.projectCloudFile(projectCloudSection))
		_, _, err := f.runSplit(t, "apply", "--cloud", "--yes")
		if err == nil || !strings.Contains(err.Error(), "boss login") {
			t.Fatalf("want the boss login refusal, got %v", err)
		}
		if calls.Load() != 0 || f.writes() != 0 {
			t.Fatalf("%d cloud request(s), %d daemon write(s) without a login", calls.Load(), f.writes())
		}
	})

	t.Run("unpaid: the precheck refuses and nothing is written on any leg", func(t *testing.T) {
		f := newApplyFixture(t)
		fake := f.withFakeCloud(t, &fakeProjectOrchestrator{paidDenied: true})
		f.write(t, f.projectCloudFile(projectCloudSection))
		_, _, err := f.runSplit(t, "apply", "--cloud", "--yes")
		if exitCodeOf(err) == 0 || !strings.Contains(err.Error(), "paid plan") {
			t.Fatalf("want a non-zero paid-plan refusal, got %v", err)
		}
		if len(fake.writeLog()) != 0 || f.writes() != 0 {
			t.Fatalf("writes after the refused precheck: cloud %q, daemon %d", fake.writeLog(), f.writes())
		}
	})

	t.Run("non-owner: triggers converge, webhooks skip owner-required, exit non-zero", func(t *testing.T) {
		f := newApplyFixture(t)
		fake := f.withFakeCloud(t, &fakeProjectOrchestrator{ownerDenied: true})
		f.write(t, f.projectCloudFile(projectCloudSection))
		stdout, _, err := f.runSplit(t, "apply", "--cloud", "--yes")
		if exitCodeOf(err) == 0 {
			t.Fatalf("a skipped webhook must exit non-zero:\n%s", stdout)
		}
		if got := fake.writeLog(); !slices.Equal(got, []string{"create-trigger audit", "create-trigger deploy"}) {
			t.Fatalf("cloud writes %q, want both triggers and no webhook", got)
		}
		if !strings.Contains(stdout, `skip webhook "https://hooks.example.com/boss" [owner-required]`) {
			t.Fatalf("plan does not report owner-required:\n%s", stdout)
		}
	})

	t.Run("non-TTY with a trigger create and no --yes writes nothing", func(t *testing.T) {
		f := newApplyFixture(t)
		fake := f.withFakeCloud(t, &fakeProjectOrchestrator{})
		f.write(t, f.projectCloudFile(projectCloudSection))
		_, _, err := f.runSplit(t, "apply", "--cloud")
		if err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("want the --yes refusal, got %v", err)
		}
		if len(fake.writeLog()) != 0 || f.writes() != 0 {
			t.Fatalf("writes without --yes: cloud %q, daemon %d", fake.writeLog(), f.writes())
		}
	})

	t.Run("--org disagreeing with the file is an error before any request", func(t *testing.T) {
		f := newApplyFixture(t)
		fake := f.withFakeCloud(t, &fakeProjectOrchestrator{})
		f.write(t, f.projectCloudFile(`{"organization": "org-1", "triggers": []}`))
		_, _, err := f.runSplit(t, "apply", "--cloud", "--org", "org-2", "--yes")
		if err == nil || !strings.Contains(err.Error(), "disagrees") {
			t.Fatalf("want the --org disagreement, got %v", err)
		}
		if fake.requests.Load() != 0 || f.writes() != 0 {
			t.Fatalf("%d request(s), %d daemon write(s) after a flag error", fake.requests.Load(), f.writes())
		}
	})
}

// fanOutFile places one cron on this daemon and one on each of host-a, host-b
// and host-c; withSkipped false drops host-b's and host-c's crons.
func (f *applyFixture) fanOutFile(withSkipped bool) string {
	crons := `{"name": "local-nightly", "prompt": "/local", "schedule": "0 1 * * *", "daemons": ["test-daemon"]},
    {"name": "a-nightly", "prompt": "/a", "schedule": "0 2 * * *", "daemons": ["host-a"]}`
	if withSkipped {
		crons += `,
    {"name": "b-nightly", "prompt": "/b", "schedule": "0 3 * * *", "daemons": ["host-b"]},
    {"name": "c-nightly", "prompt": "/c", "schedule": "0 4 * * *", "daemons": ["host-c"]}`
	}
	return `{"version": 1, "project": "widget", "repo": {"originUrl": "https://github.com/acme/widget"},
  "daemons": [{"name": "test-daemon", "daemonId": "d-test", "path": "` + f.main + `"},
    {"name": "host-a", "daemonId": "d-a", "path": "~/src/widget"},
    {"name": "host-b", "daemonId": "d-b", "path": "~/src/widget"},
    {"name": "host-c", "daemonId": "d-c", "path": "~/src/widget"}],
  "crons": [` + crons + `]}
`
}

// TestProjectApplyCloudFanOut runs `boss project apply --cloud` against a
// fleet of this daemon, an online own daemon A serving the repo, an offline
// own daemon B and another member's daemon C.
func TestProjectApplyCloudFanOut(t *testing.T) {
	f := newApplyFixture(t)
	fake := f.withFakeCloud(t, &fakeProjectOrchestrator{
		own: []*pb.DaemonInfo{
			{DaemonId: "d-test", Hostname: "test-daemon", Online: true},
			{DaemonId: "d-a", Hostname: "host-a", Online: true},
			{DaemonId: "d-b", Hostname: "host-b", Online: false},
		},
		peers: []*pb.DaemonRef{{DaemonId: "d-c", Hostname: "host-c", IsOnline: true}},
		repos: []*pb.AggregatedRepo{{OriginUrl: "https://github.com/acme/widget", Daemons: []*pb.DaemonRepoRef{
			{DaemonId: "d-test", RepoId: "repo-local"}, {DaemonId: "d-a", RepoId: "repo-a"}, {DaemonId: "d-b", RepoId: "repo-b"},
		}}},
	})
	f.write(t, f.fanOutFile(true))

	stdout, stderr, err := f.runSplit(t, "apply", "--cloud", "--yes")
	if exitCodeOf(err) == 0 {
		t.Fatalf("skipped crons must exit non-zero:\n%s%s", stdout, stderr)
	}
	if got, want := fake.writeLog(), []string{"create-cron d-a a-nightly " + projectApplyMarker}; !slices.Equal(got, want) {
		t.Fatalf("cloud writes %q, want %q\n%s", got, want, stdout)
	}
	if fake.crons[0].GetJob().GetRepoId() != "repo-a" {
		t.Fatalf("A's cron stored as %+v, want repo-a", fake.crons[0])
	}
	creates := f.daemon.CreateCronJobCalls()
	if len(creates) != 1 || creates[0].GetName() != "local-nightly" || creates[0].GetManagedBy() != projectApplyMarker {
		t.Fatalf("socket creates %+v, want only local-nightly", creates)
	}
	for _, want := range []string{
		`skip cron "b-nightly" on "host-b" [daemon-offline]`,
		`skip cron "c-nightly" on "host-c" [not-your-daemon]`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("plan lacks %q:\n%s", want, stdout)
		}
	}

	stdout, stderr, err = f.runSplit(t, "diff", "--cloud", "--exit-code")
	if code := exitCodeOf(err); code != 1 {
		t.Fatalf("diff --cloud with B and C skipped exits %d: %v\n%s%s", code, err, stdout, stderr)
	}
	if strings.Contains(stdout, "a-nightly") || strings.Contains(stdout, "local-nightly") {
		t.Fatalf("applied crons are not converged:\n%s", stdout)
	}

	f.write(t, f.fanOutFile(false))
	stdout, stderr, err = f.runSplit(t, "diff", "--cloud", "--exit-code")
	if code := exitCodeOf(err); code != 0 || !strings.Contains(stdout, "nothing to change") {
		t.Fatalf("diff --cloud without B and C exits %d: %v\n%s%s", code, err, stdout, stderr)
	}
	if len(fake.writeLog()) != 1 || len(f.daemon.CreateCronJobCalls()) != 1 {
		t.Fatalf("diff wrote: cloud %q, socket creates %d", fake.writeLog(), len(f.daemon.CreateCronJobCalls()))
	}
}
