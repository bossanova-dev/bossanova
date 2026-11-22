package server

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/api/upstream"
)

// A hosted create reaches the daemon as a stream CreateSessionCommand, not as
// a socket CreateSessionRequest. Run that whole path, from the command to the
// agent start, so dropping model or effort at any hop fails here.
func TestHostedCreateStartsTheAgentWithTheRequestedModelAndEffort(t *testing.T) {
	runner := &setupStreamAgent{}
	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, runner)
	creator := &upstream.SessionCreatorAdapter{Server: h.server, Logger: zerolog.Nop()}
	prNumber, agentName, model, effort := int32(331), "claude", "hosted-model", "xhigh"

	chunks, err := creator.Create(context.Background(), &pb.CreateSessionCommand{
		RepoId:    h.repo.ID,
		Title:     "hosted runtime",
		Plan:      "do work",
		PrNumber:  &prNumber,
		AgentName: agentName,
		Detach:    true,
		Model:     &model,
		Effort:    &effort,
	}, "cmd-hosted-runtime")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for chunk := range chunks {
		if chunk.GetError() != nil {
			t.Fatalf("create failed: %v", chunk.GetError())
		}
	}

	got := runner.startedRuntimes()
	if len(got) != 1 || got[0] != [2]string{model, effort} {
		t.Fatalf("agent starts (model, effort) = %v, want one start on (%s, %s)", got, model, effort)
	}
}
