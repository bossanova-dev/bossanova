package taskorchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/verifyjob"
)

type handoffLease bool

func (l handoffLease) Active(string) bool { return bool(l) }

type handoffFactory struct{ enabled bool }

func (c *handoffFactory) OwnsBotMerges(context.Context, string, string) bool { return c.enabled }

func handoffFixture(t *testing.T) (*Orchestrator, *domain.TaskMapping, *pb.TaskItem, repoInfo, *atomic.Int32) {
	t.Helper()
	id := "repair-session"
	m := &domain.TaskMapping{ID: "m1", ExternalID: "dependabot:org/repo:42", PluginName: "dependabot", RepoID: "r", SessionID: &id, Status: domain.TaskMappingStatusInProgress, UpdatedAt: time.Now()}
	o := newTestOrchestrator()
	store := o.taskMappings.(*mockTaskMappingStore)
	store.mappings[m.ExternalID] = m
	store.bySession = map[string]*domain.TaskMapping{id: m}
	store.updateFn = func(_ context.Context, _ string, p db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
		if p.Status != nil {
			m.Status = *p.Status
		}
		return m, nil
	}
	o.active["r"] = true
	o.activeMapping["r"] = m.ID
	merges := &atomic.Int32{}
	o.provider = &mockProvider{mergeFn: func(context.Context, string, int) error { merges.Add(1); return nil }}
	task := &pb.TaskItem{ExternalId: m.ExternalID, Action: pb.TaskAction_TASK_ACTION_AUTO_MERGE}
	return o, m, task, repoInfo{id: "r", mergeStrategy: "rebase"}, merges
}
func TestGreenRepairHandoff_Sessions_AC125_AC126(t *testing.T) {
	for _, factory := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-verify", true: "verify"}[factory], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				o, m, task, repo, merges := handoffFixture(t)
				repo.verifyOwnsMerge = factory
				var archives atomic.Int32
				o.SetSessionArchiver(SessionArchiverFunc(func(context.Context, string) error { archives.Add(1); return nil }), nil)
				queued := 0
				o.sessionCreator = &mockSessionCreatorOrch{createFn: func(context.Context, CreateSessionOpts) (*models.Session, error) {
					queued++
					return &models.Session{ID: "next"}, nil
				}}
				o.queues[repo.id] = []queuedTask{{task: &pb.TaskItem{ExternalId: "other:43", Action: pb.TaskAction_TASK_ACTION_CREATE_SESSION}, repo: repo, pluginName: "other"}}
				// Preserve actual mapping status updates for each mapping, as the next task is independent.
				store := o.taskMappings.(*mockTaskMappingStore)
				store.updateFn = func(_ context.Context, id string, p db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
					if id == m.ID && p.Status != nil {
						m.Status = *p.Status
					}
					return m, nil
				}
				o.processTask(context.Background(), task, repo, "dependabot")
				synctest.Wait()
				want := int32(1)
				if factory {
					want = 0
				}
				if merges.Load() != want || queued != 1 || *m.SessionID != "repair-session" {
					t.Fatalf("merges=%d queued=%d mapping=%+v", merges.Load(), queued, m)
				}
				if factory {
					if m.Status != domain.TaskMappingStatusInProgress {
						t.Fatal("handoff completed mapping before merge")
					}
					o.processTask(context.Background(), task, repo, "dependabot")
					synctest.Wait()
					if merges.Load() != 0 {
						t.Fatal("released verify mapping merged")
					}
					o.HandleSessionCompleted(context.Background(), *m.SessionID, domain.TaskMappingStatusCompleted)
					synctest.Wait()
				}
				if m.Status != domain.TaskMappingStatusCompleted || archives.Load() != 1 {
					t.Fatalf("status=%v archives=%d", m.Status, archives.Load())
				}
				o.HandleSessionCompleted(context.Background(), *m.SessionID, domain.TaskMappingStatusCompleted)
				synctest.Wait()
				if archives.Load() != 1 {
					t.Fatal("duplicate archive")
				}
			})
		})
	}
}
func TestGreenRepairLeaseActive(t *testing.T) {
	for _, factory := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			o, m, task, repo, merges := handoffFixture(t)
			repo.verifyOwnsMerge = factory
			o.SetRepairActivity(handoffLease(true))
			o.processTask(context.Background(), task, repo, "dependabot")
			synctest.Wait()
			if merges.Load() != 0 || m.Status != domain.TaskMappingStatusInProgress || o.activeMapping[repo.id] != m.ID || !o.active[repo.id] {
				t.Fatal("active repair was released or merged")
			}
		})
	}
}
func TestVerifyRepoMappings_Sessions_AC126(t *testing.T) {
	for _, status := range []domain.TaskMappingStatus{domain.TaskMappingStatusPending, domain.TaskMappingStatusInProgress, domain.TaskMappingStatusSkipped, domain.TaskMappingStatusCompleted, domain.TaskMappingStatusFailed, domain.TaskMappingStatusOrphaned} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				o, m, task, repo, merges := handoffFixture(t)
				repo.verifyOwnsMerge = true
				m.Status = status
				m.SessionID = nil
				before := m.UpdatedAt
				o.processTask(context.Background(), task, repo, "dependabot")
				synctest.Wait()
				if merges.Load() != 0 || m.Status != status || !m.UpdatedAt.Equal(before) {
					t.Fatal("verify mapping changed")
				}
			})
		})
	}
	synctest.Test(t, func(t *testing.T) {
		o, _, task, repo, merges := handoffFixture(t)
		repo.verifyOwnsMerge = true
		o.taskMappings.(*mockTaskMappingStore).mappings = map[string]*domain.TaskMapping{}
		o.processTask(context.Background(), task, repo, "dependabot")
		o.routeTask(context.Background(), task, repo, "dependabot")
		synctest.Wait()
		if merges.Load() != 0 || len(o.taskMappings.(*mockTaskMappingStore).mappings) != 0 {
			t.Fatal("verify auto-merge created a mapping")
		}
	})
}
func TestReleasedRepairQueueGuard(t *testing.T) {
	o, m, _, repo, _ := handoffFixture(t)
	o.activeMapping[repo.id] = "mapping-B"
	o.HandleSessionCompleted(context.Background(), *m.SessionID, domain.TaskMappingStatusCompleted)
	if !o.active[repo.id] || o.activeMapping[repo.id] != "mapping-B" {
		t.Fatal("completion released another mapping's slot")
	}
	o.processTask(context.Background(), &pb.TaskItem{ExternalId: "other:43", Action: pb.TaskAction_TASK_ACTION_CREATE_SESSION}, repo, "other")
	if len(o.queues[repo.id]) != 1 {
		t.Fatal("new repair started concurrently")
	}
}
func TestReleasedRepairMergeFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, m, task, repo, _ := handoffFixture(t)
		delete(o.activeMapping, repo.id)
		o.active[repo.id] = false
		o.provider = &mockProvider{mergeFn: func(context.Context, string, int) error { return errors.New("network unavailable") }}
		o.processTask(context.Background(), task, repo, "dependabot")
		synctest.Wait()
		if m.Status != domain.TaskMappingStatusFailed {
			t.Fatal("merge failure was not recorded")
		}
		o.processTask(context.Background(), task, repo, "dependabot")
		synctest.Wait()
		if o.taskMappings.(*mockTaskMappingStore).mappings[m.ExternalID] != m {
			t.Fatal("cooldown mapping replaced")
		}
	})
}
func TestDependabotSetupScript(t *testing.T) {
	for _, factory := range []bool{false, true} {
		for _, rebase := range []bool{false, true} {
			o, m, task, repo, _ := handoffFixture(t)
			repo.verifyOwnsMerge = factory
			task.Action = pb.TaskAction_TASK_ACTION_CREATE_SESSION
			if rebase {
				task = rebaseRepairTask(task, repo, errors.New("rebase refused"))
			}
			called := false
			o.sessionCreator = &mockSessionCreatorOrch{createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				called = true
				if opts.SkipSetupScript == factory {
					t.Errorf("factory=%v rebase=%v skip=%v", factory, rebase, opts.SkipSetupScript)
				}
				return &models.Session{ID: "new"}, nil
			}}
			o.handleCreateSession(context.Background(), task, repo, m, false)
			if !called {
				t.Fatal("red PR did not start repair")
			}
		}
	}
}

