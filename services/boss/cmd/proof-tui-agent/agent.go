package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/boss/internal/tuidriver"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// tui is the minimal surface the serve loop depends on. *tuidriver.Driver
// satisfies it directly; protocol tests pass a fake.
type tui interface {
	Screen() string
	SendString(s string) error
	PasteString(s string) error
	WaitForText(timeout time.Duration, text string) error
}

// daemonMutator is the subset of *tuitest.MockDaemon the "daemon" op drives.
// Protocol tests pass a fake; main.go passes the real mock. The pushers are
// non-blocking (TryPush* + a short timeout) and attach-validated (SessionAttached)
// so a stalled TUI can never hang the single-threaded serve loop and a push to an
// unwatched session is a loud error, never a silent no-op.
type daemonMutator interface {
	AddRepo(*pb.Repo)
	AddSession(*pb.Session)
	AddChat(*pb.ClaudeChat)
	AddCronJob(*pb.CronJob)
	AddPRs(repoID string, prs []*pb.PRSummary)
	AddTrackerIssues(repoID string, issues []*pb.TrackerIssue)
	SessionAttached(sessionID string) bool
	TryPushOutputLine(sessionID, text string, timeout time.Duration) error
	TryPushStateChange(sessionID string, prev, next pb.SessionState, timeout time.Duration) error
	TryPushSessionEnded(sessionID string, final pb.SessionState, timeout time.Duration) error
	SetRPCStall(d time.Duration)
	SetRPCStallFor(procedure string, d time.Duration)
}

// daemonPushTimeout bounds a pusher's non-blocking send. A push that cannot be
// enqueued within this window returns an error and the serve loop stays alive.
const daemonPushTimeout = 500 * time.Millisecond

// supportedOps and daemonActions back the "capabilities" op so downstream
// (BOS-219/223) can distinguish an OLD bridge — which answers `unknown op
// "capabilities"` — from a typo'd action against a new one. Kept in sync with the
// handleLine / handleDaemonOp switches below; capabilities returns them sorted.
var (
	supportedOps  = []string{"observe", "key", "type", "enter", "esc", "wait", "quit", "daemon", "capabilities"}
	daemonActions = []string{
		// mutators (home-poll-refreshed unless noted)
		"add_session", "add_repo", "add_chat", "add_cron_job",
		"add_prs",            // picker-on-demand: visible when the new-session PR picker opens
		"add_tracker_issues", // picker-on-demand: visible when the tracker-issue picker opens
		// pushers (require an attached session)
		"push_output", "state_change", "session_ended",
		// fault injection (BOS-723): wedge the daemon so a scenario can watch
		// the client's RPC bound fire and the TUI recover once it is cleared.
		"set_rpc_stall",
	}
)

// errQuit is returned by serve when a "quit" op is processed so the caller can
// tear down and exit 0.
var errQuit = errors.New("quit")

// maxLineBytes caps a single NDJSON request line. A line over the cap yields an
// error response rather than an unbounded buffer or hang.
const maxLineBytes = 1 << 20 // 1 MiB

// settleConfig controls the stability auto-settle and carries the fixed
// terminal dimensions reported by observe.
type settleConfig struct {
	poll      time.Duration
	stableFor time.Duration
	hardCap   time.Duration
	cols      int
	rows      int
}

// request is one NDJSON input line.
type request struct {
	ID        int64    `json:"id"`
	Op        string   `json:"op"`
	Keys      []string `json:"keys"`
	Text      string   `json:"text"`
	TimeoutMs int64    `json:"timeoutMs"`
	// SettleMs/HardCapMs are optional per-op settle overrides (milliseconds).
	// 0 or absent ⇒ use the process-default settleConfig. Shared with the
	// BOS-217 daemon op, which reuses resolveSettle.
	SettleMs  int64 `json:"settleMs"`
	HardCapMs int64 `json:"hardCapMs"`

	// --- BOS-217 "daemon" op fields ---
	// Action selects the daemon sub-command (see daemonActions). SessionID scopes
	// the pusher actions. The entity payloads are captured RAW (json.RawMessage)
	// and strict-decoded per-action via decodeEntity, so — like the -seed overlay
	// (plan decision #8) — an unknown field inside a nested payload fails loudly
	// naming the field, instead of being silently dropped. The top-level line
	// itself stays permissive (it mixes op/id/action with the entity payloads, so
	// it cannot be DisallowUnknownFields as a whole); only the entity sub-objects
	// are strict. Build() then supplies the shared required/enum validation.
	Action        string          `json:"action"`
	SessionID     string          `json:"sessionId"`
	Session       json.RawMessage `json:"session"`
	Repo          json.RawMessage `json:"repo"`
	Chat          json.RawMessage `json:"chat"`
	CronJob       json.RawMessage `json:"cronJob"`
	PRs           json.RawMessage `json:"prs"`
	TrackerIssues json.RawMessage `json:"trackerIssues"`
	// Pusher payloads. Text (above) carries push_output's line.
	PreviousState string `json:"previousState"`
	NewState      string `json:"newState"`
	FinalState    string `json:"finalState"`
	// set_rpc_stall payload (BOS-723). StallMs is the length of the wedge
	// window in milliseconds; exactly 0 (or an absent field) clears the stall.
	// Procedure optionally narrows the wedge to ONE unary RPC, named by its
	// bare proto method name and matched exactly, e.g. "RecordChat" (which
	// therefore does not wedge ListChats). Empty wedges every unary RPC.
	StallMs   int64  `json:"stallMs"`
	Procedure string `json:"procedure"`
}

