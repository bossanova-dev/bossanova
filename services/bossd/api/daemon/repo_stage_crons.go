package server

import (
	"context"
	"slices"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/projectconfig"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/repotrust"
)

type stageCronHost interface {
	LookPath(name string) (string, bool)
	SkillRoot(agent string) (string, error)
	HomeDir() string
	FileExists(path string) bool
}

func (s *Server) seedStageCrons(ctx context.Context, repo *models.Repo) {
	if s.stageCronHost == nil || s.cronJobs == nil {
		return
	}
	if !repotrust.Allowed(repo) {
		repoID := ""
		if repo != nil {
			repoID = repo.ID
		}
		s.logger.Info().
			Str("repo_id", repoID).
			Msg("skipping default factory stage crons: repository is not trusted")
		return
	}
	repoID := repo.ID

	loaded := s.loadedAgentNames()
	agent := ""
	for _, candidate := range projectconfig.FactoryAgents() {
		if !slices.Contains(loaded, candidate.Name) {
			continue
		}
		if _, ok := s.stageCronHost.LookPath(candidate.CLI); ok {
			agent = candidate.Name
			break
		}
	}
	if agent == "" {
		s.logger.Warn().
			Str("repo_id", repoID).
			Msg("skipping default factory stage crons: no loaded factory agent runner has its CLI on PATH")
		return
	}

	nodePath, _ := s.stageCronHost.LookPath("node")
	root, rootErr := s.stageCronHost.SkillRoot(agent)
	home := s.stageCronHost.HomeDir()

	existing, err := s.cronJobs.ListByRepo(ctx, repoID)
	if err != nil {
		s.logger.Warn().
			Err(err).
			Str("repo_id", repoID).
			Msg("skipping default factory stage crons: list existing cron jobs failed")
		return
	}

	for _, st := range projectconfig.DefaultFactoryStages() {
		if !st.DefaultOn {
			continue
		}
		if slices.ContainsFunc(existing, func(j *domain.CronJob) bool {
			return st.MatchesJob(j.Name, j.Prompt)
		}) {
			continue
		}

		failures := projectconfig.FactoryGatePreflight(st, projectconfig.FactoryGateHost{
			NodePath:     nodePath,
			SkillRoot:    root,
			SkillRootErr: rootErr,
			Home:         home,
			FileExists:   s.stageCronHost.FileExists,
		})
		_, err := s.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
			RepoId:      repoID,
			Name:        st.JobName,
			Prompt:      st.Prompt(agent),
			Schedule:    st.DefaultSchedule,
			IsEnabled:   len(failures) == 0,
			AgentName:   agent,
			GateCommand: st.GateCommand("node", root, home),
			ManagedBy:   projectconfig.DefaultsMarker,
		}))
		if err != nil {
			s.logger.Warn().
				Err(err).
				Str("repo_id", repoID).
				Str("job", st.JobName).
				Str("stage", st.Stage).
				Msg("seed default factory stage cron failed")
			continue
		}
		if len(failures) > 0 {
			s.logger.Warn().
				Str("repo_id", repoID).
				Str("job", st.JobName).
				Str("stage", st.Stage).
				Str("preflight", strings.Join(failures, "; ")).
				Msg("seeded default factory stage cron disabled after preflight failure")
		}
	}
}
