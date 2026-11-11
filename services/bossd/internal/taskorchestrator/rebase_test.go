package taskorchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

type rebaseCommentProvider struct {
	mockProvider
	comments []string
	postErr  error
}

func (p *rebaseCommentProvider) PostPRComment(_ context.Context, _ string, _ int, body string) error {
	p.comments = append(p.comments, body)
	return p.postErr
}

func TestRequestRebaseOncePerHeadAndRestart_Sessions_AC248202(t *testing.T) {
	p := &rebaseCommentProvider{}
	store := &mockTaskMappingStore{mappings: map[string]*domain.TaskMapping{}}
	store.updateFn = func(_ context.Context, id string, params db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
		for _, m := range store.mappings {
			if m.ID == id {
				if params.Status != nil {
					m.Status = *params.Status
				}
				m.UpdatedAt = time.Now()
				return m, nil
			}
		}
		return nil, errors.New("missing mapping")
	}
	repo := repoInfo{id: "r1", originURL: "https://github.com/org/repo"}
	makeOrch := func() *Orchestrator {
		return newTestOrchestrator(func(o *Orchestrator) { o.provider = p; o.taskMappings = store })
	}
	task := &pb.TaskItem{ExternalId: "dependabot:rebase:https://github.com/org/repo:abc:42", HeadSha: "abc", IsBotRebaseRequestable: true, Action: pb.TaskAction_TASK_ACTION_REQUEST_REBASE}
	run := func(o *Orchestrator) { o.processTask(t.Context(), task, repo, "dependabot"); o.workers.Wait() }
	o := makeOrch()
	run(o)
	run(o)
	run(makeOrch())
	if len(p.comments) != 1 || p.comments[0] != "@dependabot rebase" {
		t.Fatalf("comments = %v", p.comments)
	}
	task.HeadSha = "def"
	task.ExternalId = "dependabot:rebase:https://github.com/org/repo:def:42"
	run(o)
	if len(p.comments) != 2 {
		t.Fatalf("new head comments = %v", p.comments)
	}
	p.postErr = errors.New("offline")
	task.HeadSha = "ghi"
	task.ExternalId = "dependabot:rebase:https://github.com/org/repo:ghi:42"
	run(o)
	run(o)
	if len(p.comments) != 3 {
		t.Fatalf("retried before cooldown: %v", p.comments)
	}
	m := store.mappings[task.ExternalId]
	if m.Status != domain.TaskMappingStatusFailed {
		t.Fatalf("status=%v", m.Status)
	}
	m.UpdatedAt = time.Now().Add(-2 * TerminalRetryCooldown)
	p.postErr = nil
	run(o)
	if len(p.comments) != 4 || store.mappings[task.ExternalId].Status != domain.TaskMappingStatusCompleted {
		t.Fatalf("retry failed: %v", p.comments)
	}
}

func TestBotRebaseRefusalUsesClassifiedHead_Sessions_AC248201(t *testing.T) {
	for _, head := range []string{testHeadSHA, "different-head"} {
		t.Run(head, func(t *testing.T) {
			p := &rebaseCommentProvider{mockProvider: mockProvider{mergeFn: func(context.Context, string, int) error { return errors.New("pull request can't be rebased") }}}
			o := newTestOrchestrator(func(o *Orchestrator) { o.provider = p })
			task := &pb.TaskItem{ExternalId: "dependabot:pr:https://github.com/org/repo:42", HeadSha: head, IsBotRebaseRequestable: true, Action: pb.TaskAction_TASK_ACTION_AUTO_MERGE}
			o.routeTask(t.Context(), task, repoInfo{id: "r1", originURL: "https://github.com/org/repo", mergeStrategy: "rebase"}, "dependabot")
			o.workers.Wait()
			if head == testHeadSHA {
				if len(p.comments) != 1 {
					t.Fatalf("comments=%v", p.comments)
				}
				if len(o.activeMapping) != 0 {
					t.Fatal("started a repair")
				}
			} else {
				if len(p.comments) != 0 {
					t.Fatal("requested rebase for an unclassified head")
				}
				if len(o.activeMapping) != 1 {
					t.Fatal("missing existing repair fallback")
				}
			}
		})
	}
}

func TestRebaseMappingFailurePreservesRepairSlot(t *testing.T) {
	o := newTestOrchestrator()
	o.active["r1"] = true
	o.taskMappings = &mockTaskMappingStore{createFn: func(context.Context, db.CreateTaskMappingParams) (*domain.TaskMapping, error) {
		return nil, errors.New("already exists")
	}}
	o.routeTask(t.Context(), &pb.TaskItem{Action: pb.TaskAction_TASK_ACTION_REQUEST_REBASE}, repoInfo{id: "r1"}, "dependabot")
	if !o.active["r1"] {
		t.Fatal("duplicate request released an unrelated repair slot")
	}
}

