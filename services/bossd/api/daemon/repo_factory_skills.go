package server

import "context"

// factorySkillsInstaller installs embedded boss skills on operator consent
// (repo registration or cron creation). Optional; nil skips installation.
type factorySkillsInstaller interface {
	InstallOnConsent(ctx context.Context, reason string)
}

func (s *Server) installFactorySkills(ctx context.Context, reason string) {
	if s.factorySkills != nil {
		s.factorySkills.InstallOnConsent(ctx, reason)
	}
}
