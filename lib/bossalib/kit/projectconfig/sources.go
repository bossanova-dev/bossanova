package projectconfig

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// The read interfaces below are declared here, by their consumer, and shaped
// so the existing daemon and orchestrator clients satisfy them unchanged.
// They never widen the CLI's client interface.

// RepoReader reads the daemon-local rows a project config manages.
type RepoReader interface {
	ListRepos(ctx context.Context) ([]*pb.Repo, error)
	ListCronJobs(ctx context.Context, repoID string) ([]*pb.CronJob, error)
}

// CloudReader reads the organization-scoped rows a project config manages.
type CloudReader interface {
	ListTriggers(ctx context.Context, req *pb.ListTriggersRequest) ([]*pb.Trigger, error)
	ListSessionWebhooks(ctx context.Context, organizationID string) ([]*pb.SessionWebhook, error)
}

// FactoryStageReader recognises factory-stage cron jobs and supplies a
// stage's default schedule. ok is false for an unknown stage.
type FactoryStageReader interface {
	FactoryClassifier
	DefaultSchedule(stage string) (schedule string, ok bool)
}