// maxStallMs bounds one set_rpc_stall window. It matches the scenario schema's
// per-step timeoutMs cap: a wedge no step could ever outlast is a scenario
// authoring mistake, and refusing it loudly beats a run that fails every step
// after it with an unrelated-looking timeout.
const maxStallMs = 60_000

// Per-op settle-override clamp bounds (milliseconds). Positive request values
// are clamped into these ranges; non-positive values fall back to the base
// config. Shared by every settling op and by the BOS-217 daemon op.
const (
	minSettleMs  = 20
	maxSettleMs  = 5_000
	minHardCapMs = 100
	maxHardCapMs = 30_000
)

// resolveSettle returns base with stableFor/hardCap overridden from req's
// optional per-op values. poll/cols/rows are preserved. Non-positive override
// values keep the base value; positive values are clamped into the [min,max]
// bounds. The resolved hardCap is never below stableFor, so settle can always
// reach stability at least once.
func resolveSettle(base settleConfig, req request) settleConfig {
	out := base
	if req.SettleMs > 0 {
		out.stableFor = clampMs(req.SettleMs, minSettleMs, maxSettleMs)
	}
	if req.HardCapMs > 0 {
		out.hardCap = clampMs(req.HardCapMs, minHardCapMs, maxHardCapMs)
	}
	if out.hardCap < out.stableFor {
		out.hardCap = out.stableFor
	}
	return out
}

// clampMs clamps v (milliseconds) into [lo, hi] and returns a Duration.
func clampMs(v, lo, hi int64) time.Duration {
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return time.Duration(v) * time.Millisecond
}

// settle polls t.Screen() until the NORMALIZED screen is unchanged for
// st.stableFor (spinner animation ignored — see normalizeAnimation), or
// st.hardCap elapses. It returns the RAW final screen; only the stability
// comparison is normalized.
func settle(t tui, st settleConfig) string {
	deadline := time.Now().Add(st.hardCap)
	lastRaw := t.Screen()
	lastNorm := normalizeAnimation(lastRaw)
	stableSince := time.Now()
	for {
		if time.Now().After(deadline) {
			return lastRaw
		}
		time.Sleep(st.poll)
		curRaw := t.Screen()
		curNorm := normalizeAnimation(curRaw)
		if curNorm != lastNorm {
			lastRaw = curRaw
			lastNorm = curNorm
			stableSince = time.Now()
			continue
		}
		lastRaw = curRaw // keep the freshest raw frame while stable
		if time.Since(stableSince) >= st.stableFor {
			return lastRaw
		}
	}
}

// writeJSON marshals v to a single line and flushes.
func writeJSON(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return w.Flush()
}

// serve reads NDJSON requests from in and writes one NDJSON response per request
// to out. It returns nil on input EOF (parent death / closed stdin) and errQuit
// after acking a quit op. It never crashes or hangs on malformed input.
func serve(t tui, d daemonMutator, in io.Reader, out io.Writer, st settleConfig) error {
	bw := bufio.NewWriter(out)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		quit, err := handleLine(t, d, bw, line, st)
		if err != nil {
			return err
		}
		if quit {
			return errQuit
		}
	}
	if err := scanner.Err(); err != nil {
		// A line over the cap (bufio.ErrTooLong) or read error: emit an error
		// response and stop. We cannot recover the request id past this point.
		_ = writeJSON(bw, errorResponse(0, err.Error()))
		return nil
	}
	return nil
}