func TestRequestRebaseInterruptedDelivery_Sessions_AC248202(t *testing.T) {
	for _, failStatus := range []domain.TaskMappingStatus{domain.TaskMappingStatusInProgress, domain.TaskMappingStatusCompleted} {
		t.Run(fmt.Sprint(failStatus), func(t *testing.T) {
			p := &rebaseCommentProvider{}
			store := &mockTaskMappingStore{mappings: map[string]*domain.TaskMapping{}}
			failOnce := true
			store.updateFn = func(_ context.Context, id string, params db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
				if params.Status != nil && *params.Status == failStatus && failOnce {
					failOnce = false
					return nil, errors.New("persistence interrupted")
				}
				for _, m := range store.mappings {
					if m.ID != id {
						continue
					}
					if params.Status != nil {
						m.Status = *params.Status
					}
					if params.LastError != nil {
						m.LastError = *params.LastError
					}
					m.UpdatedAt = time.Now()
					return m, nil
				}
				return nil, errors.New("missing mapping")
			}
			repo := repoInfo{id: "r1", originURL: "https://github.com/org/repo"}
			task := &pb.TaskItem{ExternalId: "dependabot:rebase:https://github.com/org/repo:abc:42", HeadSha: "abc", IsBotRebaseRequestable: true, Action: pb.TaskAction_TASK_ACTION_REQUEST_REBASE}
			run := func() {
				o := newTestOrchestrator(func(o *Orchestrator) { o.provider = p; o.taskMappings = store })
				o.processTask(t.Context(), task, repo, "dependabot")
				o.workers.Wait()
			}
			run()
			mapping := store.mappings[task.ExternalId]
			if failStatus == domain.TaskMappingStatusInProgress {
				if len(p.comments) != 0 || mapping.Status != domain.TaskMappingStatusFailed || mapping.LastError == nil {
					t.Fatalf("intent failure posted=%d status=%v error=%v", len(p.comments), mapping.Status, mapping.LastError)
				}
				return
			}
			if len(p.comments) != 1 || mapping.Status != domain.TaskMappingStatusInProgress || mapping.LastError == nil {
				t.Fatalf("completion failure posted=%d status=%v error=%v", len(p.comments), mapping.Status, mapping.LastError)
			}
			// Boot orphans unfinished work. Recovery must retain the dispatch intent
			// instead of interpreting an unconfirmed response as permission to repost.
			mapping.Status = domain.TaskMappingStatusOrphaned
			run()
			run()
			if len(p.comments) != 1 || mapping.Status != domain.TaskMappingStatusSkipped || mapping.LastError == nil {
				t.Fatalf("recovery posted=%d status=%v error=%v", len(p.comments), mapping.Status, mapping.LastError)
			}
		})
	}
}

func TestRequestRebaseUnconfirmedIntentNeverClaimsDelivery_Sessions_AC248202(t *testing.T) {
	p := &rebaseCommentProvider{}
	externalID := "dependabot:rebase:https://github.com/org/repo:abc:42"
	detail := rebaseDeliveryUnconfirmed
	mapping := &domain.TaskMapping{ID: "m1", ExternalID: externalID, PluginName: "dependabot", Status: domain.TaskMappingStatusOrphaned, LastError: &detail}
	store := &mockTaskMappingStore{mappings: map[string]*domain.TaskMapping{externalID: mapping}}
	store.updateFn = func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
		if params.Status != nil {
			mapping.Status = *params.Status
		}
		if params.LastError != nil {
			mapping.LastError = *params.LastError
		}
		return mapping, nil
	}
	o := newTestOrchestrator(func(o *Orchestrator) { o.provider = p; o.taskMappings = store })
	task := &pb.TaskItem{ExternalId: externalID, HeadSha: "abc", IsBotRebaseRequestable: true, Action: pb.TaskAction_TASK_ACTION_REQUEST_REBASE}
	o.processTask(t.Context(), task, repoInfo{id: "r1", originURL: "https://github.com/org/repo"}, "dependabot")
	o.workers.Wait()
	if len(p.comments) != 0 || mapping.Status != domain.TaskMappingStatusSkipped || mapping.LastError == nil {
		t.Fatalf("unconfirmed intent posted=%d status=%v error=%v", len(p.comments), mapping.Status, mapping.LastError)
	}
}
