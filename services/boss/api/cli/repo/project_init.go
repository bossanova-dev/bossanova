package repo

import (
	"context"
	"errors"

	"github.com/recurser/boss/api/cli/skills"
	"github.com/recurser/bossalib/projectconfig"
	"github.com/spf13/cobra"
)

// LoadInitProject connects init to the project config application engine.
func (g *Group) LoadInitProject(ctx context.Context, dir string) (*skills.Project, error) {
	proj, found, err := loadProjectFile(ctx, dir)
	if errors.Is(err, errNotGitCheckout) || !found {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &skills.Project{
		Path: proj.path,
		SelectsCrons: func() bool {
			env := g.newProjectApplyEnv()
			identity, _ := env.identity(env.export)
			return len(projectconfig.CronsForDaemon(proj.cfg, identity)) > 0
		},
		CheckBranch: func(ctx context.Context, cmd *cobra.Command) (string, bool) {
			env := g.newProjectApplyEnv()
			state := observeProjectBranchState(ctx, cmd, env, proj)
			_, want, ok := projectBranchCheck(ctx, env, proj, state)
			return want, ok
		},
		Plan: func(ctx context.Context, cmd *cobra.Command) (skills.ProjectRun, error) {
			env := g.newProjectApplyEnv()
			run, err := planLocalProject(ctx, cmd, env, proj, false)
			if err != nil {
				return skills.ProjectRun{}, err
			}
			return skills.ProjectRun{
				Plan:      run.plan,
				WritePlan: writeProjectPlan,
				CheckTrust: func(plan projectconfig.LocalPlan, trust skills.ProjectTrust) error {
					run.plan = plan
					return checkProjectTrust(ctx, cmd, env, run, projectTrust{yes: trust.Yes, allowBranch: trust.AllowBranch, confirm: trust.Confirm})
				},
				Apply: func(plan projectconfig.LocalPlan) (projectconfig.LocalResult, error) {
					return projectconfig.ApplyLocal(ctx, run.c, plan)
				},
			}, nil
		},
	}, nil
}
