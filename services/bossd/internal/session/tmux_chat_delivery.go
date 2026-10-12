package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/detach"
)

// DeliveryOutcome is how a launch's wait for its first-input delivery receipt
// ended. It is never reduced to a bool: each non-confirmed outcome has its own
// cause and its own log/reason text.
type DeliveryOutcome string

const (
	// DeliveryConfirmed: a receipt arrived at or after launch.
	DeliveryConfirmed DeliveryOutcome = "confirmed"
	// DeliveryAgentExited: the pane vanished before any receipt. Checked on
	// every poll, so it fails at once rather than after the deadline.
	DeliveryAgentExited DeliveryOutcome = "agent-exited"
	// DeliveryReceiptMissing: the deadline passed with no receipt.
	DeliveryReceiptMissing DeliveryOutcome = "receipt-missing"
	// DeliveryRolloutUnresolved: the runner never resolved a provider session
	// id, so its transcript could not even be read (Codex).
	DeliveryRolloutUnresolved DeliveryOutcome = "rollout-unresolved"
)

// deliveryReceiptPollInterval paces the receipt wait. Each poll is one tmux
// has-session plus, for a runner confirmed through its transcript, one
// ReadTranscript.
const deliveryReceiptPollInterval = 250 * time.Millisecond

// deliveryReceiptTranscriptTail bounds the ReadTranscript tail the wait
// inspects. The first input lands near the end of a fresh or resumed transcript
// during the start window, so a short tail is enough.
const deliveryReceiptTranscriptTail = 20

// DeliveryUnconfirmedError is returned by StartTmuxChat when a required
// first-input delivery was not confirmed. The pane has been killed and the chat
// stamped failed; nothing re-delivers the input.
type DeliveryUnconfirmedError struct {
	Outcome DeliveryOutcome
	Reason  string
}

func (e *DeliveryUnconfirmedError) Error() string { return e.Reason }

// deliveryWait is one launch's confirmation request.
type deliveryWait struct {
	client         agent.AgentRunnerClient
	sessionID      string
	agentSessionID string
	tmuxName       string
	worktreePath   string
	launchedAt     time.Time
	// receiptKey is the id the runner's receipt hook posts under: the session
	// id bossd handed BuildInteractiveCommand.
	receiptKey string
	// knownProviderSessionID is a provider id already bound to the chat (a
	// resume), or "" when the wait must resolve it. On a resume it is also the
	// receiptKey, since that is the id bossd launched the agent with.
	knownProviderSessionID string
}

// deliveryResult is how a wait ended, and what it learned about the provider
// session id.
type deliveryResult struct {
	outcome DeliveryOutcome
	// resolved reports that the runner resolved the chat's provider session id
	// during the wait (bound below when provider-owned, or adopted from bossd's
	// own id), so the caller's own post-launch discovery has nothing left to do.
	resolved bool
	// boundProviderSessionID is the provider-owned id the wait bound, if any.
	boundProviderSessionID string
}