// handleLine processes one request line. It returns quit=true after a quit op.
func handleLine(t tui, d daemonMutator, bw *bufio.Writer, line []byte, st settleConfig) (quit bool, err error) {
	var req request
	if uerr := json.Unmarshal(line, &req); uerr != nil {
		// id may still be parseable; best-effort recover it.
		return false, writeJSON(bw, errorResponse(parseID(line), uerr.Error()))
	}

	switch req.Op {
	case "observe":
		screen := settle(t, resolveSettle(st, req))
		return false, writeJSON(bw, map[string]any{
			"id":     req.ID,
			"screen": screen,
			"cols":   st.cols,
			"rows":   st.rows,
		})
	case "key":
		for _, k := range req.Keys {
			b, kerr := tuidriver.KeyBytes(k)
			if kerr != nil {
				return false, writeJSON(bw, errorResponse(req.ID, kerr.Error()))
			}
			if serr := t.SendString(string(b)); serr != nil {
				return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
			}
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
	case "type":
		if serr := t.PasteString(req.Text); serr != nil {
			return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
	case "enter":
		if serr := t.SendString("\r"); serr != nil {
			return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
	case "esc":
		if serr := t.SendString("\x1b"); serr != nil {
			return false, writeJSON(bw, errorResponse(req.ID, serr.Error()))
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
	case "wait":
		timeout := time.Duration(req.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		if werr := t.WaitForText(timeout, req.Text); werr != nil {
			return false, writeJSON(bw, map[string]any{
				"id":     req.ID,
				"ok":     false,
				"error":  "timeout",
				"screen": t.Screen(),
			})
		}
		return false, writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
	case "quit":
		return true, writeJSON(bw, map[string]any{"id": req.ID, "ok": true})
	case "capabilities":
		// Op-discovery for BOS-219/223: an OLD bridge lacks this case and returns
		// `unknown op "capabilities"`, so the caller can tell an old binary apart
		// from a typo'd action. Both lists are returned sorted.
		return false, writeJSON(bw, capabilitiesResponse(req.ID))
	case "daemon":
		return false, handleDaemonOp(t, d, bw, req, st)
	default:
		return false, writeJSON(bw, errorResponse(req.ID, fmt.Sprintf("unknown op %q", req.Op)))
	}
}

// handleDaemonOp dispatches a {op:"daemon", action:...} request. Mutators build
// their entity through the shared fixtures overlay Build() (one validation path
// with -seed) then Add* it; after any mutation it settles and returns the settled
// screen. Home-poll-refreshed entities (sessions/repos/chats/crons) appear within
// the poll+settle window; add_prs/add_tracker_issues are picker-on-demand — they
// only render when the new-session picker opens. Pushers require a non-empty,
// currently-attached sessionId and use the non-blocking TryPush* so a stalled TUI
// never wedges the loop. set_rpc_stall injects a fault instead of data: it wedges
// the mock daemon's unary handlers (optionally only one procedure) so a scenario
// can capture the client's bounded failure and the recovery after it clears.
// Unknown action → error listing all valid actions.
func handleDaemonOp(t tui, d daemonMutator, bw *bufio.Writer, req request, st settleConfig) error {
	settled := func() error {
		return writeJSON(bw, okScreen(req.ID, settle(t, resolveSettle(st, req))))
	}

	switch req.Action {
	case "add_session":
		var payload fixtures.OverlaySession
		if err := decodeEntity(req.Session, "session", &payload); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		sess, err := payload.Build()
		if err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		d.AddSession(sess)
		return settled()
	case "add_repo":
		var payload fixtures.OverlayRepo
		if err := decodeEntity(req.Repo, "repo", &payload); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		repo, err := payload.Build()
		if err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		d.AddRepo(repo)
		return settled()
	case "add_chat":
		var payload fixtures.OverlayChat
		if err := decodeEntity(req.Chat, "chat", &payload); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		chat, err := payload.Build()
		if err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		d.AddChat(chat)
		return settled()
	case "add_cron_job":
		var payload fixtures.OverlayCronJob
		if err := decodeEntity(req.CronJob, "cronJob", &payload); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		job, err := payload.Build()
		if err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		d.AddCronJob(job)
		return settled()
	case "add_prs":
		var payload fixtures.OverlayRepoPRs
		if err := decodeEntity(req.PRs, "prs", &payload); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		repoID, prs, err := payload.Build()
		if err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		d.AddPRs(repoID, prs)
		return settled()
	case "add_tracker_issues":
		var payload fixtures.OverlayRepoIssues
		if err := decodeEntity(req.TrackerIssues, "trackerIssues", &payload); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		repoID, issues, err := payload.Build()
		if err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		d.AddTrackerIssues(repoID, issues)
		return settled()
	case "push_output":
		if err := requireAttached(d, req.SessionID, "push_output"); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		if err := d.TryPushOutputLine(req.SessionID, req.Text, daemonPushTimeout); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		return settled()
	case "state_change":
		// Validate the state names (shared fixtures mapping) BEFORE the attach
		// check so a bad state name always yields the pointerful state error.
		prev, err := fixtures.SessionStateFromName(req.PreviousState)
		if err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		next, err := fixtures.SessionStateFromName(req.NewState)
		if err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		if err := requireAttached(d, req.SessionID, "state_change"); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		if err := d.TryPushStateChange(req.SessionID, prev, next, daemonPushTimeout); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		return settled()
	case "session_ended":
		final, err := fixtures.SessionStateFromName(req.FinalState)
		if err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		if err := requireAttached(d, req.SessionID, "session_ended"); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		if err := d.TryPushSessionEnded(req.SessionID, final, daemonPushTimeout); err != nil {
			return writeJSON(bw, errorResponse(req.ID, err.Error()))
		}
		return settled()
	case "set_rpc_stall":
		// Fault injection, not a mutation: wedge (or un-wedge) the mock daemon
		// so a scenario can capture what boss does when a daemon accepts the
		// socket and then never answers. Returning the settled screen matches
		// every other action; the wedge itself takes effect immediately, so the
		// settle that follows already reflects a stalled daemon.
		if req.StallMs < 0 {
			return writeJSON(bw, errorResponse(req.ID, fmt.Sprintf("set_rpc_stall: stallMs must not be negative (got %d); use 0 to clear the stall", req.StallMs)))
		}
		if req.StallMs > maxStallMs {
			return writeJSON(bw, errorResponse(req.ID, fmt.Sprintf("set_rpc_stall: stallMs %d exceeds the %d ms maximum", req.StallMs, maxStallMs)))
		}
		window := time.Duration(req.StallMs) * time.Millisecond
		if req.Procedure == "" {
			d.SetRPCStall(window)
		} else {
			d.SetRPCStallFor(req.Procedure, window)
		}
		return settled()
	default:
		valid := append([]string(nil), daemonActions...)
		sort.Strings(valid)
		return writeJSON(bw, errorResponse(req.ID, fmt.Sprintf("unknown daemon action %q (valid: %s)", req.Action, strings.Join(valid, ", "))))
	}
}

// decodeEntity strict-decodes a daemon op's raw nested entity payload into dst.
// A missing/null payload yields a `missing "<field>" payload` error; an unknown
// nested field yields a DisallowUnknownFields error naming it — the same
// loud-failure contract as the -seed overlay (plan decision #8), so a scenario
// author's typo in a daemon payload never silently drops a field.
func decodeEntity(raw json.RawMessage, field string, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("daemon: missing %q payload", field)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("daemon %q payload: %w", field, err)
	}
	return nil
}

// requireAttached enforces the pusher precondition: a non-empty sessionId that is
// currently attached. It returns a self-explanatory error otherwise — never a
// silent no-op — so a scenario that forgot to navigate into the session fails
// loudly.
func requireAttached(d daemonMutator, sessionID, action string) error {
	if sessionID == "" {
		return fmt.Errorf("daemon %s: sessionId is required", action)
	}
	if !d.SessionAttached(sessionID) {
		return fmt.Errorf("session %s not attached; navigate there before pushing", sessionID)
	}
	return nil
}

// capabilitiesResponse reports every supported op and daemon action, sorted.
func capabilitiesResponse(id int64) map[string]any {
	ops := append([]string(nil), supportedOps...)
	sort.Strings(ops)
	actions := append([]string(nil), daemonActions...)
	sort.Strings(actions)
	return map[string]any{"id": id, "ok": true, "ops": ops, "daemonActions": actions}
}

func okScreen(id int64, screen string) map[string]any {
	return map[string]any{"id": id, "ok": true, "screen": screen}
}

func errorResponse(id int64, msg string) map[string]any {
	return map[string]any{"id": id, "ok": false, "error": msg}
}

// parseID best-effort extracts the "id" field from a malformed line so error
// responses can still echo it. Returns 0 if not recoverable.
func parseID(line []byte) int64 {
	var partial struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(line, &partial); err != nil {
		return 0
	}
	return partial.ID
}
