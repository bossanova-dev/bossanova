package taskorchestrator

import (
	"context"
	"testing"
	"testing/synctest"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/plugin"
)

func lifecycleSource(o *Orchestrator, tasks ...*pb.TaskItem) *mockTaskSource {
	o.repos = &mockRepoStore{repos: []*models.Repo{{ID: "r", CanAutoMergeDependabot: true, MergeStrategy: "rebase"}}}
	src := &mockTaskSource{name: "dependabot", pollFn: func(context.Context, string) ([]*pb.TaskItem, error) { return tasks, nil }}
	o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{src}}
	return src
}

func lifecycleTask(id string) *pb.TaskItem {
	return &pb.TaskItem{ExternalId: "dependabot:org/repo:" + id, Action: pb.TaskAction_TASK_ACTION_AUTO_MERGE}
}

func TestLifecycleDoneWaitsForMergeBookkeeping_Sessions_AC127(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newTestOrchestrator()
		lifecycleSource(o, lifecycleTask("42"))
		entered, release := make(chan struct{}), make(chan struct{})
		completed := false
		o.provider = &mockProvider{mergeFn: func(context.Context, string, int) error { close(entered); <-release; return nil }}
		o.taskMappings.(*mockTaskMappingStore).updateFn = func(_ context.Context, id string, p db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
			if p.Status != nil && *p.Status == domain.TaskMappingStatusCompleted {
				completed = true
			}
			return &domain.TaskMapping{ID: id}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		o.Start(ctx)
		<-entered
		cancel()
		synctest.Wait()
		select {
		case <-o.Done():
			t.Error("Done closed before merge bookkeeping returned")
		default:
		}
		if completed {
			t.Error("merge bookkeeping ran before merge returned")
		}
		close(release)
		<-o.Done()
		synctest.Wait()
		if !completed {
			t.Error("Done closed without recording completed mapping")
		}
	})
}

func TestLifecycleCancellationReleasesMerge_Sessions_AC127(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newTestOrchestrator()
		lifecycleSource(o, lifecycleTask("42"))
		entered := make(chan struct{})
		o.provider = &mockProvider{mergeFn: func(ctx context.Context, _ string, _ int) error { close(entered); <-ctx.Done(); return ctx.Err() }}
		ctx, cancel := context.WithCancel(context.Background())
		o.Start(ctx)
		<-entered
		cancel()
		<-o.Done()
	})
}

func TestLifecycleCancellationReleasesSemaphoreWaiter_Sessions_AC127(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newTestOrchestrator()
		lifecycleSource(o, lifecycleTask("42"), lifecycleTask("43"))
		entered, release := make(chan struct{}), make(chan struct{})
		calls := 0
		o.provider = &mockProvider{mergeFn: func(ctx context.Context, _ string, _ int) error {
			calls++
			if calls == 1 {
				close(entered)
			}
			<-ctx.Done()
			<-release
			return ctx.Err()
		}}
		ctx, cancel := context.WithCancel(context.Background())
		o.Start(ctx)
		<-entered
		synctest.Wait()
		cancel()
		// Keep the semaphore occupied until the other worker has observed cancellation.
		synctest.Wait()
		close(release)
		<-o.Done()
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("MergePR calls = %d, want 1", calls)
		}
	})
}

func TestLifecycleDoneWaitsForGreenRepairBookkeeping_Sessions_AC127(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, _, task, _, _ := handoffFixture(t)
		src := lifecycleSource(o, task)
		completed := false
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{&updatingMockTaskSource{mockTaskSource: *src, updateFn: func(_ context.Context, _ string, status pb.TaskItemStatus, _ string) error {
			if status == pb.TaskItemStatus_TASK_ITEM_STATUS_COMPLETED {
				completed = true
			}
			return nil
		}}}}
		o.SetRepairActivity(handoffLease(false))
		entered, release := make(chan struct{}), make(chan struct{})
		o.provider = &mockProvider{mergeFn: func(context.Context, string, int) error { close(entered); <-release; return nil }}
		ctx, cancel := context.WithCancel(context.Background())
		o.Start(ctx)
		<-entered
		cancel()
		synctest.Wait()
		select {
		case <-o.Done():
			t.Error("Done closed before green-repair completion bookkeeping returned")
		default:
		}
		if completed {
			t.Error("plugin completion ran before merge returned")
		}
		close(release)
		<-o.Done()
		synctest.Wait()
		if !completed {
			t.Error("Done closed without notifying plugin of completion")
		}
	})
}
