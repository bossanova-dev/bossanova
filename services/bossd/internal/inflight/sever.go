package inflight

import (
	"github.com/rs/zerolog"
)

// MarkSevered hands each recovered agent session id to onSevered — in
// production the resumer's restart trigger — and returns how many were marked.
//
// It deliberately does NOT decide whether any of them deserves a resume prompt.
// The record only proves a stream was open when the daemon died; whether the
// chat is still stalled, still resumable, still idle and still within its
// attempt budget is the resumer's gate ladder to answer, minutes later, against
// live state. Marking is the trigger, never the verdict.
//
// Call ordering is a contract: this must run AFTER the startup pane-token
// re-adoption sweep (BOS-481). A pane whose token has not been re-adopted yet
// cannot possibly reconnect, so marking earlier would stamp "severed" on chats
// that are about to recover on their own the moment their token comes back.
func MarkSevered(logger zerolog.Logger, ids []string, onSevered func(agentSessionID string)) int {
	if onSevered == nil || len(ids) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(ids))
	marked := 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		onSevered(id)
		marked++
	}
	if marked > 0 {
		logger.Info().Int("streams", marked).
			Msg("in-flight stream record: chats whose proxied streams were severed by the previous daemon exit; queued for auto-resume evaluation")
	}
	return marked
}
