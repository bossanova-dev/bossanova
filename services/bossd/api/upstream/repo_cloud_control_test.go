package upstream

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

// A read-only machine with a full grant for repo a publishes the configured
// default and each repository's effective mode separately, and stamps the
// effective mode on every session it sends.
func TestSnapshotPublishesRepoCloudControl_Sessions_AC238202(t *testing.T) {
	client, _, _ := targetClient()
	client.stores = StreamStores{
		Repos:    &fakeRepoReader{ids: []string{"a", "b"}},
		Sessions: &fakeSessionReader{sessions: []*pb.Session{{Id: "sa", RepoId: "a"}, {Id: "sb", RepoId: "b"}}},
	}
	snap, err := client.buildSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.GetIsRepoCloudControlSupported() || snap.GetCloudControl() != pb.CloudControl_CLOUD_CONTROL_READ_ONLY {
		t.Fatalf("snapshot flag/default = %v/%v", snap.GetIsRepoCloudControlSupported(), snap.GetCloudControl())
	}
	want := map[string]pb.CloudControl{"a": pb.CloudControl_CLOUD_CONTROL_FULL, "b": pb.CloudControl_CLOUD_CONTROL_READ_ONLY}
	if got := repoModeMap(snap.GetRepoCloudControls()); len(got) != 2 || got["a"] != want["a"] || got["b"] != want["b"] {
		t.Fatalf("repo modes = %v", got)
	}
	for _, session := range snap.GetSessions() {
		if session.GetCloudControl() != want[session.GetRepoId()] {
			t.Fatalf("session %s mode = %v", session.GetId(), session.GetCloudControl())
		}
	}
	if snap.GetRepoCloudControlRevision() == 0 {
		t.Fatal("snapshot carries no revision")
	}

	out := make(chan *pb.DaemonEvent, 1)
	client.forwardEvent(t.Context(), StreamEvent{Session: &SessionEvent{Kind: pb.SessionDelta_KIND_UPDATED, Session: &pb.Session{Id: "sb", RepoId: "b"}}}, out, nil)
	if got := (<-out).GetSession().GetSession().GetCloudControl(); got != pb.CloudControl_CLOUD_CONTROL_READ_ONLY {
		t.Fatalf("session delta mode = %v", got)
	}
}

// originURLSnapshotClient mirrors production, where the snapshot repo reader
// returns canonical origin URLs (for webhook routing) while grants, sessions
// and bosso's command targets name repositories by their database ids.
func originURLSnapshotClient() (*StreamClient, []string) {
	client, _, _ := targetClient()
	urls := []string{"https://github.com/acme/granted", "https://github.com/acme/protected"}
	client.stores = StreamStores{
		Repos:    &fakeRepoReader{ids: urls},
		Sessions: &fakeSessionReader{sessions: []*pb.Session{{Id: "sa", RepoId: "a", Plan: "plan"}, {Id: "sb", RepoId: "b", Plan: "plan"}}},
	}
	return client, urls
}

// The per-repo modes are keyed by database repo id, the key bosso resolves a
// command target by, while RepoIds keeps the origin URLs webhooks route by.
func TestSnapshotRepoCloudControlKeyedByRepoID(t *testing.T) {
	client, urls := originURLSnapshotClient()
	snap, err := client.buildSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(snap.GetRepoIds(), urls) {
		t.Fatalf("repo ids = %v, want origin URLs %v", snap.GetRepoIds(), urls)
	}
	got := repoModeMap(snap.GetRepoCloudControls())
	if len(got) != 2 || got["a"] != pb.CloudControl_CLOUD_CONTROL_FULL || got["b"] != pb.CloudControl_CLOUD_CONTROL_READ_ONLY {
		t.Fatalf("repo modes = %v", got)
	}
}

func TestBuildCloudSnapshotKeysRepoCloudControlByRepoID_Sessions_AC238202(t *testing.T) {
	client, urls := originURLSnapshotClient()
	snap, err := BuildCloudSnapshot(t.Context(), client.stores, client.cloudControl, client.targetObjects)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(snap.GetRepoIds(), urls) {
		t.Fatalf("repo ids = %v, want origin URLs %v", snap.GetRepoIds(), urls)
	}
	want := map[string]pb.CloudControl{"a": pb.CloudControl_CLOUD_CONTROL_FULL, "b": pb.CloudControl_CLOUD_CONTROL_READ_ONLY}
	if got := repoModeMap(snap.GetRepoCloudControls()); len(got) != 2 || got["a"] != want["a"] || got["b"] != want["b"] {
		t.Fatalf("repo modes = %v", got)
	}
	if len(snap.GetSessions()) != 2 {
		t.Fatalf("sessions = %v", snap.GetSessions())
	}
	for _, session := range snap.GetSessions() {
		if session.GetCloudControl() != want[session.GetRepoId()] || session.GetPlan() != "plan" {
			t.Fatalf("session %s mode/plan = %v/%q", session.GetId(), session.GetCloudControl(), session.GetPlan())
		}
	}
}

func TestBuildCloudSnapshotWithoutTargetsFailsClosed(t *testing.T) {
	client, _ := originURLSnapshotClient()
	snap, err := BuildCloudSnapshot(t.Context(), client.stores, client.cloudControl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GetRepoCloudControls()) != 0 {
		t.Fatalf("repo modes without targets = %v", snap.GetRepoCloudControls())
	}
	if len(snap.GetSessions()) != 2 {
		t.Fatalf("sessions = %v", snap.GetSessions())
	}
	for _, session := range snap.GetSessions() {
		if session.GetCloudControl() != pb.CloudControl_CLOUD_CONTROL_PRIVATE || session.GetPlan() != "" {
			t.Fatalf("session %s mode/plan = %v/%q", session.GetId(), session.GetCloudControl(), session.GetPlan())
		}
	}
}

