package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func listClaudeModels(t *testing.T, srv *Server) []*bossanovav1.AgentModel {
	t.Helper()
	resp, err := srv.ListModels(context.Background(), &bossanovav1.ListModelsRequest{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	return resp.GetModels()
}

func TestListModelsOffersDefaultAndAliases(t *testing.T) {
	srv := newServer(nil, zerolog.Nop())
	models := listClaudeModels(t, srv)

	var ids []string
	for _, m := range models {
		ids = append(ids, m.GetId())
		if !slices.Equal(m.GetSupportedEfforts(), []string{"low", "medium", "high", "xhigh", "max"}) {
			t.Fatalf("%q efforts = %v", m.GetId(), m.GetSupportedEfforts())
		}
		if m.GetDefaultEffort() != defaultClaudeEffort {
			t.Fatalf("%q default effort = %q, want the runner's %q", m.GetId(), m.GetDefaultEffort(), defaultClaudeEffort)
		}
		if m.GetIsDefault() != (m.GetId() == "") {
			t.Fatalf("%q is_default = %t; only Default is the default", m.GetId(), m.GetIsDefault())
		}
	}
	if want := []string{"", "fable", "opus", "sonnet"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %q, want %q", ids, want)
	}
	if models[0].GetDisplayName() != "Default" {
		t.Fatalf("first model = %q, want Default", models[0].GetDisplayName())
	}

	// The ids must mean what the runner does with them.
	defaultArgv := strings.Join(srv.runner.buildArgv(agentruntime.BuildArgvInput{Options: map[string]string{"model": models[0].GetId()}}), "\x00")
	if strings.Contains(defaultArgv, "--model") {
		t.Fatalf("Default argv = %q, want no --model", defaultArgv)
	}
	opusArgv := strings.Join(srv.runner.buildArgv(agentruntime.BuildArgvInput{Options: map[string]string{"model": "opus"}}), "\x00")
	if !strings.Contains(opusArgv, "--model\x00opus") {
		t.Fatalf("opus argv = %q, want --model opus", opusArgv)
	}
}

func TestListModelsReportsPinnedModelAndEffort(t *testing.T) {
	models := listClaudeModels(t, newServer(nil, zerolog.Nop(), WithModel("opus"), WithEffort("max")))
	if !strings.Contains(models[0].GetDescription(), "opus") {
		t.Fatalf("Default description = %q, want it to name the pinned opus", models[0].GetDescription())
	}
	for _, m := range models {
		if m.GetDefaultEffort() != "max" {
			t.Fatalf("%q default effort = %q, want pinned max", m.GetId(), m.GetDefaultEffort())
		}
	}
}
