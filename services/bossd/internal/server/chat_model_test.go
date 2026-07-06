package server

import (
	"testing"

	"github.com/recurser/bossalib/models"
)

func TestModelForChatAgent(t *testing.T) {
	tests := []struct {
		name      string
		sessAgent string
		sessModel string
		chatAgent string
		wantModel string
	}{
		{
			name:      "same agent forwards the session model",
			sessAgent: "claude", sessModel: "opus", chatAgent: "claude", wantModel: "opus",
		},
		{
			name:      "empty chat agent normalizes to claude, claude session forwards model",
			sessAgent: "claude", sessModel: "opus", chatAgent: "", wantModel: "opus",
		},
		{
			// An empty chat agent normalizes to "claude" (the agent the spawn
			// path actually launches), NOT to the codex session agent — so the
			// codex model must be dropped, mirroring the cross-agent rule. This
			// pins that the helper's normalization matches the argv builder's.
			name:      "empty chat agent in codex session drops the codex model",
			sessAgent: "codex", sessModel: "gpt-5-codex", chatAgent: "", wantModel: "",
		},
		{
			name:      "cross-agent codex chat in claude session drops the claude model",
			sessAgent: "claude", sessModel: "opus", chatAgent: "codex", wantModel: "",
		},
		{
			name:      "cross-agent claude chat in codex session drops the codex model",
			sessAgent: "codex", sessModel: "gpt-5-codex", chatAgent: "claude", wantModel: "",
		},
		{
			name:      "legacy empty session agent normalizes to claude, codex chat drops model",
			sessAgent: "", sessModel: "opus", chatAgent: "codex", wantModel: "",
		},
		{
			name:      "legacy empty session agent, claude chat forwards model",
			sessAgent: "", sessModel: "opus", chatAgent: "claude", wantModel: "opus",
		},
		{
			name:      "empty session model stays empty for same agent",
			sessAgent: "codex", sessModel: "", chatAgent: "codex", wantModel: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &models.Session{AgentName: tt.sessAgent, Model: tt.sessModel}
			if got := modelForChatAgent(sess, tt.chatAgent); got != tt.wantModel {
				t.Fatalf("modelForChatAgent(agent=%q model=%q, chat=%q) = %q, want %q",
					tt.sessAgent, tt.sessModel, tt.chatAgent, got, tt.wantModel)
			}
		})
	}
}

func TestModelForChatAgentNilSession(t *testing.T) {
	if got := modelForChatAgent(nil, "codex"); got != "" {
		t.Fatalf("modelForChatAgent(nil, codex) = %q, want empty", got)
	}
}
