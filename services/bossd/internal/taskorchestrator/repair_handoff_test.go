package taskorchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/plugin"
)

type handoffLease bool

func (l handoffLease) Active(string) bool { return bool(l) }

type handoffFactory struct{ enabled bool }

func (c *handoffFactory) IsFactoryRepo(context.Context, string) bool { return c.enabled }

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
				repo.isFactory = factory
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
			repo.isFactory = factory
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
				repo.isFactory = true
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
		repo.isFactory = true
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
			repo.isFactory = factory
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