func TestVerifyRepoFlip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, m, task, _, merges := handoffFixture(t)
		checker := &handoffFactory{enabled: true}
		o.SetFactoryRepoChecker(checker)
		o.repos = &mockRepoStore{repos: []*models.Repo{{ID: "r", CanAutoMergeDependabot: true}}}
		src := &mockTaskSource{name: "dependabot", pollFn: func(context.Context, string) ([]*pb.TaskItem, error) { return []*pb.TaskItem{task}, nil }}
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{src}}
		o.poll(context.Background())
		synctest.Wait()
		if merges.Load() != 0 || o.active["r"] || m.Status != domain.TaskMappingStatusInProgress {
			t.Fatal("verify poll did not hand off")
		}
		checker.enabled = false
		o.poll(context.Background())
		synctest.Wait()
		if merges.Load() != 1 || m.Status != domain.TaskMappingStatusCompleted {
			t.Fatal("disabled verify job did not restore direct merge on next poll")
		}
	})
}

type verifyCronStore struct{}

func (verifyCronStore) ListByRepo(context.Context, string) ([]*domain.CronJob, error) {
	return []*domain.CronJob{{IsEnabled: true, Prompt: "/boss-verify"}}, nil
}

// A verify cron alone must not stop Dependabot auto-merge: verify takes a bot PR only when the
// repo's admission policy admits bots, so without it the daemon stays the merge owner.
func TestVerifyRepoMergeOwnerFollowsBotAdmission(t *testing.T) {
	for _, tt := range []struct {
		name, config string
		wantMerges   int32
	}{
		{"no bot admission", "", 1},
		{"bots admitted", `{"verifyDefaults":{"admission":{"bot":{"admit":true,"merge":"auto"}}}}`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.config != "" {
				if err := os.WriteFile(filepath.Join(dir, ".boss-skills.json"), []byte(tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			synctest.Test(t, func(t *testing.T) {
				o, _, task, _, merges := handoffFixture(t)
				o.SetFactoryRepoChecker(verifyjob.New(verifyCronStore{}))
				o.repos = &mockRepoStore{repos: []*models.Repo{{ID: "r", LocalPath: dir, CanAutoMergeDependabot: true}}}
				src := &mockTaskSource{name: "dependabot", pollFn: func(context.Context, string) ([]*pb.TaskItem, error) { return []*pb.TaskItem{task}, nil }}
				o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{src}}
				o.poll(context.Background())
				synctest.Wait()
				if got := merges.Load(); got != tt.wantMerges {
					t.Fatalf("daemon merges = %d, want %d", got, tt.wantMerges)
				}
			})
		})
	}
}

type dependabotBusyGate struct{ busy bool }

func (g *dependabotBusyGate) DependabotRepairInFlight(context.Context, string) (string, bool) {
	return "other-session", g.busy
}

func TestDependabotStartDeferredAndRetried_Sessions_AC248003(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(fmt.Sprint(direct), func(t *testing.T) {
			o, m, task, repo, _ := handoffFixture(t)
			task.Action = pb.TaskAction_TASK_ACTION_CREATE_SESSION
			gate := &dependabotBusyGate{busy: true}
			o.SetDependabotRepairGate(gate)
			created := 0
			o.sessionCreator = &mockSessionCreatorOrch{createFn: func(context.Context, CreateSessionOpts) (*models.Session, error) {
				created++
				return &models.Session{ID: "new"}, nil
			}}
			before := m.RetryCount
			if direct {
				o.handleCreateSession(context.Background(), task, repo, m, false)
				if o.active[repo.id] {
					t.Fatal("deferred start retained repo slot")
				}
			} else {
				o.active[repo.id] = false
				delete(o.activeMapping, repo.id)
				m.Status = domain.TaskMappingStatusFailed
				m.UpdatedAt = time.Time{}
				o.processTask(context.Background(), task, repo, "dependabot")
			}
			if created != 0 || m.RetryCount != before {
				t.Fatalf("created=%d retry=%d", created, m.RetryCount)
			}
			if direct && m.Status == domain.TaskMappingStatusFailed {
				t.Fatal("deferral failed mapping")
			}
			gate.busy = false
			o.processTask(context.Background(), task, repo, "dependabot")
			if created != 1 {
				t.Fatalf("retry created=%d", created)
			}
		})
	}
}