// The stream writer keeps modes and plans instead of registering origin URLs.
func TestWriterSnapshotSessionsKeepRepoCloudControl(t *testing.T) {
	client, _ := originURLSnapshotClient()
	snap, err := client.buildSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan *pb.DaemonEvent, 1)
	out <- &pb.DaemonEvent{Event: &pb.DaemonEvent_Snapshot{Snapshot: snap}}
	close(out)
	stream := &policyDaemonStream{sent: make(chan *pb.DaemonEvent, 1)}
	client.runWriter(t.Context(), stream, out)
	want := map[string]pb.CloudControl{"a": pb.CloudControl_CLOUD_CONTROL_FULL, "b": pb.CloudControl_CLOUD_CONTROL_READ_ONLY}
	sessions := (<-stream.sent).GetSnapshot().GetSessions()
	if len(sessions) != 2 {
		t.Fatalf("sessions = %v", sessions)
	}
	for _, session := range sessions {
		if session.GetCloudControl() != want[session.GetRepoId()] || session.GetPlan() != "plan" {
			t.Fatalf("session %s mode/plan = %v/%q", session.GetId(), session.GetCloudControl(), session.GetPlan())
		}
	}
}

// A local grant change publishes RepoCloudControlsChanged with a newer revision
// than the connect snapshot, naming the changed repository and any removed one.
func TestWatchCloudControlPublishesRepoDelta(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, _, fixture := targetClient()
		targets := &lockedTargets{fixture: fixture}
		policy := &grantPolicy{repos: map[string]config.CloudControlMode{"a": config.CloudControlFull}}
		client.cloudControl, client.targetObjects = policy, targets
		client.stores = StreamStores{Repos: targets}
		snap, err := client.buildSnapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		initial, err := client.repoCloudControl(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		out := make(chan *pb.DaemonEvent, 4)
		done := make(chan struct{})
		go func() {
			defer close(done)
			client.watchCloudControl(ctx, snap.GetCloudControl(), out, initial)
		}()
		time.Sleep(2 * defaultCloudControlPollInterval)
		synctest.Wait()
		if ev := pendingEvent(out); ev != nil {
			t.Fatalf("unchanged modes published %v", ev)
		}

		policy.set("a", config.CloudControlReadOnly)
		targets.remove("b")
		time.Sleep(defaultCloudControlPollInterval)
		synctest.Wait()
		delta := pendingEvent(out).GetRepoCloudControlsChanged()
		if delta.GetRevision() <= snap.GetRepoCloudControlRevision() {
			t.Fatalf("delta revision %d not newer than snapshot %d (%v)", delta.GetRevision(), snap.GetRepoCloudControlRevision(), delta)
		}
		if up := delta.GetUpserted(); len(up) != 1 || up[0].GetRepoId() != "a" || up[0].GetCloudControl() != pb.CloudControl_CLOUD_CONTROL_READ_ONLY {
			t.Fatalf("upserted = %v", up)
		}
		if removed := delta.GetRemovedRepoIds(); len(removed) != 1 || removed[0] != "b" {
			t.Fatalf("removed = %v", removed)
		}
		resnap := pendingEvent(out).GetSnapshot()
		if resnap.GetRepoCloudControlRevision() != delta.GetRevision() || len(resnap.GetRepoCloudControls()) != 1 {
			t.Fatalf("follow-up snapshot = %v", resnap)
		}
		cancel()
		<-done
	})
}

func repoModeMap(modes []*pb.RepoCloudControlMode) map[string]pb.CloudControl {
	out := make(map[string]pb.CloudControl, len(modes))
	for _, mode := range modes {
		out[mode.GetRepoId()] = mode.GetCloudControl()
	}
	return out
}

// grantPolicy is a read-only machine whose per-repo grants change under test.
type grantPolicy struct {
	mu    sync.Mutex
	repos map[string]config.CloudControlMode
}

func (p *grantPolicy) Mode() config.CloudControlMode { return config.CloudControlReadOnly }
func (p *grantPolicy) RepoMode(_ context.Context, id string) (config.CloudControlMode, config.CloudControlSource) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if mode, ok := p.repos[id]; ok {
		return mode, config.CloudControlSourceGrant
	}
	return config.CloudControlReadOnly, config.CloudControlSourceMachine
}
func (p *grantPolicy) set(id string, mode config.CloudControlMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repos[id] = mode
}

// lockedTargets lets a test unregister a repository while the watcher reads.
type lockedTargets struct {
	mu      sync.Mutex
	fixture *targetFixture
}

func (l *lockedTargets) GetTarget(ctx context.Context, kind, id string) (CommandTargetObject, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fixture.GetTarget(ctx, kind, id)
}
func (l *lockedTargets) ListTargetRepos(ctx context.Context) ([]CommandTargetObject, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fixture.ListTargetRepos(ctx)
}
func (l *lockedTargets) SnapshotRepoIDs(ctx context.Context) ([]string, error) {
	repos, err := l.ListTargetRepos(ctx)
	ids := make([]string, 0, len(repos))
	for _, repo := range repos {
		ids = append(ids, repo.RepoID)
	}
	return ids, err
}
func (l *lockedTargets) remove(repoID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fixture.objects, targetKey{"repo", repoID})
}
