package taskorchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

func TestRouteTask_CreateSession(t *testing.T) {
	var capturedOpts CreateSessionOpts

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "sess-new"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*domain.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:repo:55",
		Title:          "Bump react",
		Plan:           "Fix failing tests",
		BaseBranch:     "develop",
		ExistingBranch: "dependabot/npm/react-18.3.0",
		Action:         bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
	}, repoInfo{id: "r1", originURL: "https://github.com/org/repo"}, "dependabot")

	if capturedOpts.RepoID != "r1" {
		t.Errorf("expected repo r1, got %q", capturedOpts.RepoID)
	}
	if capturedOpts.Title != "Bump react" {
		t.Errorf("expected title 'Bump react', got %q", capturedOpts.Title)
	}
	if !capturedOpts.Detach {
		t.Error("repair plan must run unattended")
	}
	if capturedOpts.Plan != "Fix failing tests" {
		t.Errorf("expected plan, got %q", capturedOpts.Plan)
	}
	if capturedOpts.BaseBranch != "develop" {
		t.Errorf("expected base branch 'develop', got %q", capturedOpts.BaseBranch)
	}
	if capturedOpts.HeadBranch != "dependabot/npm/react-18.3.0" {
		t.Errorf("expected head branch, got %q", capturedOpts.HeadBranch)
	}
}

func TestRouteTask_AutoMerge_RebaseFailureCreatesRepairSession(t *testing.T) {
	var capturedOpts CreateSessionOpts
	var updatedStatus domain.TaskMappingStatus

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				return errors.New("merge PR: gh pr merge 426 --repo recurser/bossanova --rebase --delete-branch: exit status 1: GraphQL: This branch can't be rebased (mergePullRequest)")
			},
		}
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "repair-session"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*domain.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*domain.TaskMapping, error) {
				if params.Status != nil {
					updatedStatus = *params.Status
				}
				return &domain.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:git@github.com:recurser/bossanova.git:426",
		Title:          "chore(web)(deps-dev): bump @types/react",
		Plan:           "Original dependabot plan.",
		Action:         bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		ExistingBranch: "dependabot/npm_and_yarn/services/web/types/react-19.2.15",
		Labels:         []string{"dependabot"},
	}, repoInfo{
		id:            "r1",
		originURL:     "git@github.com:recurser/bossanova.git",
		baseBranch:    "main",
		mergeStrategy: "rebase",
	}, "dependabot")

	if capturedOpts.HeadBranch != "dependabot/npm_and_yarn/services/web/types/react-19.2.15" {
		t.Fatalf("HeadBranch = %q, want existing Dependabot branch", capturedOpts.HeadBranch)
	}
	if !strings.Contains(capturedOpts.Plan, "rebase") {
		t.Fatalf("repair plan should explicitly mention rebase, got %q", capturedOpts.Plan)
	}
	if !capturedOpts.Detach {
		t.Error("rebase repair plan must run unattended")
	}
	if !strings.Contains(capturedOpts.Plan, "gh pr merge") {
		t.Fatalf("repair plan should include failed merge command context, got %q", capturedOpts.Plan)
	}
	if updatedStatus != domain.TaskMappingStatusInProgress {
		t.Fatalf("status = %d, want InProgress", updatedStatus)
	}
}

func TestRouteTask_CreateSession_WhitespacePlanStaysIdle(t *testing.T) {
	var capturedOpts CreateSessionOpts

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "sess-new"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*domain.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "task:1",
		Title:      "Fix bug",
		Plan:       " \n\t",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
		// BaseBranch intentionally empty
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if capturedOpts.Detach {
		t.Error("whitespace plan must stay idle")
	}
	if capturedOpts.BaseBranch != "main" {
		t.Errorf("expected default base branch 'main', got %q", capturedOpts.BaseBranch)
	}
}
