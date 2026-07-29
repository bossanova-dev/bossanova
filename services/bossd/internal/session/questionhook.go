package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"strconv"
)

// This file holds the whole daemon-side half of the BOS-486 question-signal
// wiring: the registrar seam, the ExtraEnv keys handed to a headless agent, the
// mint/register/release trio for the per-run bearer token, and the policy for
// which agents receive it. It mirrors
// plugins/bossd-plugin-opencode/questionhook.go, the plugin-side half, so the
// feature is greppable as one unit from either module.
//
// Only two things stay outside it: the questionHooks field on Lifecycle, and
// the call sites in StartSession and SignalSessionRunComplete.

// questionHookRegistrar records (and later drops) the per-run bearer token a
// HEADLESS agent run was handed in its environment overlay, so the run's
// loopback POSTs to /hooks/question/{agent_session_id} can authenticate.
// Satisfied by *plugin.HostServiceServer. Declared here — rather than widening
// pollRunCompleter, which the same object also satisfies — so the two concerns
// stay independently fakeable and existing test doubles keep compiling.
type questionHookRegistrar interface {
	RegisterHeadlessRunHookToken(agentSessionID, token string)
	ReleaseHeadlessRunHookToken(agentSessionID string)
}

// questionHookPortEnv / questionHookTokenEnv are the ExtraEnv keys that carry
// the loopback question-signal context to a headless agent process (BOS-486).
//
// They are the agent-facing half of the seam: bossd-plugin-opencode injects its
// event hook only when BOTH are present in the run's ExtraEnv, and the injected
// JS reads them back off process.env at runtime to reach
// POST /hooks/question/{session id} with a Bearer token.
const (
	questionHookPortEnv  = "BOSS_HOOK_PORT"
	questionHookTokenEnv = "BOSS_HOOK_TOKEN"
)

// questionHookOptOutAgents names the agent runners that provably do NOT consume
// the question-hook context, and are therefore never handed it.
//
// BOSS_HOOK_TOKEN is a live bearer credential for a daemon endpoint that writes
// the question store, so it goes only to processes that have a use for it: an
// LLM agent with shell access can read its own environment, and handing it a
// credential it cannot use is gratuitous. This list is also what enforces "a
// headless claude or codex run behaves exactly as it did before BOS-486" —
// today nothing outside bossd-plugin-opencode reads either key, but that is a
// property of the current tree, not something the tree checks.
//
// It matches the STORED session AgentName, not the runner the dispatcher
// eventually resolves. Those differ only for a row with an empty agent_name in
// a multi-runner install, which resolveByName sends to the default agent
// (usually claude) — a legacy shape, since resolveAgentName backfills the name
// at session-create time. Closing that last gap would mean resolving the runner
// here, which the lifecycle has no handle for; the plugin-side gate is the
// backstop either way.
//
// It is deliberately an opt-OUT list rather than an opt-in one. Dispatcher
// resolution treats an empty session AgentName as "the default agent, or the
// sole loaded runner" (agent.Dispatcher.resolveByName), so an opencode-only
// install legitimately starts runs with AgentName == "" — an allow-list keyed on
// the stored name would silently disarm the hook there. Naming the two agents we
// know ignore the keys keeps the hook armed for everything else, where the
// plugin-side gate (installQuestionHook, which no-ops unless both keys are
// present) is the functional backstop.
var questionHookOptOutAgents = map[string]bool{
	"claude": true,
	"codex":  true,
}

