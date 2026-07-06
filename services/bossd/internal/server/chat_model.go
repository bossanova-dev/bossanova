package server

import "github.com/recurser/bossalib/models"

// modelForChatAgent decides which agent model id to forward to a chat's
// runner. sess.Model is the model chosen for the session's original agent
// (models.Session.Model). A chat may override its agent per-chat
// (RecordChatRequest.agent_name), so a Codex chat can live inside a Claude
// session; forwarding the session's Claude model id (e.g. "opus") to Codex
// makes Codex reject it as an unknown model (BOS-255).
//
// Rule: forward sess.Model only when the chat runs the SAME agent as the
// session; otherwise return "" so the target plugin resolves its own default
// (its BOSS_PLUGIN_model env, else the agent CLI's own default). This keys off
// agent identity, never off parsing model strings — the daemon deliberately
// does not enumerate model names (see model_resolution.go).
//
// Both agent names are normalized through defaultLegacyAgent ("" → "claude")
// before comparison, exactly as the argv builder and the sibling dispatchers
// (liveArgvBuilder / liveTranscriptOracle / liveInteractiveSessionResolver in
// spawn_chat_tmux.go) resolve an empty agent name. Keeping this identical to
// dispatch is what makes the gate provably correct: an empty chat agent
// resolves to "claude" (the agent the spawn will actually launch), never to
// the session's agent — so a codex session model is never forwarded to a
// claude-defaulted chat spawn.
func modelForChatAgent(sess *models.Session, chatAgentName string) string {
	if sess == nil {
		return ""
	}
	sessionAgent := sess.AgentName
	if sessionAgent == "" {
		sessionAgent = defaultLegacyAgent
	}
	chatAgent := chatAgentName
	if chatAgent == "" {
		chatAgent = defaultLegacyAgent
	}
	if chatAgent != sessionAgent {
		return ""
	}
	return sess.Model
}
