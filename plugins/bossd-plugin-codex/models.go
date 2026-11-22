package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const codexModelListTimeout = 20 * time.Second

// appServerModel is one `model/list` entry. Codex reports each supported
// effort as an object carrying its own description.
type appServerModel struct {
	ID                        string `json:"id"`
	DisplayName               string `json:"displayName"`
	Description               string `json:"description"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
	DefaultReasoningEffort string `json:"defaultReasoningEffort"`
	IsDefault              bool   `json:"isDefault"`
	Hidden                 bool   `json:"hidden"`
}

type appServerModelPage struct {
	Data       []appServerModel `json:"data"`
	NextCursor string           `json:"nextCursor"`
}

type modelCatalog interface {
	Models(context.Context) ([]appServerModel, error)
}

// codexAppServerModelCatalog reads the live model list from `codex app-server`.
type codexAppServerModelCatalog struct {
	binary         string
	loginShell     string
	commandFactory func(context.Context, string, ...string) *exec.Cmd
}

func (c codexAppServerModelCatalog) Models(ctx context.Context) ([]appServerModel, error) {
	ctx, cancel := context.WithTimeout(ctx, codexModelListTimeout)
	defer cancel()
	conn, err := openAppServer(ctx, appServerLaunch{binary: c.binary, loginShell: c.loginShell, commandFactory: c.commandFactory})
	if err != nil {
		return nil, err
	}
	defer conn.close()
	var models []appServerModel
	seenCursors := map[string]struct{}{}
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := conn.call("model/list", params)
		if err != nil {
			return nil, fmt.Errorf("list codex models: %w", err)
		}
		var page appServerModelPage
		if err := json.Unmarshal(result, &page); err != nil {
			return nil, fmt.Errorf("decode codex models: %w", err)
		}
		models = append(models, page.Data...)
		cursor = page.NextCursor
		if cursor == "" {
			return models, nil
		}
		if _, duplicate := seenCursors[cursor]; duplicate {
			return nil, fmt.Errorf("codex model list repeated cursor %q", cursor)
		}
		seenCursors[cursor] = struct{}{}
	}
}

// ListModels reports codex's own model list, hidden models omitted. A model or
// effort pinned in the plugin settings is what a run without its own choice
// uses, so it takes the default flag from codex's.
func (s *Server) ListModels(ctx context.Context, _ *bossanovav1.ListModelsRequest) (*bossanovav1.ListModelsResponse, error) {
	models, err := s.modelCatalog.Models(ctx)
	if err != nil {
		return nil, err
	}
	return &bossanovav1.ListModelsResponse{Models: agentModels(models, s.runner.model, s.runner.effort)}, nil
}

func agentModels(models []appServerModel, pinnedModel, pinnedEffort string) []*bossanovav1.AgentModel {
	out := make([]*bossanovav1.AgentModel, 0, len(models))
	for _, m := range models {
		if m.Hidden {
			continue
		}
		efforts := make([]string, 0, len(m.SupportedReasoningEfforts))
		defaultEffort := m.DefaultReasoningEffort
		for _, e := range m.SupportedReasoningEfforts {
			efforts = append(efforts, e.ReasoningEffort)
			if e.ReasoningEffort == pinnedEffort {
				defaultEffort = pinnedEffort
			}
		}
		isDefault := m.IsDefault
		if pinnedModel != "" {
			isDefault = m.ID == pinnedModel
		}
		out = append(out, &bossanovav1.AgentModel{
			Id:               m.ID,
			DisplayName:      m.DisplayName,
			Description:      m.Description,
			SupportedEfforts: efforts,
			DefaultEffort:    defaultEffort,
			IsDefault:        isDefault,
		})
	}
	return out
}