// withQuestionHookEnv overlays the loopback question-hook context onto a
// headless run's environment and returns the (possibly unchanged) map. The
// returned map is the single source of truth for whether the run is armed and
// with which token — it is literally the bytes the child reads — so the caller
// registers `env[questionHookTokenEnv]` rather than a separately-returned copy
// the two could drift apart on. Absent means not armed;
// registerQuestionHookToken already treats an empty token as a no-op.
//
// Returns env untouched when the daemon cannot support the signal: an agent
// that does not consume it, no registrar wired, no bound hook port, or a
// failure to mint a token. Losing the question signal is a degradation, never a
// reason to fail a run, so nothing here returns an error.
//
// The two keys are applied ON TOP of the resolved overlay (account > proof >
// worktree .env), matching the tmux path's rule that managed BOSS_* keys are
// authoritative and cannot be shadowed by a repo's .env. The token is never
// logged.
//
// SCOPE: called from the fresh-start headless branch of StartSession only. The
// other headless StartByAgent sites — orphan_resume.go, rotation.go, and
// resumeHeadlessRun — are deliberately NOT armed; they restart a run whose
// token was already released, and nothing consumes the signal for a headless
// chat yet. Arming one means repeating the mint/register pair below AND giving
// it a release handoff, as StartSession's headless branch does.
func (l *Lifecycle) withQuestionHookEnv(agentName string, env map[string]string) map[string]string {
	if l.questionHooks == nil || l.hookPort == 0 || questionHookOptOutAgents[agentName] {
		return env
	}
	token, err := newQuestionHookToken()
	if err != nil {
		l.logger.Warn().Err(err).Msg("question hook: mint token failed; headless run starts without a question signal")
		return env
	}
	overlaid := make(map[string]string, len(env)+2)
	maps.Copy(overlaid, env)
	overlaid[questionHookPortEnv] = strconv.Itoa(l.hookPort)
	overlaid[questionHookTokenEnv] = token
	return overlaid
}

// registerQuestionHookToken binds a minted token to the agent session id the
// plugin actually resolved, which is normally the id the injected hook reports
// itself under and the id the question store is keyed by. A no-op for an
// unarmed run (empty token) or a run whose id never resolved.
//
// Registration necessarily happens AFTER the spawn — the token has to be in
// the child's env, but the id is only known once the agent reports it — so
// there is a window in which the injected hook could POST before its token is
// registered. ValidateRunToken answers ErrAgentRunNotFound for that, which
// handleQuestion turns into a 200 no-op, and the hook does not retry: an event
// in that window is dropped. Harmless today, because the only event that can
// fire headless is session.idle (which merely clears) and it cannot arrive
// before the run has produced output.
//
// "Normally" because agentruntime.Start mints a throwaway UUID when the caller
// passes no session id (this path does) and only re-keys to the agent's real
// `ses_…` if it appears in the run's early output within earlyOutputTimeout. If
// that window is missed the token is registered under the UUID while the
// injected hook POSTs under the real id, so ValidateRunToken answers
// ErrAgentRunNotFound and the receiver 200s without a write — the signal is
// dropped silently, exactly as if no question had been asked. That window is a
// property of the shared id-resolution path, not of this feature; it degrades
// the signal only, never the run.
func (l *Lifecycle) registerQuestionHookToken(agentSessionID, token string) {
	if token == "" || agentSessionID == "" || l.questionHooks == nil {
		return
	}
	l.questionHooks.RegisterHeadlessRunHookToken(agentSessionID, token)
}

// releaseQuestionHookToken drops a completed headless run's token. Safe for any
// id: ids that were never registered (tmux chats, unarmed runs) are no-ops.
func (l *Lifecycle) releaseQuestionHookToken(agentSessionID string) {
	if agentSessionID == "" || l.questionHooks == nil {
		return
	}
	l.questionHooks.ReleaseHeadlessRunHookToken(agentSessionID)
}

// newQuestionHookToken mints a fresh per-run bearer token. 32 bytes of entropy
// (64 hex chars), matching the shape of the Stop-hook tokens so daemon logs and
// the hook table read consistently.
func newQuestionHookToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// SetQuestionHookRegistrar wires the registry that authenticates a headless
// run's loopback question-hook POSTs (BOS-486). Leaving it unset disables the
// headless question signal entirely — no BOSS_HOOK_* is handed to the agent.
func (l *Lifecycle) SetQuestionHookRegistrar(r questionHookRegistrar) { l.questionHooks = r }