func TestDeferredRepairPreservesRetryLimit(t *testing.T) {
	ctx := context.Background()
	o, m, task, repo, _ := handoffFixture(t)
	task.Action = pb.TaskAction_TASK_ACTION_CREATE_SESSION
	m.Status = domain.TaskMappingStatusFailed
	m.UpdatedAt = time.Time{}
	m.RetryCount = MaxReRepairAttempts - 1
	store := o.taskMappings.(*mockTaskMappingStore)
	store.updateFn = func(_ context.Context, id string, p db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
		current := store.mappings[m.ExternalID]
		if current == nil || current.ID != id {
			return nil, errors.New("mapping not found")
		}
		if p.Status != nil {
			current.Status = *p.Status
		}
		if p.RetryCount != nil {
			current.RetryCount = *p.RetryCount
		}
		return current, nil
	}
	created := 0
	o.sessionCreator = &mockSessionCreatorOrch{createFn: func(context.Context, CreateSessionOpts) (*models.Session, error) {
		created++
		return nil, errors.New("repair failed")
	}}
	gate := &dependabotBusyGate{}
	o.SetDependabotRepairGate(gate)

	// The terminal retry queues behind another task before contention appears.
	o.processTask(ctx, task, repo, "dependabot")
	if m.RetryCount != MaxReRepairAttempts || len(o.queues[repo.id]) != 1 {
		t.Fatal("terminal retry was not counted and queued")
	}
	gate.busy = true
	o.dequeueNext(ctx, repo.id)
	if created != 0 || m.Status != domain.TaskMappingStatusOrphaned {
		t.Fatal("queued repair was not deferred")
	}
	gate.busy = false
	o.processTask(ctx, task, repo, "dependabot")
	persisted, err := store.GetByExternalID(ctx, m.ExternalID)
	if err != nil || persisted == nil {
		t.Fatalf("read resumed mapping: %v", err)
	}
	if created != 1 || persisted.RetryCount != MaxReRepairAttempts || persisted.Status != domain.TaskMappingStatusFailed {
		t.Fatalf("resumed attempt: created=%d retry=%d status=%v", created, persisted.RetryCount, persisted.Status)
	}
	m.UpdatedAt = time.Time{}
	o.processTask(ctx, task, repo, "dependabot")
	if created != 1 {
		t.Fatal("exhausted repair started another session")
	}
	if got, _ := store.GetByExternalID(ctx, m.ExternalID); got != m {
		t.Fatal("deferral replaced the repair mapping")
	}
}
