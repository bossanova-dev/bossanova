package agent

// The modal half of the BOS-600 delivery readiness gate, on the agent side of
// the seam: "is this pane showing a menu rather than a composer?" answered by
// asking the agent's own plugin.
//
// It lives here rather than in internal/server because it is no longer only the
// SendChatMessage path's business. Session start delivers into the same panes
// through internal/session, and an agent that boots onto an interstitial
// (BOS-894) blocks input before any chat message exists. Both callers need the
// same answer built the same way; a second copy would be a second grammar to
// keep in step.
//
// It deliberately returns a bare func rather than tmux.ModalDetector so this
// package never imports internal/tmux. internal/tmux already depends on nothing
// but the stdlib for its gate, and the named type is defined there precisely so
// the tmux client can stay ignorant of agents; importing it back would close
// that loop for no benefit. Go's assignability makes the bare func usable as a
// tmux.ModalDetector at every call site, including a nil one.

import (
	"context"
	"sync"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ModalPaneChecker is the "does this pane block input right now?" question.
//
// It is an ALIAS, not a defined type, and that is load-bearing. Go's
// assignability rule requires one side to be unnamed, so a defined type here
// could not be assigned to tmux.ModalDetector — every call site would need an
// explicit conversion, and the whole point of returning a bare func is that it
// drops into the gate untouched. An alias is the unnamed type, so it names the
// concept for readers without introducing one.
type ModalPaneChecker = func(ctx context.Context, pane []byte) (bool, error)

// NewModalPaneChecker returns the modal check for one agent, or nil when it
// cannot be answered.
//
// It routes to the agent's plugin over AgentRunnerService.HasQuestionPrompt
// rather than matching glyphs here, because each agent's modal grammar belongs
// to that agent: codex's approval, request_user_input and update-interstitial
// grammars live in the codex plugin, Claude's in bossalib/statusdetect.
// Recognising them here would fork two grammars that already exist and drift
// from them silently — and reaching into the plugin packages directly would
// cross a module boundary.
//
// It reads blocks_input, NOT has_prompt. The two are different predicates with
// opposite failure costs: has_prompt is the notify signal the status poller
// uses, and it fires for a conversational "…?" asked with a live, empty
// composer. Gating delivery on that would refuse to answer the very question
// the agent just asked — the commonest reason anyone sends a chat message —
// and broadcast fan-out and GitHub callbacks, which deliver through the same
// RPC, would inherit the failure. blocks_input is the strict modal subset: the
// composer is gone and a keystroke selects.
//
// A nil client returns a nil checker, which disables the check. That is
// deliberate fail-open, mirroring the poller: an unloaded plugin must never
// become a new reason delivery fails. Callers resolve the client themselves
// (from a registry, from the chat's own agent handle) and this constructor does
// not care which — it only refuses to build a checker over nothing.
func NewModalPaneChecker(client AgentRunnerClient, agentName string, logger zerolog.Logger) ModalPaneChecker {
	if client == nil {
		return nil
	}
	// The readiness gate treats a checker error as "not a modal", so a wedged or
	// version-skewed plugin cannot become a new reason delivery stops. That
	// fail-open is deliberate but it is also INVISIBLE: a permanently failing
	// HasQuestionPrompt silently restores the pre-BOS-600 behaviour with no
	// signal anywhere, and the next Enter into a menu would look like an
	// unexplained regression. Report the first failure per checker — one is
	// built per delivery, so this is one line per degraded delivery even if the
	// gate's probe budget is ever widened.
	var reportOnce sync.Once
	return func(ctx context.Context, pane []byte) (bool, error) {
		resp, err := client.HasQuestionPrompt(ctx, &bossanovav1.HasQuestionPromptRequest{PaneContent: pane})
		if err != nil {
			reportOnce.Do(func() {
				logger.Warn().Err(err).Str("agent", agentName).
					Msg("modal check unavailable; delivering without the BOS-600 modal gate")
			})
			return false, err
		}
		return resp.GetBlocksInput(), nil
	}
}