// awaitDeliveryReceipt waits for proof that the agent submitted the first input
// it was launched with. Two receipt channels are polled, whichever the runner
// provides:
//
//   - the prompt-submitted hook receipt (Claude's UserPromptSubmit), keyed by
//     w.receiptKey and accepted only if it arrived at or after launch;
//   - the agent's own transcript (Codex's rollout): ResolveInteractiveSessionID
//     is retried on every poll until a provider id resolves — well past the 2s
//     foreground window of persistFreshProviderSessionID — and its transcript
//     is then read for a user message timestamped at or after launch. Only a
//     provider-OWNED id (one that differs from bossd's agent_session_id) is
//     read this way; an agent that adopts bossd's id (Claude) confirms through
//     its hook, since its transcript can carry synthetic user entries.
//
// The pane is checked on every poll so an agent that exits fails at once. The
// deadline is the configured session-start readiness deadline: the argv path no
// longer spends that wait on the composer, so every caller's budget is
// unchanged.
func (l *Lifecycle) awaitDeliveryReceipt(ctx context.Context, w deliveryWait) deliveryResult {
	waitCtx, cancel := context.WithTimeout(ctx, l.tmux.SessionStartReadyDeadline())
	defer cancel()

	panePID := 0
	if pid, err := l.tmux.PanePID(waitCtx, w.tmuxName); err == nil && pid > 0 {
		panePID = pid
	}
	resolved := w.knownProviderSessionID != ""
	transcriptID := ""
	if resolved && w.knownProviderSessionID != w.agentSessionID {
		transcriptID = w.knownProviderSessionID
	}
	result := deliveryResult{resolved: resolved}

	ticker := time.NewTicker(deliveryReceiptPollInterval)
	defer ticker.Stop()
	for {
		if l.deliveryReceipts.receivedSince(w.receiptKey, w.launchedAt) {
			result.outcome = DeliveryConfirmed
			return result
		}
		if alive, err := l.tmux.HasSessionStatus(waitCtx, w.tmuxName); err == nil && !alive {
			result.outcome = DeliveryAgentExited
			return result
		}
		if !resolved {
			resp, err := w.client.ResolveInteractiveSessionID(waitCtx,
				newResolveInteractiveSessionIDRequest(w.worktreePath, w.receiptKey, panePID, w.launchedAt))
			if id := resp.GetSessionId(); err == nil && resp.GetFound() && id != "" {
				resolved = true
				result.resolved = true
				if id != w.agentSessionID {
					transcriptID = id
					l.bindProviderSessionID(waitCtx, w.agentSessionID, id)
					result.boundProviderSessionID = id
				}
			}
		}
		if transcriptID != "" && l.transcriptHasUserMessageSince(waitCtx, w, transcriptID) {
			result.outcome = DeliveryConfirmed
			return result
		}
		select {
		case <-waitCtx.Done():
			result.outcome = DeliveryReceiptMissing
			if !resolved {
				result.outcome = DeliveryRolloutUnresolved
			}
			return result
		case <-ticker.C:
		}
	}
}

// confirmLaunchDelivery runs the receipt wait for a launch whose runner
// consumed its first input from argv, and acts on the outcome:
//
//   - confirmed: the launch stands; the input file (already read by the login
//     shell) is removed if still present.
//   - anything else on a required input: the pane tail is captured into the
//     reason, then failStartBestEffort kills the pane and stamps the chat
//     failed, so an errored chat can never run its input late and be
//     duplicated by a relaunch. Nothing re-delivers. The returned error makes
//     StartTmuxChat's deferred cleanup remove the input file.
//   - anything but agent-exited on a DeliveryOptional input (the
//     account-switch notice): a warning, and the pane is kept. Its input file
//     is left for the login shell, which may still be about to read it; the
//     startup sweep collects it otherwise. An agent that exited has no pane to
//     keep, so it fails like a required input.
func (l *Lifecycle) confirmLaunchDelivery(ctx context.Context, input ChatInput, launch launchInput, w deliveryWait, logPath string) (deliveryResult, error) {
	res := l.awaitDeliveryReceipt(ctx, w)
	if res.outcome == DeliveryConfirmed {
		l.logger.Info().
			Str("session", w.sessionID).
			Str("agentSessionID", w.agentSessionID).
			Str("deliveryOutcome", string(res.outcome)).
			Msg("first input delivery confirmed")
		l.removeInitialInputFile(launch.path, w.sessionID)
		return res, nil
	}
	reason := deliveryFailureReason(res.outcome, l.tmux.SessionStartReadyDeadline(), l.startFailureTail(ctx, w.tmuxName, logPath))
	if input.DeliveryOptional && res.outcome != DeliveryAgentExited {
		l.logger.Warn().
			Str("session", w.sessionID).
			Str("agentSessionID", w.agentSessionID).
			Str("deliveryOutcome", string(res.outcome)).
			Msg("optional first input not confirmed; the pane is kept")
		return res, nil
	}
	l.logger.Warn().
		Str("session", w.sessionID).
		Str("agentSessionID", w.agentSessionID).
		Str("deliveryOutcome", string(res.outcome)).
		Msg("first input delivery not confirmed; failing the start and killing the pane")
	l.failStartBestEffort(ctx, w.sessionID, w.agentSessionID, w.tmuxName, reason)
	return res, &DeliveryUnconfirmedError{Outcome: res.outcome, Reason: reason}
}

