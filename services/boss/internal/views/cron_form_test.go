package views

import (
	"context"
	"testing"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestCronFormBuildForm_DefaultsToDaemonAgent verifies that a fresh create
// form does not synthesize an explicit agent. Leaving the value blank lets the
// daemon resolve its configured default on submit.
func TestCronFormBuildForm_DefaultsToDaemonAgent(t *testing.T) {
	m := CronFormModel{
		ctx:    context.Background(),
		agents: []client.AgentInfo{{Name: "codex"}, {Name: "claude"}},
	}

	m.buildForm()

	if got, want := m.fd.agentName, ""; got != want {
		t.Fatalf("default agentName = %q, want %q", got, want)
	}
}

func TestCronFormHandleSubmit_CreateIncludesSelectedAgentName(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		fd: &cronFormData{
			name:      "Agent job",
			repoID:    "repo-1",
			prompt:    "Run with selected agent.",
			schedule:  "@daily",
			enabled:   true,
			agentName: "opencode",
		},
	}

	_, cmd := m.handleSubmit()
	if cmd == nil {
		t.Fatal("handleSubmit command = nil, want CreateCronJob command")
	}
	_ = cmd()

	if c.createdCronReq == nil {
		t.Fatal("CreateCronJob was not called")
	}
	if got, want := c.createdCronReq.AgentName, "opencode"; got != want {
		t.Fatalf("CreateCronJob.AgentName = %q, want %q", got, want)
	}
}

func TestCronFormHandleSubmit_CreateKeepsDaemonDefaultAgentBlank(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		fd: &cronFormData{
			name:     "Default agent job",
			repoID:   "repo-1",
			prompt:   "Run with daemon default.",
			schedule: "@daily",
			enabled:  true,
		},
	}

	_, cmd := m.handleSubmit()
	if cmd == nil {
		t.Fatal("handleSubmit command = nil, want CreateCronJob command")
	}
	_ = cmd()

	if c.createdCronReq == nil {
		t.Fatal("CreateCronJob was not called")
	}
	if got, want := c.createdCronReq.AgentName, ""; got != want {
		t.Fatalf("CreateCronJob.AgentName = %q, want blank daemon default", got)
	}
}

func TestCronFormHandleSubmit_UpdateIncludesChangedAgentName(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		job: &pb.CronJob{
			Id:        "cron-1",
			Name:      "Agent job",
			RepoId:    "repo-1",
			Prompt:    "Run with selected agent.",
			Schedule:  "@daily",
			Enabled:   true,
			AgentName: "claude",
		},
		fd: &cronFormData{
			name:      "Agent job",
			repoID:    "repo-1",
			prompt:    "Run with selected agent.",
			schedule:  "@daily",
			enabled:   true,
			agentName: "opencode",
		},
	}

	_, cmd := m.handleSubmit()
	if cmd == nil {
		t.Fatal("handleSubmit command = nil, want UpdateCronJob command")
	}
	_ = cmd()

	if c.updatedCronReq == nil {
		t.Fatal("UpdateCronJob was not called")
	}
	if c.updatedCronReq.AgentName == nil {
		t.Fatal("UpdateCronJob.AgentName = nil, want changed agent")
	}
	if got, want := *c.updatedCronReq.AgentName, "opencode"; got != want {
		t.Fatalf("UpdateCronJob.AgentName = %q, want %q", got, want)
	}
}
