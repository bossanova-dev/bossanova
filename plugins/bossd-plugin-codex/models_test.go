package main

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// The fixture is codex-cli 0.167.2's `model/list` reply captured with
// includeHidden, so it carries the hidden entries the plugin must drop.
func fakeModelListAppServer(t *testing.T) codexAppServerModelCatalog {
	t.Helper()
	fixture, err := filepath.Abs("testdata/app_server_model_list.json")
	if err != nil {
		t.Fatal(err)
	}
	return codexAppServerModelCatalog{binary: "codex", commandFactory: fakeCodexShell(t, `
		read -r initialize
		printf '%s\n' '{"id":1,"result":{}}'
		read -r initialized
		read -r list
		cat '`+fixture+`'
		printf '\n'
	`)}
}

func modelByID(models []*bossanovav1.AgentModel, id string) *bossanovav1.AgentModel {
	for _, m := range models {
		if m.GetId() == id {
			return m
		}
	}
	return nil
}

func TestListModelsReportsCodexModelList(t *testing.T) {
	srv := newServer(nil, zerolog.Nop(), WithEffort(""))
	srv.modelCatalog = fakeModelListAppServer(t)

	resp, err := srv.ListModels(context.Background(), &bossanovav1.ListModelsRequest{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	models := resp.GetModels()
	var ids, defaults []string
	for _, m := range models {
		ids = append(ids, m.GetId())
		if m.GetIsDefault() {
			defaults = append(defaults, m.GetId())
		}
	}
	wantIDs := []string{"gpt-6.2-sol", "gpt-6.2-astra", "gpt-6.1-terra", "gpt-6.1-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}
	if !slices.Equal(ids, wantIDs) {
		t.Fatalf("ids = %v, want %v (hidden entries dropped, codex order kept)", ids, wantIDs)
	}
	if !slices.Equal(defaults, []string{"gpt-6.2-sol"}) {
		t.Fatalf("defaults = %v, want only codex's default", defaults)
	}
	sol := modelByID(models, "gpt-6.2-sol")
	if sol.GetDisplayName() != "GPT-6.2-Sol" || sol.GetDescription() == "" {
		t.Fatalf("gpt-6.2-sol display = %q description = %q", sol.GetDisplayName(), sol.GetDescription())
	}
	if want := []string{"low", "medium", "high", "xhigh", "max", "ultra"}; !slices.Equal(sol.GetSupportedEfforts(), want) {
		t.Fatalf("gpt-6.2-sol efforts = %v, want %v", sol.GetSupportedEfforts(), want)
	}
	if want := []string{"low", "medium", "high", "xhigh", "max"}; !slices.Equal(modelByID(models, "gpt-6-luna").GetSupportedEfforts(), want) {
		t.Fatalf("gpt-6-luna efforts = %v, want %v", modelByID(models, "gpt-6-luna").GetSupportedEfforts(), want)
	}
	if sol.GetDefaultEffort() != "low" || modelByID(models, "gpt-6-astra").GetDefaultEffort() != "medium" {
		t.Fatalf("default efforts = %q, %q; want codex's own low, medium", sol.GetDefaultEffort(), modelByID(models, "gpt-6-astra").GetDefaultEffort())
	}
}

func TestListModelsPrefersPinnedModelAndEffort(t *testing.T) {
	srv := newServer(nil, zerolog.Nop(), WithModel("gpt-6-luna"), WithEffort("ultra"))
	srv.modelCatalog = fakeModelListAppServer(t)

	resp, err := srv.ListModels(context.Background(), &bossanovav1.ListModelsRequest{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	for _, m := range resp.GetModels() {
		if m.GetIsDefault() != (m.GetId() == "gpt-6-luna") {
			t.Fatalf("%s is_default = %t; only the pinned model is the default", m.GetId(), m.GetIsDefault())
		}
	}
	if got := modelByID(resp.GetModels(), "gpt-6.2-sol").GetDefaultEffort(); got != "ultra" {
		t.Fatalf("gpt-6.2-sol default effort = %q, want pinned ultra", got)
	}
	if got := modelByID(resp.GetModels(), "gpt-6-luna").GetDefaultEffort(); got != "medium" {
		t.Fatalf("gpt-6-luna default effort = %q, want its own medium because it lacks ultra", got)
	}
}

func TestCodexModelCatalogFollowsCursor(t *testing.T) {
	catalog := codexAppServerModelCatalog{binary: "codex", commandFactory: fakeCodexShell(t, `
		read -r initialize
		printf '%s\n' '{"id":1,"result":{}}'
		read -r initialized
		read -r first
		printf '%s\n' '{"id":2,"result":{"data":[{"id":"a"}],"nextCursor":"page-2"}}'
		read -r second
		case "$second" in *'"cursor":"page-2"'*) ;; *) exit 1 ;; esac
		printf '%s\n' '{"id":3,"result":{"data":[{"id":"b"}],"nextCursor":null}}'
	`)}
	models, err := catalog.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "a" || models[1].ID != "b" {
		t.Fatalf("models = %+v, want a then b", models)
	}
}