// knownProviderSessionID returns the provider session id already bound to a
// resumed chat, or "" when there is none to reuse.
func knownProviderSessionID(chat *models.AgentChat) string {
	if chat == nil || chat.ProviderSessionID == nil {
		return ""
	}
	return *chat.ProviderSessionID
}

// transcriptHasUserMessageSince reports whether the agent's transcript holds a
// user message timestamped at or after launch. A resumed transcript already
// holds older user turns; those never count. A read error is "not yet".
func (l *Lifecycle) transcriptHasUserMessageSince(ctx context.Context, w deliveryWait, transcriptID string) bool {
	resp, err := w.client.ReadTranscript(ctx, &bossanovav1.ReadTranscriptRequest{
		WorkDir:        w.worktreePath,
		AgentSessionId: transcriptID,
		MaxMessages:    deliveryReceiptTranscriptTail,
	})
	if err != nil || !resp.GetExists() {
		return false
	}
	return userMessageSince(resp.GetMessages(), w.launchedAt)
}

// userMessageSince reports whether msgs holds a user message whose timestamp is
// at or after since. Transcript timestamps are millisecond-precision, so since
// is truncated to the millisecond before comparing; an unparsable or missing
// timestamp cannot prove anything and never counts.
func userMessageSince(msgs []*bossanovav1.ChatMessage, since time.Time) bool {
	floor := since.Truncate(time.Millisecond)
	for _, m := range msgs {
		if m.GetRole() != "user" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, m.GetTimestamp())
		if err != nil {
			continue
		}
		if !ts.Before(floor) {
			return true
		}
	}
	return false
}

// deliveryFailureReason builds the start_error text for a non-confirmed
// outcome, carrying the pane tail so the operator sees what the agent showed.
func deliveryFailureReason(outcome DeliveryOutcome, deadline time.Duration, tail string) string {
	var cause string
	switch outcome {
	case DeliveryAgentExited:
		cause = "the agent exited before submitting its first input"
	case DeliveryRolloutUnresolved:
		cause = fmt.Sprintf("the agent's session never resolved within %s, so its first input could not be confirmed", deadline)
	default:
		cause = fmt.Sprintf("no delivery receipt for the first input within %s", deadline)
	}
	reason := fmt.Sprintf("first input not confirmed (%s): %s", outcome, cause)
	if tail = strings.TrimSpace(tail); tail != "" {
		reason += "; pane tail:\n" + tail
	}
	return reason
}

const (
	startFailureTailLines = 15
	startFailureTailBytes = 1500
)

// ansiEscapeRe matches CSI and OSC terminal escape sequences, which the
// pipe-pane log carries verbatim.
var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// startFailureTail returns the last lines the agent showed, for a failure
// reason. It captures the live pane on a detached budget (the caller's context
// is often the one that just expired); a pane that is already gone falls back
// to the tail of its pipe-pane log.
func (l *Lifecycle) startFailureTail(ctx context.Context, tmuxName, logPath string) string {
	var tail string
	if l.tmux != nil && tmuxName != "" {
		detach.Cleanup(ctx, detach.CleanupBudget, func(captureCtx context.Context) {
			if out, err := l.tmux.CapturePane(captureCtx, tmuxName); err == nil {
				tail = out
			}
		})
	}
	if strings.TrimSpace(tail) == "" {
		tail = readFileTail(logPath, 64<<10)
	}
	return lastLines(ansiEscapeRe.ReplaceAllString(tail, ""), startFailureTailLines, startFailureTailBytes)
}

// readFileTail returns up to max trailing bytes of path, or "" on any error.
func readFileTail(path string, maxBytes int64) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	if info.Size() > maxBytes {
		if _, err := f.Seek(info.Size()-maxBytes, io.SeekStart); err != nil {
			return ""
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(data)
}

// lastLines keeps the last n non-blank lines of s, then at most maxBytes from
// the end.
func lastLines(s string, n, maxBytes int) string {
	var kept []string
	lines := strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if line := strings.TrimRight(lines[i], " \t"); strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	out := strings.Join(kept, "\n")
	if len(out) > maxBytes {
		cut := len(out) - maxBytes
		for cut < len(out) && !utf8.RuneStart(out[cut]) {
			cut++
		}
		out = out[cut:]
	}
	return out
}
