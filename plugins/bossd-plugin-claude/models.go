package main

import (
	"context"
	"slices"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// claudeModelAliases are the --model aliases the claude CLI resolves to the
// newest model of each family. The CLI has no command that lists models.
var claudeModelAliases = []struct{ id, name, description string }{
	{"fable", "Fable", "The newest Fable model"},
	{"opus", "Opus", "The newest Opus model"},
	{"sonnet", "Sonnet", "The newest Sonnet model"},
}

// ListModels offers Default, an empty id that passes no per-run --model so the
// run gets the plugin's pinned model or the CLI's own default, followed by the
// family aliases. Every entry defaults to the effort a run without its own
// choice gets.
func (s *Server) ListModels(context.Context, *bossanovav1.ListModelsRequest) (*bossanovav1.ListModelsResponse, error) {
	effort := s.runner.effort
	if !slices.Contains(claudeEfforts, effort) {
		effort = ""
	}
	description := "Claude Code's default model"
	if s.runner.model != "" {
		description = "The " + s.runner.model + " model pinned in the claude plugin settings"
	}
	models := make([]*bossanovav1.AgentModel, 0, 1+len(claudeModelAliases))
	models = append(models, &bossanovav1.AgentModel{
		DisplayName:      "Default",
		Description:      description,
		SupportedEfforts: slices.Clone(claudeEfforts),
		DefaultEffort:    effort,
		IsDefault:        true,
	})
	for _, alias := range claudeModelAliases {
		models = append(models, &bossanovav1.AgentModel{
			Id:               alias.id,
			DisplayName:      alias.name,
			Description:      alias.description,
			SupportedEfforts: slices.Clone(claudeEfforts),
			DefaultEffort:    effort,
		})
	}
	return &bossanovav1.ListModelsResponse{Models: models}, nil
}
