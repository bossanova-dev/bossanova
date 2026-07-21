package server

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestValidateModel_AcceptsArbitrary(t *testing.T) {
	s := &Server{}
	for _, m := range []string{"", "sonnet", "claude-opus-4-8", "gpt-5-codex", "some-future-model-2027"} {
		if err := s.validateModel(m); err != nil {
			t.Errorf("validateModel(%q) = %v, want nil", m, err)
		}
	}
	if err := s.validateModel(strings.Repeat("x", 201)); err == nil {
		t.Error("validateModel(201 chars) = nil, want error")
	}
}

// TestCronJobAPIsRoundTripModel asserts a model set on create survives Get/List
// and that Update can change it (including clearing it back to "" = default).
func TestCronJobAPIsRoundTripModel(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)

	created, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:    repoID,
		Name:      "Daily",
		Prompt:    "Run daily checks",
		Schedule:  "@daily",
		Model:     "sonnet",
		IsEnabled: false,
	}))
	if err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	jobID := created.Msg.CronJob.Id
	if created.Msg.CronJob.Model != "sonnet" {
		t.Fatalf("create model = %q, want sonnet", created.Msg.CronJob.Model)
	}

	got, err := srv.GetCronJob(ctx, connect.NewRequest(&pb.GetCronJobRequest{Id: jobID}))
	if err != nil {
		t.Fatalf("GetCronJob: %v", err)
	}
	if got.Msg.CronJob.Model != "sonnet" {
		t.Fatalf("get model = %q, want sonnet", got.Msg.CronJob.Model)
	}

	newModel := "opus"
	updated, err := srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:    jobID,
		Model: &newModel,
	}))
	if err != nil {
		t.Fatalf("UpdateCronJob: %v", err)
	}
	if updated.Msg.CronJob.Model != "opus" {
		t.Fatalf("update model = %q, want opus", updated.Msg.CronJob.Model)
	}

	empty := ""
	cleared, err := srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:    jobID,
		Model: &empty,
	}))
	if err != nil {
		t.Fatalf("UpdateCronJob clear: %v", err)
	}
	if cleared.Msg.CronJob.Model != "" {
		t.Fatalf("cleared model = %q, want empty", cleared.Msg.CronJob.Model)
	}
}

// TestCreateCronJob_AcceptsBogusModel proves there is no allowlist: an arbitrary
// model string is accepted by the server (only the agent CLI rejects it at run).
func TestCreateCronJob_AcceptsBogusModel(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)

	created, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:    repoID,
		Name:      "Bogus",
		Prompt:    "x",
		Schedule:  "@daily",
		Model:     "totally-not-a-real-model-9000",
		IsEnabled: false,
	}))
	if err != nil {
		t.Fatalf("CreateCronJob with bogus model should succeed, got: %v", err)
	}
	if created.Msg.CronJob.Model != "totally-not-a-real-model-9000" {
		t.Fatalf("model = %q, want the bogus value preserved", created.Msg.CronJob.Model)
	}
}
