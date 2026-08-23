// Package config manages global Bossanova settings stored as a JSON file
// in the user's config directory.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/buildinfo"
)

const settingsPathEnv = "BOSS_SETTINGS_PATH"

// refuseDefaultEnv, when set to "1", marks the process as running under test so
// SaveTo refuses to overwrite the developer's real settings.json. The test
// harnesses set it in the boss subprocess (where testing.Testing() is false) so
// an E2E test that forgets to point BOSS_SETTINGS_PATH at a temp file fails
// loudly instead of clobbering the real file. It has no effect on shipped
// binaries (unset). Reads are never blocked — only writes to the real default.
// Intended only for test harnesses: do not set it in a real shell, or the
// shipped binary will refuse to save its settings.
const refuseDefaultEnv = "BOSS_REFUSE_DEFAULT_SETTINGS"

// realDefaultPath is the OS default settings path computed once at package init,
// before any t.Setenv can redirect HOME. It is the path the under-test guard
// refuses. Empty if it cannot be resolved (the guard then no-ops).
var realDefaultPath = func() string {
	if dir, err := DefaultAppDataDir(); err == nil {
		return filepath.Join(dir, "settings.json")
	}
	return ""
}()

// initEnvSettingsPath is BOSS_SETTINGS_PATH as it was at process start, captured
// once at package init before any t.Setenv can change it. For an in-process test
// binary launched from a developer shell (e.g. via direnv) this is the
// developer's real settings.json, so the under-test guard refuses writes to it —
// catching tests that redirect HOME but forget BOSS_SETTINGS_PATH (which
// config.Path() consults before HOME). Empty when the env var is unset (CI) or
// relative, in which case the guard no-ops. It is NOT refused for the subprocess
// sentinel: a test harness deliberately points BOSS_SETTINGS_PATH at its own temp
// file before spawning, so there this path IS the legitimate write target.
var initEnvSettingsPath = resolveInitEnvSettingsPath()

// resolveInitEnvSettingsPath reads BOSS_SETTINGS_PATH and returns its cleaned
// absolute value, or "" when the var is unset or relative. It is split out of
// the package-init var above so the env-gating logic can be unit-tested with
// t.Setenv without re-running package initialization.
func resolveInitEnvSettingsPath() string {
	if p := os.Getenv(settingsPathEnv); p != "" && filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return ""
}

// guardRealDefaultWrite returns an error when a test would overwrite a real
// settings.json. It is the pure, unit-testable core of the write guard: reads
// are never blocked, only a write whose resolved path equals a real settings
// location while running under test. Tests must isolate via BOSS_SETTINGS_PATH (a
// temp file) or a redirected HOME so the resolved path matches neither location.
//
// Two locations are guarded:
//   - realDefault: the OS-default path (HOME-derived). Refused for both the
//     in-process test binary and the subprocess sentinel.
//   - envPath: BOSS_SETTINGS_PATH captured at process start. Refused only for the
//     in-process test binary (inProcessTest); the subprocess sentinel points
//     BOSS_SETTINGS_PATH at its own temp file, so there envPath is legitimate.
func guardRealDefaultWrite(path string, inProcessTest, sentinelSet bool, realDefault, envPath string) error {
	clean := filepath.Clean(path)
	if (inProcessTest || sentinelSet) && realDefault != "" && clean == realDefault {
		return fmt.Errorf(
			"refusing to overwrite the real settings path %q under test: set %s to an absolute temp path or redirect HOME to a temp dir",
			realDefault, settingsPathEnv)
	}
	if inProcessTest && envPath != "" && clean == envPath {
		return fmt.Errorf(
			"refusing to overwrite the developer's %s path %q under test: point %s at an absolute temp file (a redirected HOME alone is not enough — %s takes precedence)",
			settingsPathEnv, envPath, settingsPathEnv, settingsPathEnv)
	}
	return nil
}

func refusesDefaultSettingsWrite() bool {
	return os.Getenv(refuseDefaultEnv) == "1"
}

// PluginConfig describes a single plugin to load.
type PluginConfig struct {
	Name    string            `json:"name"`
	Path    string            `json:"path"`
	Enabled bool              `json:"enabled"`
	Version string            `json:"version,omitempty"`
	Config  map[string]string `json:"config,omitempty"`
}

// RepairSkills maps repair workflow operations to skill names.
type RepairSkills struct {
	Repair string `json:"repair,omitempty"`
}

// RepairConfig holds configuration for the repair plugin.
type RepairConfig struct {
	Skills                     RepairSkills `json:"skills,omitzero"`
	CooldownMinutes            int          `json:"cooldown_minutes,omitempty"`
	PollIntervalSeconds        int          `json:"poll_interval_seconds,omitempty"`
	SweepIntervalMinutes       int          `json:"sweep_interval_minutes,omitempty"`
	IdleRepairThresholdMinutes int          `json:"idle_repair_threshold_minutes,omitempty"`
}

// CooldownDuration returns the configured cooldown or the default of 1 minute.
func (c RepairConfig) CooldownDuration() time.Duration {
	if c.CooldownMinutes > 0 {
		return time.Duration(c.CooldownMinutes) * time.Minute
	}
	return 1 * time.Minute
}

// PollInterval returns the configured poll interval or the default of 5 seconds.
func (c RepairConfig) PollInterval() time.Duration {
	if c.PollIntervalSeconds > 0 {
		return time.Duration(c.PollIntervalSeconds) * time.Second
	}
	return 5 * time.Second
}

// IdleRepairThreshold returns the configured idle threshold or the default of 5 minutes.
// When a session has a live chat but its most recent output is older than this
// threshold, the repair plugin treats the chat as idle and proceeds with repair.
func (c RepairConfig) IdleRepairThreshold() time.Duration {
	if c.IdleRepairThresholdMinutes > 0 {
		return time.Duration(c.IdleRepairThresholdMinutes) * time.Minute
	}
	return 5 * time.Minute
}

// SkillName returns the configured repair skill name or the default.
func (c RepairConfig) SkillName() string {
	if c.Skills.Repair != "" {
		return c.Skills.Repair
	}
	return "boss-repair"
}

// StallDetectionConfig holds the per-phase thresholds bossd's status poller uses
// to decide that a chat claiming CHAT_STATUS_WORKING has stopped making semantic
// progress (BOS-667). The phases come from the agent runner's progress-liveness
// RPC, and each gets its own threshold because a bare "the transcript hasn't
// grown in N minutes" signal false-positives on every long tool call:
//
//   - AWAITING_MODEL — the agent owes an assistant message, so a model request is
//     in flight. Nothing but the round-trip should be happening; the threshold is
//     tight.
//   - EXECUTING_TOOL — a tool is running and legitimately writes nothing to the
//     transcript until it returns (a `make test-all` can run for a quarter of an
//     hour). The threshold is generous.
//
// Both defaults are deliberately well above a normal round-trip. They are meant
// to be tuned with real data: a false "your session is dead" banner burns
// operator trust faster than the missed detection does, so prefer raising them.
type StallDetectionConfig struct {
	AwaitingModelMinutes int `json:"awaiting_model_minutes,omitempty"`
	ExecutingToolMinutes int `json:"executing_tool_minutes,omitempty"`
}

// AwaitingModelThreshold returns the configured AWAITING_MODEL stall threshold or
// the default of 5 minutes. A non-positive value (unset, or a hand-edited
// settings.json carrying a negative) falls back to the default rather than
// flagging every chat instantly.
func (c StallDetectionConfig) AwaitingModelThreshold() time.Duration {
	if c.AwaitingModelMinutes > 0 {
		return time.Duration(c.AwaitingModelMinutes) * time.Minute
	}
	return 5 * time.Minute
}

// ExecutingToolThreshold returns the configured EXECUTING_TOOL stall threshold or
// the default of 45 minutes. Non-positive values fall back to the default for the
// same reason AwaitingModelThreshold does.
func (c StallDetectionConfig) ExecutingToolThreshold() time.Duration {
	if c.ExecutingToolMinutes > 0 {
		return time.Duration(c.ExecutingToolMinutes) * time.Minute
	}
	return 45 * time.Minute
}

// TmuxReaperConfig holds the policy knobs for bossd's orphaned-tmux reaper
// (BOS-846), which reconciles live tmux panes against the database and kills
// boss-owned panes no row accounts for.
//
// Every default here is the safe one, because the failure mode is killing a
// running agent rather than leaking a pane:
//
//   - Enabled defaults OFF, so an upgrade never silently arms a killer.
//   - DryRun defaults ON, so the first thing an operator who flips Enabled gets
//     is a log of what would have been reaped, not a reaping.
//   - ReapUnstamped defaults OFF: a pane with no BOSS_DAEMON_ID predates the
//     stamp or belongs to something else, and is not ours to kill.
//
// All three are *bool rather than bool because the interesting states are
// three-valued — unset, explicitly true, explicitly false — and the bool zero
// value cannot distinguish "unset" from "off" for a knob whose default is on.
type TmuxReaperConfig struct {
	Enabled              *bool `json:"enabled,omitempty"`
	DryRun               *bool `json:"dry_run,omitempty"`
	ReapUnstamped        *bool `json:"reap_unstamped,omitempty"`
	SweepIntervalSeconds int   `json:"sweep_interval_seconds,omitempty"`
	GracePeriodSeconds   int   `json:"grace_period_seconds,omitempty"`
}

// IsEnabled reports whether the reaper runs at all. Unset means off.
func (c TmuxReaperConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// IsDryRun reports whether the reaper only logs what it would kill. Unset means
// dry-run: the operator must opt in to destruction twice, once to enable the
// sweep and once to arm it.
func (c TmuxReaperConfig) IsDryRun() bool {
	return c.DryRun == nil || *c.DryRun
}

// ReapsUnstamped reports whether panes carrying no BOSS_DAEMON_ID are eligible.
// Unset means no.
func (c TmuxReaperConfig) ReapsUnstamped() bool {
	return c.ReapUnstamped != nil && *c.ReapUnstamped
}

// SweepInterval returns the configured gap between sweeps or the default of 5
// minutes. A non-positive value (unset, or a hand-edited negative) falls back
// rather than spinning the sweep loop.
func (c TmuxReaperConfig) SweepInterval() time.Duration {
	if c.SweepIntervalSeconds > 0 {
		return time.Duration(c.SweepIntervalSeconds) * time.Second
	}
	return 5 * time.Minute
}

// GracePeriod returns how old a pane must be, and how long it must have been
// unaccounted-for, before it can be reaped; the default is 10 minutes. A
// non-positive value falls back to the default: a zero grace window would make
// every pane instantly old enough, which is exactly the race the window exists
// to prevent (a pane is created before its row is written).
func (c TmuxReaperConfig) GracePeriod() time.Duration {
	if c.GracePeriodSeconds > 0 {
		return time.Duration(c.GracePeriodSeconds) * time.Second
	}
	return 10 * time.Minute
}

// TmuxIdleReapConfig holds the policy knobs for bossd's IDLE-pane reaper
// (BOS-886), which kills the tmux pane of a chat nobody has touched for hours
// and clears that chat's pane pointer. The chat row, its session and every
// other piece of DB state survive; the chat stays listed and re-attachable.
//
// It is deliberately NOT governed by TmuxReaperConfig, whose defaults are the
// opt-in-twice posture appropriate to an ORPHAN reap. The two differ in what a
// mistake costs (BOS-886 D6):
//
//   - An orphan reap is unrecoverable — the pane is the only trace of that
//     work, so it ships off and dry-run.
//   - An idle reap is recoverable — the chat row survives and attaching wakes
//     it — so it ships ENABLED and armed, and reclaims memory without an
//     operator having to discover a knob.
//
// Enabled is a *bool rather than a bool for the reason the block above spells
// out: the bool zero value cannot distinguish "unset" from "explicitly off"
// for a knob whose default is ON, and only the pointer form lets an operator
// turn the feature off. DryRun is a *bool for symmetry with its sibling block.
type TmuxIdleReapConfig struct {
	Enabled              *bool `json:"enabled,omitempty"`
	DryRun               *bool `json:"dry_run,omitempty"`
	IdleThresholdSeconds int   `json:"idle_threshold_seconds,omitempty"`
}

// IsEnabled reports whether idle panes are reaped at all. Unset means ON — the
// inverse of TmuxReaperConfig.IsEnabled, see the type comment for why.
func (c TmuxIdleReapConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// IsDryRun reports whether an eligible candidate is only logged. Unset means
// live: a feature that ships enabled but dry-run would reclaim nothing while
// reading as armed.
func (c TmuxIdleReapConfig) IsDryRun() bool {
	return c.DryRun != nil && *c.DryRun
}

// IdleThreshold returns how long a chat must have been reported IDLE with no
// visible pane output before its pane may be reaped; the default is 8 hours
// (28800 seconds). A non-positive value (unset, or a hand-edited negative)
// falls back rather than making every idle chat instantly eligible.
//
// This is a FLOOR, not a timer. A candidate is confirmed on a later sweep, so
// the pane dies at least this long after its last output — up to two sweep
// intervals later, never earlier.
func (c TmuxIdleReapConfig) IdleThreshold() time.Duration {
	if c.IdleThresholdSeconds > 0 {
		return time.Duration(c.IdleThresholdSeconds) * time.Second
	}
	return 8 * time.Hour
}

// TmuxDeliveryConfig holds the composer-readiness budgets bossd waits out
// before it delivers input into a tmux pane (BOS-893). The wait polls
// capture-pane for the agent's composer prompt glyph; the budget is what bounds
// that poll.
//
// There are deliberately TWO knobs rather than one, because the two delivery
// paths answer to different ceilings:
//
//   - Session start (and resume) has to cover tmux spawn, a full interactive
//     login-shell init, exec of the agent, node boot, and TUI first paint.
//     Measured on an affected host, `fish -l -i -c 'exec true'` alone ranged
//     0.75s to 12s — up to 2.4x the historical 5s budget before the agent even
//     started. No downstream deadline usefully bounds it, so it is UNCLAMPED —
//     with one caller-shaped caveat spelled out on the accessor.
//   - An established chat's send runs inside the SendChatMessage RPC, which
//     bosso relays under a fixed command deadline. A budget that outlives that
//     relay produces an ambiguous delivery the caller must not retry, because a
//     retry double-types into the composer. So it is CLAMPED (see the accessor).
//
// Both are plain ints rather than *ints: the interesting states are only
// "unset" and "a positive number of seconds", and a non-positive value has a
// single sane reading (fall back to the default) rather than a third meaning.
// "No wait at all" is not an offered option — it is the bug this block exists
// to make configurable, not a posture an operator should be able to select.
type TmuxDeliveryConfig struct {
	SessionStartReadyDeadlineSeconds int `json:"session_start_ready_deadline_seconds,omitempty"`
	SendReadyDeadlineSeconds         int `json:"send_ready_deadline_seconds,omitempty"`
}

// DefaultSessionStartReadyDeadline and DefaultSendReadyDeadline are the
// fallbacks for the two knobs above, and SendReadyDeadlineMax is the ceiling
// the send knob is clamped to. They are exported so a caller can describe the
// effective policy without re-deriving the numbers.
//
// bossd's tmux package carries its own copies of the two defaults (it must not
// import this package — see the accessors), so a drift guard in
// services/bossd/internal/tmux asserts the pairs stay equal.
const (
	DefaultSessionStartReadyDeadline = 45 * time.Second
	DefaultSendReadyDeadline         = 5 * time.Second
	SendReadyDeadlineMax             = 20 * time.Second
)

// SessionStartReadyDeadlinePluginKey is projected into every plugin process as
// BOSS_PLUGIN_session_start_ready_deadline_seconds. Plugins that need a budget
// sized against the daemon's resolved readiness deadline read this key rather
// than re-loading settings.json themselves.
const SessionStartReadyDeadlinePluginKey = "session_start_ready_deadline_seconds"

// SwitchModalProbeReserve, switchPreRespawnReserve and switchPolicyMargin are
// the three fixed allowances SwitchRespawnBudgetFor adds on top of the resolved
// readiness deadline. They are named terms so in-package guards can describe
// the arithmetic without restating one opaque sum.
//
//   - SwitchModalProbeReserve restates services/bossd/internal/tmux's
//     modalProbeTimeout. The readiness wait reserves one modal probe beyond its
//     own budget on the way out, so a FULL attempt costs the deadline plus this.
//     bossalib cannot import that package (the dependency edge runs the other
//     way), so this term is exported for
//     TestSwitchModalProbeReserveMatchesModalProbeTimeout in
//     services/bossd/internal/tmux, the drift guard that holds the pair equal
//     — the same shape DefaultSessionStartReadyDeadline already uses.
//   - switchPreRespawnReserve covers the real work the switch does BEFORE the
//     respawn starts and inside the same budget: the mid-turn check, the pane
//     kill, the transcript probe and the binding write. The readiness wait only
//     ever receives the budget MINUS this, which is why the guard on the sizing
//     asserts against that quantity rather than against the budget itself.
//   - switchPolicyMargin is deliberate headroom above one full attempt, not a
//     measurement. It is also the lever that decides the retry: what survives
//     attempt 1 is exactly this margin, so the remaining-budget guard declines
//     attempt 2 whenever the configured readiness exceeds switchPolicyMargin
//     minus SwitchModalProbeReserve — 33s at these values.
//
// Only their SUM is pinned, by the compatibility equality in
// TestSwitchRespawnBudgetAtTheDefaultIsNinetySeconds: at the 45s default the
// three add to 45s, so the derived budget still lands on exactly the 90s BOS-897
// shipped as a constant. The split between the second and third is a judgement
// nobody has measured on a slow host — re-tuning either means re-deciding the
// pair against that equality rather than nudging one.
const (
	SwitchModalProbeReserve = 2 * time.Second
	switchPreRespawnReserve = 8 * time.Second
	switchPolicyMargin      = 35 * time.Second
)

// StartChatRunSubmitVerifierTail and StartChatRunFreshSessionIDResolveTail are
// the two fixed in-RPC tails StartChatRunBudgetFor accounts for after the
// session-start readiness wait. They restate values in services/bossd/internal:
// submitVerifyWait from tmux.startDeliveryOpts and
// freshProviderSessionIDResolveDeadline from session.StartTmuxChat. bossalib
// cannot import either package, so bossd-side drift guards hold these pairs
// equal.
const (
	StartChatRunSubmitVerifierTail        = 2 * time.Second
	StartChatRunFreshSessionIDResolveTail = 2 * time.Second
)

// StartChatRunInRPCTail is the fixed floor for the work that follows the
// readiness wait inside one StartChatRun RPC.
const StartChatRunInRPCTail = StartChatRunSubmitVerifierTail + StartChatRunFreshSessionIDResolveTail

// switchBudgetAllowances is the fixed part of the budget: everything except the
// operator-configurable readiness term.
const switchBudgetAllowances = SwitchModalProbeReserve + switchPreRespawnReserve + switchPolicyMargin

// StartChatRunBudgetFor returns the per-call ceiling for the plugin→host
// StartChatRun RPC when the host resolved the supplied readiness deadline.
//
// It takes the RESOLVED readiness duration rather than a TmuxDeliveryConfig for
// the same reason SwitchRespawnBudgetFor does: the zero-value config means
// "default", so accepting it would let tests and callers accidentally exercise
// only the default path while the configured path regressed.
//
// The sizing is readiness + max(readiness, StartChatRunInRPCTail). The second
// readiness-sized term preserves the long-standing 2x policy ratio at ordinary
// values, while the tail floor keeps very small configured readiness values
// from starving the submit verifier and fresh-session-ID resolve that run inside
// the same RPC. At the 45s default the derived value is still exactly 90s.
//
// Non-positive readiness floors to DefaultSessionStartReadyDeadline, matching
// the settings accessor. Overflow saturates instead of falling back: a very
// large configured readiness must not receive the smallest budget.
func StartChatRunBudgetFor(readiness time.Duration) time.Duration {
	if readiness <= 0 {
		readiness = DefaultSessionStartReadyDeadline
	}
	tail := readiness
	if tail < StartChatRunInRPCTail {
		tail = StartChatRunInRPCTail
	}
	budget := readiness + tail
	if budget < readiness {
		return time.Duration(math.MaxInt64)
	}
	return budget
}

// SwitchResultCeiling bounds how long callers wait for an account-switch
// result; it does not bound the switch itself. The switch budget remains
// SwitchRespawnBudgetFor(readiness), so when an operator raises
// session_start_ready_deadline_seconds past SwitchBudgetCrossoverReadiness, the
// caller-side ceiling can expire before the daemon gives a verdict. The
// invariant at every configured readiness is therefore that callers are never
// told a daemon verdict the daemon did not give. Below the nominal crossover the
// daemon budget is shorter than the result ceiling on paper, but real elapsed
// ordering can still be affected by bosso work before the daemon budget starts.
// Near or above the crossover they may see the caller-side expiry in its own
// wording.
//
// Three enforcement sites must stay equal to this ceiling:
// services/bosso/internal/server/proxy.go's switchCommandDeadline,
// services/bossd/internal/server/server.go's http.Server WriteTimeout, and
// services/boss/internal/client/rpcdeadline.go's slowRPCDeadline. The latter two
// are generic ceilings for many operations, so their production values are
// pinned by relational guards rather than renamed. The operator-facing
// crossover is derived by SwitchBudgetCrossoverReadiness; if the allowances or
// this ceiling move, update services/docs/docs/reference/settings.md with the
// new derived figure.
const SwitchResultCeiling = 120 * time.Second

// SwitchBudgetCrossoverReadiness returns the nominal configured readiness at
// which SwitchRespawnBudgetFor exactly meets SwitchResultCeiling. It compares
// budgets only and excludes bosso auth/account/ownership lookup and dispatch
// latency before the daemon switch budget begins.
func SwitchBudgetCrossoverReadiness() time.Duration {
	return SwitchResultCeiling - switchBudgetAllowances
}

// SwitchRespawnBudgetFor returns the budget that bounds the whole account-switch
// primitive as it is reached from internal/server — the SwitchSessionAccount RPC
// and the "/boss switch" interception inside SendChatMessage. Without it that
// path arrives at StartTmuxChat carrying the daemon's long-lived stream context,
// so the composer-readiness wait spends its full per-attempt budget twice with
// nothing above it to stop (see the SessionStartReadyDeadline comment below).
//
// It takes the RESOLVED readiness duration rather than a TmuxDeliveryConfig, and
// that parameter shape is the whole point of BOS-948. What this replaces was a
// compiled 90s constant whose entire sizing argument was stated against
// SessionStartReadyDeadline — a setting the accessor below leaves deliberately
// UNCLAMPED and services/docs/docs/reference/settings.md actively tells slow-host
// operators to raise. Past 88 configured seconds the constant could no longer
// fund one attempt plus its modal probe, so it clamped the readiness wait BELOW
// the value the operator had chosen, and both guards on it read the compiled
// default and stayed green. Taking a config struct would have preserved that
// blind spot exactly: a zero-value struct answers with the default, so the one
// call shape a careless caller reaches for is the one that can never fail.
//
// The sizing is readiness + the three allowances above. It funds ONE full
// readiness attempt and — above the threshold named there — declines a second.
// A budget short enough to clamp attempt 1 would trade BOS-897's bug for
// BOS-896's, which is why TestSwitchRespawnBudgetFundsOneFullAttemptAtEveryConfiguredValue asserts
// the inequality at a range of CONFIGURED values rather than at the default.
//
// Two edges, both chosen rather than inherited:
//
//   - A non-positive readiness is floored to DefaultSessionStartReadyDeadline
//     before deriving, matching SessionStartReadyDeadline's own contract, so an
//     unconfigured or hand-edited-negative setting yields the 90s default budget
//     rather than a budget made of allowances alone.
//   - A readiness within one allowance of time.Duration's ceiling SATURATES
//     rather than falling back to the default. Falling back there would be this
//     ticket's own bug at a different number — it would hand the largest
//     configured readiness the smallest budget.
//
// It lives in bossalib because it is the one place both services can import
// from. services/bosso's switchCommandDeadline is bound to SwitchResultCeiling,
// which carries the cross-service result-ordering decision and derives the
// crossover where this budget meets the caller-side result ceiling.
//
// The rotation engine (services/bossd/cmd/main.go) calls lifecycle.SwitchAccount
// directly and never enters internal/server, so its automatic switch stays
// unbounded by construction. That is the intended scope boundary.
func SwitchRespawnBudgetFor(readiness time.Duration) time.Duration {
	if readiness <= 0 {
		readiness = DefaultSessionStartReadyDeadline
	}
	budget := readiness + switchBudgetAllowances
	if budget < readiness {
		// int64 nanoseconds top out around 292 years, so a readiness within one
		// allowance of that ceiling wraps negative. Saturate instead of falling
		// back: the caller asked for the longest budget it could express, and
		// every alternative here — the default, or the readiness unchanged —
		// hands the largest configured value a budget SMALLER than a modest one
		// would get, which is the inversion this function exists to remove.
		return time.Duration(math.MaxInt64)
	}
	return budget
}

// SessionStartReadyDeadline returns how long ONE session-start or resume
// readiness wait may spend looking for the agent's composer prompt before it
// gives up. The default is 45s, sized against the measured 12s shell-init
// ceiling with roughly 33s — about 3x — left for exec, node boot and TUI first
// paint.
//
// The return value is a contract, not a passthrough. A non-positive configured
// value (unset, or a hand-edited negative) yields the default rather than a
// zero or negative duration, which downstream reads as "no wait"; a value so
// large it overflows time.Duration yields the default rather than wrapping
// negative. Every representable positive value is honoured verbatim, because
// this accessor is deliberately UNCLAMPED, unlike SendReadyDeadline below: an
// operator on a pathologically slow host may raise it as far as their patience
// allows.
//
// WHO SPENDS IT. Only the four start-path …WithModal wrappers, all reached
// through injectTmuxChatInput (services/bossd/internal/session/tmux_chat.go),
// whose production callers are Lifecycle.StartTmuxChat and
// Lifecycle.sendInputToLiveTmuxChat. WakeChat is not among them: it spawns its
// pane through spawnChatTmux (services/bossd/internal/server), which never
// waits for the ready marker and so never spends this budget at all.
//
// IT IS PER-ATTEMPT, NOT A TOTAL. services/bossd/internal/tmux owns the attempt
// count (sessionStartReadyAttempts) and re-runs the whole readiness wait that
// many times; each attempt additionally reserves modalProbeTimeout
// (tmux_modal.go) for the ModalDetector call injectTmuxChatInput binds into the
// wrappers. The wall clock a doomed start costs is therefore the attempt count
// multiplied by the sum of this value and that probe reservation. That product
// is stated here as a rule rather than as a number: neither constant is
// importable from this package, so a literal here would be a copy nothing
// checks. services/docs/docs/reference/settings.md quotes the arithmetic for
// operators, and scripts/check-settings-readiness-figure.mjs derives that
// page's figure from all three constants so the two cannot drift.
//
// WHAT BOUNDS THE SPENDERS FROM ABOVE. For most of them, nothing — which is
// what leaves this knob free. The exception is Lifecycle.SwitchAccount
// (services/bossd/internal/session/switch_account.go), which respawns the
// switched chat through StartTmuxChat against a cold pane. Every route into it
// that passes through internal/server — the SwitchSessionAccount RPC and the
// "/boss switch" interception inside SendChatMessage — runs the switch
// primitive in Server.executeAccountSwitch (services/bossd/internal/server)
// under the switch respawn budget this package declares above, which is
// DERIVED from this value rather than fixed: raising this raises that budget
// with it, so the budget funds one full attempt and declines a second at every
// configured value and not only at the default. That keeps the retry loop's
// remaining-budget guard live here: it fires only when ctx.Deadline() reports
// one, and here one is reported, so attempt 2 starts when the budget left can
// fund this value plus modalProbeTimeout and is declined otherwise.
//
// Two overlapping attempts on one chat compute the same tmux name — a resumable
// switch reuses the same agentSessionID, and tmux.ChatSessionName is pure over
// repoID and agentSessionID — so what stops a second attempt tearing down the
// first's pane is SERIALIZATION, not arithmetic. Server.chatSwitchGroup, a
// singleflight.Group keyed by agentSessionID, admits one flight per chat and
// joins the rest to the leader; it is joined with DoChan and a caller-owned
// select rather than Do, so a joiner whose own context ends returns without
// touching the pane while the leader runs on. Spending the budget also leaves
// bossd's inbound command reader free: dispatchSwitchAccount
// (services/bossd/internal/upstream) hands the work to runAsyncCommand, so the
// Receive loop keeps draining other commands.
//
// The rotation engine (services/bossd/cmd/main.go) calls
// lifecycle.SwitchAccount directly, entering neither the budget nor the group,
// so an automatic rotation and a manual switch on one chat can overlap and tear
// down each other's pane; that scope boundary is deliberate and is tracked as
// deferred follow-up in the BOS-897 plan.
//
// The price of the 45s default is that a start which is genuinely going to fail
// spends the full per-attempt budget on every attempt rather than failing fast,
// and that cost serializes across a cron sweep. It is accepted: the alternative
// is failing correct starts.
func (c TmuxDeliveryConfig) SessionStartReadyDeadline() time.Duration {
	if c.SessionStartReadyDeadlineSeconds <= 0 {
		return DefaultSessionStartReadyDeadline
	}
	d := time.Duration(c.SessionStartReadyDeadlineSeconds) * time.Second
	if d <= 0 {
		// int64 nanoseconds top out around 292 years, so a large enough
		// positive seconds value wraps to zero or negative. Re-check the
		// PRODUCT, not just the input: guarding the int alone would let an
		// overflowed value out of an accessor whose contract above is that it
		// never returns a non-positive duration. "Unclamped" is preserved for
		// every value time.Duration can actually represent.
		return DefaultSessionStartReadyDeadline
	}
	return d
}

// SendReadyDeadline returns how long a send into an ESTABLISHED chat's composer
// waits for the agent's ready marker; the default is 5s, and the result is
// clamped to SendReadyDeadlineMax (20s) however large the configured value is —
// including a value large enough to overflow time.Duration, which clamps rather
// than wrapping past the ceiling it exists to enforce.
//
// The default is unchanged from the historical hardcoded budget on purpose: the
// defect BOS-893 fixes is a session-start defect, and an established chat's
// agent is already booted, so its composer is either there now or wedged.
//
// That premise has one known exception, and it is a residual instance of the
// very defect this change was made to fix. SendChatMessage with wake_if_asleep
// wakes an asleep chat first (services/bossd/internal/server/send_chat_message.go),
// and the wake does NOT spend the session-start budget: spawnChatTmux only
// launches (or resumes) the agent via NewSessionWithCmd and returns. It
// delivers nothing — argvBuilder.BuildInteractive takes no parameter that can
// carry the user's message (its appendSystemPrompt argument reaches argv, but
// the chat message has no slot at all) — and it runs no readiness wait,
// so the session-start budget is never spent there. The message is typed in
// afterwards by the ordinary send path (send_chat_message.go →
// liveTmuxSpawner.SendMessage → SendMessageWithModal), which then
// meets a pane that is still cold-booting — shell init, exec, node boot, first
// paint — on THIS budget, 5s by default and 20s at the ceiling. Whether that is
// enough is exactly the question BOS-893 answered "no" to for the start path.
//
// Raising this default is not the fix, because it is the wrong knob: the send
// clamp below is derived from a relay ceiling that a wake-then-send is already
// closer to breaching (the same wake spends up to
// providerSessionIDLegacyBackfillTimeout — 20s,
// services/bossd/internal/server/server.go — on the legacy codex backfill
// before the send begins). Closing it properly means the wake path waiting for
// readiness on its own budget, which is a change to that path rather than to
// this value. Until then, a caller that wants the long budget should wake the
// chat explicitly and send once it is live.
//
// The clamp is DERIVED, not picked. This delivery happens inside the
// SendChatMessage RPC, which bosso relays under
// `commandDeadline = 30 * time.Second`
// (services/bosso/internal/server/proxy.go). That 30s has to cover the whole
// round trip: a relay hop each way, the chat lookup, prefix rendering,
// delivery, the 2s submit-verify wait, and the verifier's one Enter retry.
// Reserving 10s for everything that is NOT the readiness wait leaves 20s here.
// Let the readiness wait exceed that and bossd can still be waiting after bosso
// has already returned CodeDeadlineExceeded — an ambiguous delivery that must
// not be retried, because a retry double-types into the composer.
//
// The dependency is real but not mechanical: commandDeadline lives in another
// module this one cannot import. If it is ever lowered, this clamp silently
// stops protecting the relay, and the 10s reservation is an estimate rather
// than a measurement — only an operator who deliberately raises this knob
// toward the ceiling is exposed to that.
func (c TmuxDeliveryConfig) SendReadyDeadline() time.Duration {
	if c.SendReadyDeadlineSeconds <= 0 {
		return DefaultSendReadyDeadline
	}
	d := time.Duration(c.SendReadyDeadlineSeconds) * time.Second
	if d <= 0 || d > SendReadyDeadlineMax {
		// d <= 0 here means int64 nanosecond overflow (the non-positive input
		// already returned above), i.e. an operator asking for an enormous
		// budget. Clamping is the faithful answer to that ask and it is also
		// the safe one: without the overflow arm the `d > max` test is false
		// for a wrapped-negative d, and the clamp this whole accessor exists
		// to enforce would be silently skipped.
		return SendReadyDeadlineMax
	}
	return d
}

// ManagedAccountsConfig holds account-rotation policy knobs.
type ManagedAccountsConfig struct {
	DefaultCooldownMinutes int `json:"default_cooldown_minutes,omitempty"`
	// Enabled gates auto-rotation of headless runs. nil = unset = enabled;
	// unlike the int-based knobs below, a default-true bool can't use the
	// "if x > 0" idiom, so this is a *bool.
	Enabled                  *bool `json:"enabled,omitempty"`
	MaxRotationsPerRun       int   `json:"max_rotations_per_run,omitempty"`
	ParkSweepIntervalSeconds int   `json:"park_sweep_interval_seconds,omitempty"`

	// AutoRotateChats is the global scope for automatic rotation of interactive
	// tmux chats on a CHAT_STATUS_LIMITED transition (Epic 4.3, BOS-175). nil
	// means ON (decision D4: fully automatic by default). Distinct from Enabled,
	// which is the global kill switch for all automatic rotation.
	AutoRotateChats *bool `json:"auto_rotate_chats,omitempty"`
	// AutoRotateChatsPerRepo overrides the global interactive-chat scope per repo
	// ID (both directions). The global kill-switch/settings UX is BOS-176.
	AutoRotateChatsPerRepo map[string]bool `json:"auto_rotate_chats_per_repo,omitempty"`
	// ChatRotateMinIntervalMinutes rate-limits automatic rotation attempts per
	// chat (belt-and-braces against banner-flap loops). 0 = default (10m).
	ChatRotateMinIntervalMinutes int `json:"chat_rotate_min_interval_minutes,omitempty"`
	// UsageStalenessWindowMinutes bounds how old a cached usage snapshot may be
	// to influence default-account selection at bind time. 0 = default (30m).
	UsageStalenessWindowMinutes int `json:"usage_staleness_window_minutes,omitempty"`

	// ProactiveRotation gates the proactive pre-cap sweep (BOS-318). Unlike the
	// other rotation bools it defaults OFF (nil ⇒ false): the sweep must be an
	// explicit opt-in, never on by default.
	ProactiveRotation *bool `json:"proactive_rotation_enabled,omitempty"`
	// ProactiveSweepIntervalSeconds is the cadence of the proactive pre-cap sweep.
	// 0 ⇒ default (300s / 5m).
	ProactiveSweepIntervalSeconds int `json:"proactive_sweep_interval_seconds,omitempty"`

	// FailoverProxy gates the S7 local failover reverse proxy (BOS-320): a
	// loopback server injected as ANTHROPIC_BASE_URL that transparently swaps
	// accounts on a 429/401 upstream response without respawning the tmux pane.
	// Defaults ON (nil ⇒ true) as of the managed-accounts change: with account
	// management enabled, the proxy is injected unless explicitly disabled. Opt
	// out with failover_proxy_enabled:false or managed_accounts.enabled:false.
	FailoverProxy *bool `json:"failover_proxy_enabled,omitempty"`

	// FailoverProxyPortSetting pins the loopback failover proxy to a FIXED port so
	// a frozen ANTHROPIC_BASE_URL baked into a tmux pane survives a daemon restart
	// (BOS-409). Exposed as a plain int (the field is named distinctly from the
	// FailoverProxyPort() accessor because Go forbids a field and method sharing a
	// name). 0/unset ⇒ the accessor's default (44127); a genuine ephemeral opt-out
	// would require promoting this to a *int in a later ticket (see accessor).
	FailoverProxyPortSetting int `json:"failover_proxy_port,omitempty"`

	// ProxyDrainTimeoutSeconds bounds how long a daemon shutdown waits for
	// in-flight proxied model streams to finish before it cuts them (BOS-888).
	// Agents route every model request through the loopback failover proxy, so
	// the proxy's lifetime IS the agent's connection lifetime: a restart that
	// tears the proxy down mid-turn severs the stream ("Connection lost
	// mid-response"). It is deliberately separate from — and larger than — the
	// 5s the gRPC/hook servers share, but it is sized against ONE in-flight
	// /v1/messages response rather than a whole agent turn, because it also has
	// to fit under the shutdown ceilings above bossd (see the accessor's
	// default). It bounds worst-case `boss daemon restart` latency: a restart
	// with nothing in flight is unaffected (http.Server.Shutdown returns as soon
	// as connections are idle), and only a genuinely mid-turn restart can wait
	// this long. 0/unset or negative ⇒ the accessor's default (15s).
	ProxyDrainTimeoutSeconds int `json:"proxy_drain_timeout_seconds,omitempty"`

	// AutoResumeOrphans gates auto-resume of headless runs that a daemon restart
	// orphaned (BOS-407). Unlike the rotation bools it defaults OFF (nil ⇒ false):
	// auto-resume reverses the deliberate "a one-shot's prompt may have side
	// effects — the human decides" default, so it must be an explicit opt-in. It
	// is independent of the rotation kill switch (no ManagedAccountsEnabled()
	// coupling); the resume restarts on the SAME account, never rotating.
	AutoResumeOrphans *bool `json:"auto_resume_orphans,omitempty"`
}

// AutoRotateChatsEnabled resolves the interactive-chat auto-rotate scope for a
// repo: per-repo override → global → default ON (decision D4). It does not apply
// the global ManagedAccountsEnabled kill switch; callers gate that separately.
func (c ManagedAccountsConfig) AutoRotateChatsEnabled(repoID string) bool {
	if v, ok := c.AutoRotateChatsPerRepo[repoID]; ok {
		return v
	}
	if c.AutoRotateChats != nil {
		return *c.AutoRotateChats
	}
	return true
}

// ChatRotateMinInterval returns the per-chat automatic-rotation rate limit, or
// the default of 10 minutes when unset.
func (c ManagedAccountsConfig) ChatRotateMinInterval() time.Duration {
	if c.ChatRotateMinIntervalMinutes > 0 {
		return time.Duration(c.ChatRotateMinIntervalMinutes) * time.Minute
	}
	return 10 * time.Minute
}

// UsageStalenessWindow returns the max age a cached usage snapshot may have to
// influence default-account selection, or 30 minutes when unset.
func (c ManagedAccountsConfig) UsageStalenessWindow() time.Duration {
	if c.UsageStalenessWindowMinutes > 0 {
		return time.Duration(c.UsageStalenessWindowMinutes) * time.Minute
	}
	return 30 * time.Minute
}

// UsageRefreshInterval returns how often the daemon proactively re-probes active
// accounts' usage so selectDefault always has a snapshot inside the staleness
// window. It is half the staleness window (so a refreshed snapshot never ages
// out before the next refresh), floored at 5 minutes to avoid over-probing.
func (c ManagedAccountsConfig) UsageRefreshInterval() time.Duration {
	interval := c.UsageStalenessWindow() / 2
	if interval < 5*time.Minute {
		return 5 * time.Minute
	}
	return interval
}

// DefaultCooldown returns the configured default cooldown applied to a
// usage-limited account when the signal carries no reset time, or 60 minutes
// when unset.
func (c ManagedAccountsConfig) DefaultCooldown() time.Duration {
	if c.DefaultCooldownMinutes > 0 {
		return time.Duration(c.DefaultCooldownMinutes) * time.Minute
	}
	return 60 * time.Minute
}

// ManagedAccountsEnabled returns whether auto-rotation is enabled. Unset (nil)
// defaults to true.
func (c ManagedAccountsConfig) ManagedAccountsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// MaxRotations returns the configured cap on rotations per headless run, or
// the default of 3 when unset.
func (c ManagedAccountsConfig) MaxRotations() int {
	if c.MaxRotationsPerRun > 0 {
		return c.MaxRotationsPerRun
	}
	return 3
}

// ParkSweepInterval returns the configured interval between sweeps of parked
// sessions awaiting rotation, or the default of 60 seconds when unset.
func (c ManagedAccountsConfig) ParkSweepInterval() time.Duration {
	if c.ParkSweepIntervalSeconds > 0 {
		return time.Duration(c.ParkSweepIntervalSeconds) * time.Second
	}
	return 60 * time.Second
}

// ProactiveRotationEnabled reports whether the proactive pre-cap sweep is on.
// It defaults OFF (nil ⇒ false); the sweep is opt-in only (BOS-318).
func (c ManagedAccountsConfig) ProactiveRotationEnabled() bool {
	return c.ProactiveRotation != nil && *c.ProactiveRotation
}

// FailoverProxyEnabled reports whether the S7 local failover reverse proxy is
// on. Default-ON (nil ⇒ true) as of the managed-accounts change: with account
// management enabled, the proxy is injected as ANTHROPIC_BASE_URL for Claude
// sessions unless explicitly disabled. Injection also requires
// ManagedAccountsEnabled() (gated by the caller). Opt out with
// managed_accounts.failover_proxy_enabled:false or managed_accounts.enabled:false.
func (c ManagedAccountsConfig) FailoverProxyEnabled() bool {
	return c.FailoverProxy == nil || *c.FailoverProxy
}

// defaultFailoverProxyPort is the fixed loopback failover-proxy port used when
// none is configured. Chosen high and obscure: above common dev ranges
// (3000/5000/8000) and below the macOS ephemeral range (49152+), so the OS
// never hands that port to another process (BOS-409).
const defaultFailoverProxyPort = 44127

// FailoverProxyPort returns the fixed loopback failover-proxy port, or the
// default 44127 when unset (BOS-409). A value <= 0 (absent JSON key or a
// negative sentinel) is treated as unset and defaults to 44127 — the fix is on
// by default so a frozen ANTHROPIC_BASE_URL survives a daemon restart. Because a
// plain int can't distinguish "absent" from an explicit 0, this ticket keeps the
// surface minimal (<=0 ⇒ default); a first-class "absent vs 0" ephemeral opt-out
// is deferred to a *int only if a later UI ticket needs it. This is the
// DEFAULTING path used at bind time.
func (c ManagedAccountsConfig) FailoverProxyPort() int {
	if c.FailoverProxyPortSetting > 0 {
		return c.FailoverProxyPortSetting
	}
	return defaultFailoverProxyPort
}

// defaultProxyDrainTimeout is how long a daemon shutdown waits for in-flight
// proxied model streams before cutting them (BOS-888).
//
// The ceiling on this value is set by the shutdown budgets ABOVE bossd, not by
// how long an agent turn can run. `boss daemon stop|restart` gives up waiting
// for the socket after daemon.LifecycleShutdownTimeout and, on the installed
// path, restart RETURNS AN ERROR WITHOUT STARTING THE REPLACEMENT; macOS
// launchd SIGKILLs at its ExitTimeOut and systemd at TimeoutStopSec. A drain
// longer than those does not buy a longer drain — it buys a hard kill that
// skips the deferred database.Close and socket cleanup, plus a CLI that
// reports a false timeout and leaves the daemon down.
//
// 15s is sized against what the drain actually protects: ONE in-flight
// /v1/messages response. A Claude turn is many such requests (every tool
// round-trip is a new one), and http.Server.Shutdown closes the listener
// immediately, so the drain finishes the current response rather than the
// whole turn. That leaves room under the ceilings for the other legs of the
// same shutdown — the 10s cron drain, the sequential plugin-host stop (3s per
// plugin), and the 5s server shutdown. The relationship is pinned by
// TestLifecycleWaitTimeoutsCoverDaemonStartupAndShutdown in
// services/boss/internal/daemon — raising this without raising those is a bug,
// and that test will say so.
const defaultProxyDrainTimeout = 15 * time.Second

// maxProxyDrainTimeout caps what the setting can ask for, and it is deliberately
// EQUAL to the default: this key can shorten the drain, never lengthen it.
//
// The ceiling argument above is about the shipped default, but nothing made it
// true of a configured value — `proxy_drain_timeout_seconds: 45` would have
// produced a 10 + 45 + 24 + 5 = 84s worst-case shutdown against a 60s
// LifecycleShutdownTimeout, i.e. exactly the "restart returns an error without
// starting the replacement daemon" failure this whole change exists to prevent,
// reachable by editing one JSON key. The invariant test could not catch it
// because it read the default.
//
// Capping at the default keeps the proven arithmetic (10 + 15 + 24 + 5 = 54s,
// 6s of headroom under 60s) true for EVERY reachable configuration rather than
// for one of them. Raising this cap is therefore not a local edit: it requires
// raising daemon.LifecycleShutdownTimeout, daemon.LifecycleStartupTimeout, the
// launchd ExitTimeOut and the systemd TimeoutStopSec in lockstep, and
// TestLifecycleWaitTimeoutsCoverDaemonStartupAndShutdown — which reads this cap,
// not the default — will fail until they are.
const maxProxyDrainTimeout = defaultProxyDrainTimeout

// ProxyDrainTimeout returns the failover proxy's own shutdown drain budget, or
// the default of 15 seconds when unset (BOS-888). A value <= 0 (absent JSON
// key or a negative sentinel) is treated as unset and defaults, mirroring
// FailoverProxyPort: the drain is on by default, because the failure it
// prevents (a restart severing a mid-turn agent stream) is silent and costly.
// A value above maxProxyDrainTimeout is clamped rather than rejected — see that
// constant for why the ceiling above bossd makes a longer drain unserviceable.
func (c ManagedAccountsConfig) ProxyDrainTimeout() time.Duration {
	if c.ProxyDrainTimeoutSeconds <= 0 {
		return defaultProxyDrainTimeout
	}
	if configured := time.Duration(c.ProxyDrainTimeoutSeconds) * time.Second; configured < maxProxyDrainTimeout {
		return configured
	}
	return maxProxyDrainTimeout
}

// AutoResumeOrphansEnabled reports whether auto-resume of daemon-restart-
// orphaned headless runs is on (BOS-407). It defaults OFF (nil ⇒ false): the
// resume is an explicit opt-in because it reverses the deliberate "human
// decides" default for side-effectful one-shots. With it disabled the orphan
// sweep behaves exactly as before — the run stays in the terminal Orphaned
// state until a human nudges it.
func (c ManagedAccountsConfig) AutoResumeOrphansEnabled() bool {
	return c.AutoResumeOrphans != nil && *c.AutoResumeOrphans
}

// ProactiveSweepInterval returns the cadence of the proactive pre-cap sweep,
// or the default of 5 minutes when unset.
func (c ManagedAccountsConfig) ProactiveSweepInterval() time.Duration {
	if c.ProactiveSweepIntervalSeconds > 0 {
		return time.Duration(c.ProactiveSweepIntervalSeconds) * time.Second
	}
	return 5 * time.Minute
}

const pluginPrefix = "bossd-plugin-"

// DedupPluginConfigs returns cfgs with duplicate entries removed, keeping the
// first occurrence of each name. The second return value reports whether any
// duplicates were dropped, which callers can use to decide whether to persist
// the cleaned-up slice back to disk.
//
// Duplicates cause a second plugin subprocess to be launched with its own
// in-memory state, breaking per-session dedup in plugins like repair (each
// instance independently fires NotifyStatusChange → CreateWorkflow, yielding
// parallel chats).
func DedupPluginConfigs(cfgs []PluginConfig) ([]PluginConfig, bool) {
	if len(cfgs) <= 1 {
		return cfgs, false
	}
	seen := make(map[string]struct{}, len(cfgs))
	out := make([]PluginConfig, 0, len(cfgs))
	dropped := false
	for _, c := range cfgs {
		if _, ok := seen[c.Name]; ok {
			dropped = true
			continue
		}
		seen[c.Name] = struct{}{}
		out = append(out, c)
	}
	return out, dropped
}

// MergeDiscoveredPlugins appends any discovered plugin whose name is not
// already present in existing, returning the merged slice and the names that
// were added. Existing entries always win: their path, enabled flag, and config
// are preserved untouched, so a user who customized a plugin's path or disabled
// it keeps that choice even when discovery finds the binary on disk.
//
// This is what lets a freshly-built plugin binary (e.g. a new bossd-plugin-*
// added since settings.json was last written) load on the next daemon start
// without a hand-edit — and self-heals a config that a clobbering save stripped
// the entry from. The input slices are not mutated.
func MergeDiscoveredPlugins(existing, discovered []PluginConfig) ([]PluginConfig, []string) {
	have := make(map[string]struct{}, len(existing))
	for _, p := range existing {
		have[p.Name] = struct{}{}
	}

	merged := append([]PluginConfig(nil), existing...)
	var added []string
	for _, d := range discovered {
		if _, ok := have[d.Name]; ok {
			continue
		}
		have[d.Name] = struct{}{}
		merged = append(merged, d)
		added = append(added, d.Name)
	}
	return merged, added
}

// PluginRejection records a bossd-plugin-* binary that was found on disk but
// refused before exec, with the reason (untrusted perms, checksum mismatch,
// missing manifest). bossd surfaces these as security-level errors.
type PluginRejection struct {
	Name   string
	Path   string
	Reason string
}

// discoveryPolicy controls how strictly plugin discovery vets binaries.
type discoveryPolicy struct {
	requireSafePerms bool // reject group/world-writable or wrong-owner dirs+files
	verifyChecksums  bool // enforce plugins.sum (release builds)
	allowCWDWalk     bool // dev-mode walk up from CWD looking for bin/
}

// activePolicy derives the discovery policy from the build type. Path
// hardening always applies; checksum enforcement and the CWD walk are gated on
// release vs dev. See docs/plans/BOS-27-*.md.
func activePolicy() discoveryPolicy {
	release := buildinfo.IsReleaseBuild()
	return discoveryPolicy{
		requireSafePerms: true,
		verifyChecksums:  release,
		allowCWDWalk:     !release,
	}
}

// DiscoverPlugins scans for plugin binaries in precedence order (see
// DiscoverPluginsVerified) and returns only the binaries that passed
// verification. Rejected binaries are dropped silently here; callers that need
// to surface them (bossd) use DiscoverPluginsVerified.
func DiscoverPlugins() []PluginConfig {
	plugins, _ := DiscoverPluginsVerified()
	return plugins
}

// DiscoverPluginsVerified scans for plugin binaries in precedence order:
//  1. ../libexec/plugins/ relative to the running binary (Homebrew layout),
//     then the binary's own directory (dev mode);
//  2. (dev builds only) a bin/ directory found by walking up from CWD;
//  3. the per-user plugin dir used by upgrades.
//
// Every candidate is vetted by the active discoveryPolicy. Returns accepted
// plugins and the rejected ones (with reasons).
func DiscoverPluginsVerified() ([]PluginConfig, []PluginRejection) {
	policy := activePolicy()
	if plugins, rej := discoverPluginsFrom("", policy); len(plugins) > 0 || len(rej) > 0 {
		return plugins, rej
	}
	if policy.allowCWDWalk {
		if plugins, rej := discoverDevPluginsFromCWD(policy); len(plugins) > 0 || len(rej) > 0 {
			return plugins, rej
		}
	}
	dir, err := UserPluginDir()
	if err != nil {
		return nil, nil
	}
	return scanForPlugins(dir, policy)
}

// osExecutable indirects os.Executable so the binDir=="" fallback (which
// locates plugins relative to the running binary) is unit-testable.
var osExecutable = os.Executable

func discoverPluginsFrom(binDir string, policy discoveryPolicy) ([]PluginConfig, []PluginRejection) {
	if binDir == "" {
		exe, err := osExecutable()
		if err != nil {
			return nil, nil
		}
		resolved, err := filepath.EvalSymlinks(exe)
		if err != nil {
			return nil, nil
		}
		binDir = filepath.Dir(resolved)
	}
	libexecDir := filepath.Clean(filepath.Join(binDir, "..", "libexec", "plugins"))
	if plugins, rej := scanForPlugins(libexecDir, policy); len(plugins) > 0 || len(rej) > 0 {
		return plugins, rej
	}
	return scanForPlugins(binDir, policy)
}

func discoverDevPluginsFromCWD(policy discoveryPolicy) ([]PluginConfig, []PluginRejection) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, nil
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if plugins, rej := scanForPlugins(filepath.Join(dir, "bin"), policy); len(plugins) > 0 || len(rej) > 0 {
			return plugins, rej
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
	}
}

// platformSuffixes lists OS/arch suffixes to skip during plugin discovery
// (cross-compiled binaries in dev mode).
var platformSuffixes = []string{
	"-darwin-arm64", "-darwin-amd64",
	"-linux-arm64", "-linux-amd64",
}

// nonDiscoverablePlugins lists plugin binaries that must never be picked up by
// auto-discovery.
//
// bossd-plugin-stub-runner is a deterministic AgentRunnerService used only by
// E2E tests (it launches no real agent subprocess) and is NO_DISTRIBUTE. The
// E2E harness loads it via an explicit plugins config entry; auto-discovering
// it would surface a non-functional "stub" agent in real and dev daemons'
// agent pickers.
//
// bossd-plugin-opencode was excluded through the BOS-433..436 epic slices while
// its run/launch RPCs returned codes.Unimplemented; BOS-437 wired and
// live-validated the launch path (StartRun/StopRun/IsRunning/ExitStatus drive a
// real opencode session), so the binary is now a functional agent runner and is
// discovered like claude/codex. It is intentionally NO LONGER listed here.
var nonDiscoverablePlugins = []string{
	"bossd-plugin-stub-runner",
}

func isNonDiscoverablePlugin(name string) bool {
	return slices.Contains(nonDiscoverablePlugins, name)
}

// FilterNonDiscoverablePlugins drops any entry that names a non-discoverable
// plugin (see nonDiscoverablePlugins), returning the filtered slice and the
// names that were removed. PluginConfig.Name holds the binary name without the
// "bossd-plugin-" prefix, so it is re-prefixed before comparison.
//
// Auto-discovery (scanForPlugins) already skips these binaries, but a config
// persisted by an older daemon — built before the binary was marked
// non-discoverable — can still carry a stale entry. Filtering the persisted
// list at load keeps such an entry from loading and surfacing a non-functional
// agent (e.g. the E2E-only "stub-runner") in the picker. The explicit --plugins
// path (E2E) intentionally bypasses this so tests can still load the stub.
func FilterNonDiscoverablePlugins(cfgs []PluginConfig) ([]PluginConfig, []string) {
	out := make([]PluginConfig, 0, len(cfgs))
	var dropped []string
	for _, c := range cfgs {
		if isNonDiscoverablePlugin(pluginPrefix + c.Name) {
			dropped = append(dropped, c.Name)
			continue
		}
		out = append(out, c)
	}
	return out, dropped
}

// scanForPlugins scans dir for bossd-plugin-* executables, applying the policy.
// Cross-compiled binaries with platform suffixes are skipped (not rejections).
func scanForPlugins(dir string, policy discoveryPolicy) ([]PluginConfig, []PluginRejection) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}

	// Directory-level perms gate: an untrusted dir taints every binary in it.
	if policy.requireSafePerms {
		if ok, reason := isTrustedPath(dir); !ok {
			var rej []PluginRejection
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasPrefix(name, pluginPrefix) || hasPlatformSuffix(name) {
					continue
				}
				if isNonDiscoverablePlugin(name) {
					continue
				}
				if !isExecutableFile(filepath.Join(dir, name)) {
					continue
				}
				rej = append(rej, PluginRejection{
					Name:   name[len(pluginPrefix):],
					Path:   filepath.Join(dir, name),
					Reason: "untrusted plugin directory: " + reason,
				})
			}
			return nil, rej
		}
	}

	// Load the manifest once if checksum verification is on. A missing/invalid
	// manifest on a release build means we reject every binary (fail closed).
	var sums map[string]string
	var sumErr error
	if policy.verifyChecksums {
		sums, sumErr = loadPluginSums(dir)
	}

	var plugins []PluginConfig
	var rejections []PluginRejection
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, pluginPrefix) {
			continue
		}
		if isNonDiscoverablePlugin(name) {
			continue
		}
		path := filepath.Join(dir, name)
		if !isExecutableFile(path) {
			continue
		}
		if hasPlatformSuffix(name) {
			continue
		}
		shortName := name[len(pluginPrefix):]

		if policy.requireSafePerms {
			if ok, reason := isTrustedPath(path); !ok {
				rejections = append(rejections, PluginRejection{Name: shortName, Path: path, Reason: reason})
				continue
			}
		}
		if policy.verifyChecksums {
			if sumErr != nil {
				rejections = append(rejections, PluginRejection{Name: shortName, Path: path,
					Reason: "checksum manifest unavailable: " + sumErr.Error()})
				continue
			}
			if ok, reason := verifyPluginChecksum(path, sums); !ok {
				rejections = append(rejections, PluginRejection{Name: shortName, Path: path, Reason: reason})
				continue
			}
		}
		plugins = append(plugins, PluginConfig{Name: shortName, Path: path, Enabled: true})
	}
	return plugins, rejections
}

// VerifyConfiguredPlugins re-applies the active discovery policy's safety checks
// to an explicit plugin list — entries that came from settings.Plugins (e.g.
// persisted by `boss config init --plugin-dir`, which the official installer
// runs) or a hand-edited config rather than from a fresh auto-discovery scan.
// Auto-discovery (scanForPlugins) vets every binary it returns, but configured
// entries are exec'd by their stored path without re-checking, so on a release
// build a plugin binary swapped after `config init` would otherwise bypass the
// plugins.sum manifest the installer shipped beside the binaries. This verifies
// each configured binary against the manifest in its own directory and returns
// the accepted entries plus any rejections (so callers can fail closed). On dev
// builds (checksum enforcement off) the list is returned unchanged.
func VerifyConfiguredPlugins(cfgs []PluginConfig) ([]PluginConfig, []PluginRejection) {
	return verifyConfiguredPlugins(cfgs, activePolicy())
}

func verifyConfiguredPlugins(cfgs []PluginConfig, policy discoveryPolicy) ([]PluginConfig, []PluginRejection) {
	// Each plugin directory carries its own plugins.sum; load each at most once.
	type manifest struct {
		sums map[string]string
		err  error
	}
	manifests := make(map[string]manifest)
	accepted := make([]PluginConfig, 0, len(cfgs))
	var rejections []PluginRejection
	for _, c := range cfgs {
		dir := filepath.Dir(c.Path)
		if policy.requireSafePerms {
			if ok, reason := isTrustedPath(dir); !ok {
				rejections = append(rejections, PluginRejection{Name: c.Name, Path: c.Path,
					Reason: "untrusted plugin directory: " + reason})
				continue
			}
			if ok, reason := isTrustedPath(c.Path); !ok {
				rejections = append(rejections, PluginRejection{Name: c.Name, Path: c.Path, Reason: reason})
				continue
			}
		}
		if !policy.verifyChecksums {
			accepted = append(accepted, c)
			continue
		}
		m, ok := manifests[dir]
		if !ok {
			m.sums, m.err = loadPluginSums(dir)
			manifests[dir] = m
		}
		if m.err != nil {
			rejections = append(rejections, PluginRejection{Name: c.Name, Path: c.Path,
				Reason: "checksum manifest unavailable: " + m.err.Error()})
			continue
		}
		if ok, reason := verifyPluginChecksum(c.Path, m.sums); !ok {
			rejections = append(rejections, PluginRejection{Name: c.Name, Path: c.Path, Reason: reason})
			continue
		}
		accepted = append(accepted, c)
	}
	return accepted, rejections
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	mode := info.Mode()
	return mode.IsRegular() && mode.Perm()&0o111 != 0
}

func hasPlatformSuffix(name string) bool {
	for _, s := range platformSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// UserPluginDir returns the per-user plugin directory shared by boss upgrade
// and bossd plugin auto-discovery.
func UserPluginDir() (string, error) {
	settings, err := loadWithoutSideEffects()
	if err != nil {
		return "", err
	}
	if dir, ok, err := ConfiguredAppDataDir(settings); err != nil {
		return "", err
	} else if ok {
		return filepath.Join(dir, "plugins"), nil
	}

	dir, err := DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plugins"), nil
}

func loadWithoutSideEffects() (Settings, error) {
	p, err := Path()
	if err != nil {
		return DefaultSettings(), err
	}
	return LoadFrom(p)
}

// Settings holds global Bossanova configuration.
type Settings struct {
	WorktreeBaseDir                string                `json:"worktree_base_dir"`
	AppDataDir                     string                `json:"app_data_dir,omitempty"`
	SocketPath                     string                `json:"socket_path,omitempty"`
	DefaultAgent                   string                `json:"default_agent,omitempty"`
	InstalledAt                    time.Time             `json:"installed_at,omitzero"`
	BossCloudGuestOfferHidden      bool                  `json:"boss_cloud_guest_offer_hidden,omitempty"`
	BossCloudValueDeliveredAt      time.Time             `json:"boss_cloud_value_delivered_at,omitzero"`
	SkillsDeclined                 bool                  `json:"skills_declined,omitempty"`
	SkillsDeclinedByAgent          map[string]bool       `json:"skills_declined_by_agent,omitempty"`
	SkillsDeclinedManifestByAgent  map[string]string     `json:"skills_declined_manifest_by_agent,omitempty"`
	SkillsInstalledManifestByAgent map[string]string     `json:"skills_installed_manifest_by_agent,omitempty"`
	PollIntervalSeconds            int                   `json:"poll_interval_seconds,omitempty"`
	NotificationsEnabled           *bool                 `json:"notifications_enabled,omitempty"`
	EventTracingEnabled            bool                  `json:"event_tracing_enabled,omitempty"`
	ErrorTrackingEnabled           bool                  `json:"error_tracking_enabled,omitempty"`
	PostHogProjectToken            string                `json:"posthog_project_token,omitempty"`
	PostHogHost                    string                `json:"posthog_host,omitempty"`
	Plugins                        []PluginConfig        `json:"plugins,omitempty"`
	Repair                         RepairConfig          `json:"repair,omitzero"`
	StallDetection                 StallDetectionConfig  `json:"stall_detection,omitzero"`
	ManagedAccounts                ManagedAccountsConfig `json:"managed_accounts,omitzero"`
	TmuxReaper                     TmuxReaperConfig      `json:"tmux_reaper,omitzero"`
	TmuxIdleReap                   TmuxIdleReapConfig    `json:"tmux_idle_reap,omitzero"`
	TmuxDelivery                   TmuxDeliveryConfig    `json:"tmux_delivery,omitzero"`
	ProvidersAcknowledged          bool                  `json:"providers_acknowledged,omitempty"`
	KnownAgentProviders            []string              `json:"known_agent_providers,omitempty"`
	// DaemonName is an optional, operator-chosen display name for this
	// daemon. It is PRESENTATION METADATA ONLY: it feeds the self-reported
	// hostname bossd advertises to the orchestrator, and never the daemon's
	// routing identity (daemon_id / BOSSD_DAEMON_ID / the persisted UUID).
	// Empty (or whitespace-only) means "use the machine hostname", so the
	// key stays absent from a fresh settings.json and legacy files keep
	// inheriting the live OS hostname.
	DaemonName string `json:"daemon_name,omitempty"`
	// LoginShell is the user's interactive shell ($SHELL), captured by the TUI
	// (which runs in that shell) so the launchd daemon — which has no $SHELL —
	// can launch agents through it for per-project tool resolution.
	LoginShell string `json:"login_shell,omitempty"`
	// DaemonPathExtra lists directories PREPENDED to the PATH written into the
	// generated bossd service file (the macOS LaunchAgent plist and the Linux
	// systemd unit). It exists because a service manager never sources an
	// interactive shell config, so a toolchain installed by nodenv, nvm, asdf
	// or volta is invisible to the daemon and every `node`-based cron gate
	// exits 127 (BOS-880).
	//
	// Prepend-only is deliberate: the baseline entries are never removable, so
	// a typo here costs one tool rather than the whole daemon. There is
	// intentionally NO full-replacement override — one omitting /usr/bin would
	// produce a daemon that cannot run git, failing exactly like the bug this
	// key fixes.
	//
	// Entries must be absolute or `~/`-rooted; anything else (and anything
	// carrying a `:`, a newline, or a character that would corrupt the plist
	// XML) is dropped when the service file is rendered. Read at render time,
	// so a change takes effect on the next `boss daemon restart`.
	DaemonPathExtra []string `json:"daemon_path_extra,omitempty"`
	// SubagentDispatchGrant selects which boss-managed chats receive the bounded
	// subagent-dispatch grant in their appended system prompt. It is a raw
	// string rather than the typed SubagentDispatchGrant so an unknown value
	// round-trips through settings.json untouched instead of being rewritten;
	// parseSubagentDispatchGrant is the only place a value becomes a mode.
	// Empty (the key absent from a fresh settings.json) means the shipped
	// default, "always".
	SubagentDispatchGrant string `json:"subagent_dispatch_grant,omitempty"`
}

// SubagentDispatchGrant names which boss-managed chats bossd grants the bounded
// subagent-dispatch authority to in their appended system prompt. It is a named
// type, not a bare string, so it cannot be silently transposed with the other
// string arguments at the prompt-building call sites it is threaded through.
type SubagentDispatchGrant string

const (
	// SubagentDispatchGrantAlways is the shipped default (BOS-882): unattended
	// runs and attended chats both receive the grant.
	SubagentDispatchGrantAlways SubagentDispatchGrant = "always"
	// SubagentDispatchGrantUnattended is the operator opt-out: only unattended
	// sessions receive the grant, restoring the earlier attended behaviour in
	// which an attended chat had to be re-authorised by hand each session.
	SubagentDispatchGrantUnattended SubagentDispatchGrant = "unattended"
)

// ResolveSubagentDispatchGrant maps a Settings value to the mode bossd acts on.
//
// It never errors and never panics: nil settings, an absent key, whitespace, a
// differently-cased spelling, and any UNRECOGNISED value all resolve to
// SubagentDispatchGrantAlways. That direction is deliberate — a typo in
// settings.json must not silently withdraw the shipped default grant and
// degrade every attended chat, and it must not fail daemon startup either.
// Case and surrounding whitespace are normalised before matching so that a
// plausible spelling of the opt-out ("Unattended", " unattended ") is honoured
// rather than quietly falling through to the default it was written to remove.
// It has no production caller — the spawn path goes through
// LoadSubagentDispatchGrant, which needs the discard reason this signature
// cannot return — and that is deliberate rather than dead code awaiting
// removal. It is a library package's mode accessor for callers holding a
// Settings they already loaded, and it is what lets the prompt tests assert the
// settings-string-to-mode wiring end to end without a settings file on disk.
// Delete it only together with those tests, not on a bare no-callers reading.
func ResolveSubagentDispatchGrant(s *Settings) SubagentDispatchGrant {
	if s == nil {
		return SubagentDispatchGrantAlways
	}
	grant, _ := parseSubagentDispatchGrant(s.SubagentDispatchGrant)
	return grant
}

// parseSubagentDispatchGrant is the ONE place a settings string becomes a mode,
// returning the resolved mode and — when the operator's configured value was
// DISCARDED — the reason, or "" when their configuration was honoured.
//
// Both halves come out of a single normalisation and a single switch on
// purpose. They are two views of one decision, and splitting them across two
// functions couples their correctness with nothing enforcing it: a third mode
// added to one switch and not the other makes bossd warn about a value it
// honours, or honour a value it warns about — and the warning is the operator
// signal this whole path exists to provide. One switch cannot disagree with
// itself.
//
// The discard direction is deliberate. The resolver fails open — a typo must
// not silently degrade every attended chat, and it must not fail daemon startup
// — but failing open *silently* is the defect: the discard is exactly the case
// the operator most needs told about, and the case in which the attended
// directive's claim about their configuration is least able to back itself.
//
// It returns the reason rather than logging it because config is linked into
// the boss TUI, where zerolog's default stderr writer would corrupt the Bubble
// Tea display. The caller owns the logger; see session.ResolveSubagentGrantForSpawn.
func parseSubagentDispatchGrant(raw string) (SubagentDispatchGrant, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return SubagentDispatchGrantAlways, ""
	}
	switch SubagentDispatchGrant(strings.ToLower(trimmed)) {
	case SubagentDispatchGrantUnattended:
		return SubagentDispatchGrantUnattended, ""
	case SubagentDispatchGrantAlways:
		return SubagentDispatchGrantAlways, ""
	default:
		return SubagentDispatchGrantAlways, fmt.Sprintf(
			"subagent_dispatch_grant %q is not a recognised mode (%q or %q); "+
				"falling back to %q, so an intended opt-out is NOT in effect",
			trimmed, SubagentDispatchGrantAlways, SubagentDispatchGrantUnattended, SubagentDispatchGrantAlways)
	}
}

// LoadSubagentDispatchGrant loads global settings and resolves the grant mode,
// degrading to the shipped default when settings cannot be loaded at all.
//
// Chat spawn sites call this so the resolved mode is passed into prompt
// composition as a plain value rather than re-read inside it.
//
// The second return is a discard reason ("" when the operator's configuration
// was honoured): a failed load and an unrecognised value both resolve to the
// shipped default, and neither may pass unremarked. Failing open is deliberate
// (AC 7); failing open in silence was the defect.
func LoadSubagentDispatchGrant() (SubagentDispatchGrant, string) {
	s, err := Load()
	if err != nil {
		return SubagentDispatchGrantAlways, fmt.Sprintf(
			"settings could not be loaded (%v); falling back to subagent_dispatch_grant %q, "+
				"so any configured opt-out is NOT in effect", err, SubagentDispatchGrantAlways)
	}
	return parseSubagentDispatchGrant(s.SubagentDispatchGrant)
}

// UnmarshalJSON accepts the managed_accounts block and, for backward
// compatibility, the legacy top-level "rotation" key that predates the rename.
// managed_accounts wins when both are present; otherwise the legacy rotation
// block populates ManagedAccounts. All other fields decode normally.
func (s *Settings) UnmarshalJSON(data []byte) error {
	type alias Settings // avoid infinite recursion
	aux := &struct {
		Managed *ManagedAccountsConfig `json:"managed_accounts"`
		Legacy  *ManagedAccountsConfig `json:"rotation"`
		*alias
	}{alias: (*alias)(s)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	switch {
	case aux.Managed != nil:
		s.ManagedAccounts = *aux.Managed
	case aux.Legacy != nil:
		s.ManagedAccounts = *aux.Legacy
	}
	return nil
}

// PluginConfigBool reads a boolean-valued entry from a named plugin's
// Config map. Returns false when the plugin isn't configured, the key is
// absent, or the value isn't "true".
func PluginConfigBool(s *Settings, pluginName, key string) bool {
	if s == nil {
		return false
	}
	for _, p := range s.Plugins {
		if p.Name == pluginName {
			return p.Config[key] == "true"
		}
	}
	return false
}

// SetPluginConfigBool writes a boolean-valued entry into a named plugin's
// Config map. When value is true it stores "true"; when false it removes
// the key entirely so the JSON stays clean. If the named plugin isn't yet
// in s.Plugins, an entry is appended so the toggle isn't silently lost
// before `boss config init` runs (init later fills in Path/Enabled, while
// the Config map is preserved by name).
func SetPluginConfigBool(s *Settings, pluginName, key string, value bool) {
	if s == nil {
		return
	}
	for i := range s.Plugins {
		if s.Plugins[i].Name != pluginName {
			continue
		}
		if value {
			if s.Plugins[i].Config == nil {
				s.Plugins[i].Config = map[string]string{}
			}
			s.Plugins[i].Config[key] = "true"
		} else if s.Plugins[i].Config != nil {
			delete(s.Plugins[i].Config, key)
		}
		return
	}
	if !value {
		// Removing a key from a not-yet-configured plugin is a no-op.
		return
	}
	s.Plugins = append(s.Plugins, PluginConfig{
		Name:   pluginName,
		Config: map[string]string{key: "true"},
	})
}

// PluginConfigString returns the string value at plugin.Config[key] or "".
func PluginConfigString(s *Settings, pluginName, key string) string {
	if s == nil {
		return ""
	}
	for _, p := range s.Plugins {
		if p.Name == pluginName {
			return p.Config[key]
		}
	}
	return ""
}

// SetPluginConfigString writes a string entry into the named plugin's Config
// map. Empty value deletes the key (matches SetPluginConfigBool behaviour).
// If the named plugin isn't yet in s.Plugins, an entry is appended so the
// setting isn't silently lost before `boss config init` runs.
func SetPluginConfigString(s *Settings, pluginName, key, value string) {
	if s == nil {
		return
	}
	for i := range s.Plugins {
		if s.Plugins[i].Name != pluginName {
			continue
		}
		if value == "" {
			if s.Plugins[i].Config != nil {
				delete(s.Plugins[i].Config, key)
			}
			return
		}
		if s.Plugins[i].Config == nil {
			s.Plugins[i].Config = map[string]string{}
		}
		s.Plugins[i].Config[key] = value
		return
	}
	if value == "" {
		// Removing a key from a not-yet-configured plugin is a no-op.
		return
	}
	s.Plugins = append(s.Plugins, PluginConfig{
		Name:   pluginName,
		Config: map[string]string{key: value},
	})
}

// SetPluginConfigEnum writes a string-valued setting after validating that
// value appears in allowed. Returns an error without mutating s when the
// value is rejected.
func SetPluginConfigEnum(s *Settings, pluginName, key, value string, allowed []string) error {
	if !slices.Contains(allowed, value) {
		return fmt.Errorf("value %q not in allowed list %v for %s.%s", value, allowed, pluginName, key)
	}
	SetPluginConfigString(s, pluginName, key, value)
	return nil
}

// DisplayPollInterval returns the interval for polling PR display status.
// Defaults to 2 minutes if not configured.
func (s Settings) DisplayPollInterval() time.Duration {
	if s.PollIntervalSeconds > 0 {
		return time.Duration(s.PollIntervalSeconds) * time.Second
	}
	return 2 * time.Minute
}

// NotificationsEnabled reports whether OS-level notifications are enabled.
// Notifications default to enabled unless explicitly disabled.
func NotificationsEnabled(s Settings) bool {
	return s.NotificationsEnabled == nil || *s.NotificationsEnabled
}

// DaemonDisplayName resolves the daemon's user-facing name: the trimmed
// daemon_name override when one is configured, otherwise the caller-supplied
// machine hostname. Callers must pass the real OS hostname as the fallback and
// must keep using that hostname — never this result — for daemon-ID
// resolution, so renaming a daemon can never re-key its identity.
func DaemonDisplayName(s Settings, hostname string) string {
	if name := strings.TrimSpace(s.DaemonName); name != "" {
		return name
	}
	return hostname
}

// DefaultSettings returns settings with sensible defaults.
func DefaultSettings() Settings {
	home, _ := os.UserHomeDir()
	return Settings{
		WorktreeBaseDir: filepath.Join(home, ".bossanova", "worktrees"),
		DefaultAgent:    "claude",
	}
}

// EnsureInstalledAt returns settings with InstalledAt initialized.
// It reports whether the caller should persist the returned settings.
func (s Settings) EnsureInstalledAt(now time.Time) (Settings, bool) {
	if !s.InstalledAt.IsZero() {
		return s, false
	}
	s.InstalledAt = now.UTC()
	return s, true
}

// EnsureBossCloudValueDeliveredAt returns settings with BossCloudValueDeliveredAt
// initialized to now (UTC). It reports whether the caller should persist the
// returned settings. If BossCloudValueDeliveredAt is already set, it is left
// unchanged and the method returns false.
func (s Settings) EnsureBossCloudValueDeliveredAt(now time.Time) (Settings, bool) {
	if !s.BossCloudValueDeliveredAt.IsZero() {
		return s, false
	}
	s.BossCloudValueDeliveredAt = now.UTC()
	return s, true
}

// DefaultAppDataDir returns the platform-default directory for Bossanova's
// per-user state. The settings file, daemon database, lock, socket, and the
// plugin directory all live here when no app_data_dir/socket_path override is
// configured, so boss and bossd agree on one location on every platform.
//
//	macOS: ~/Library/Application Support/bossanova
//	Linux: $XDG_CONFIG_HOME/bossanova (defaults to ~/.config/bossanova)
//
// It does not create the directory; callers that need it on disk MkdirAll it
// themselves.
func DefaultAppDataDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bossanova"), nil
}

// Path returns the default settings file path.
// If BOSS_SETTINGS_PATH is set, it must be absolute and points directly to the
// settings file to load. Otherwise platform defaults apply.
// On macOS: ~/Library/Application Support/bossanova/settings.json
// On Linux: ~/.config/bossanova/settings.json
func Path() (string, error) {
	if p := os.Getenv(settingsPathEnv); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be absolute: %q", settingsPathEnv, p)
		}
		return filepath.Clean(p), nil
	}

	dir, err := DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// ConfiguredAppDataDir returns the configured local runtime data directory.
// The boolean is false when app_data_dir is unset and callers should keep their
// existing platform default.
func ConfiguredAppDataDir(s Settings) (string, bool, error) {
	if s.AppDataDir == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(s.AppDataDir) {
		return "", false, fmt.Errorf("app_data_dir must be absolute: %q", s.AppDataDir)
	}
	dir := filepath.Clean(s.AppDataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, fmt.Errorf("create app_data_dir %q: %w", dir, err)
	}
	return dir, true, nil
}

// ConfiguredSocketPath returns the configured Unix socket path. socket_path
// wins when set. If only app_data_dir is set, the socket defaults to
// app_data_dir/bossd.sock. The boolean is false when neither setting applies.
func ConfiguredSocketPath(s Settings) (string, bool, error) {
	if s.SocketPath != "" {
		if !filepath.IsAbs(s.SocketPath) {
			return "", false, fmt.Errorf("socket_path must be absolute: %q", s.SocketPath)
		}
		return filepath.Clean(s.SocketPath), true, nil
	}

	dir, ok, err := ConfiguredAppDataDir(s)
	if err != nil || !ok {
		return "", ok, err
	}
	return filepath.Join(dir, "bossd.sock"), true, nil
}

// Load reads settings from the default path, returning defaults if the file is missing.
func Load() (Settings, error) {
	p, err := Path()
	if err != nil {
		return DefaultSettings(), err
	}
	return LoadFrom(p)
}

// LoadFrom reads settings from a specific path, returning defaults if the file is missing.
func LoadFrom(path string) (Settings, error) {
	cleaned := filepath.Clean(path)
	data, err := os.ReadFile(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultSettings(), nil
		}
		return DefaultSettings(), err
	}

	s := DefaultSettings()
	if err := json.Unmarshal(data, &s); err != nil {
		return DefaultSettings(), err
	}
	// Backfill DefaultAgent if the file explicitly sets it to "" — that's the
	// only case this branch covers, since files that omit the key entirely
	// already inherit "claude" from the DefaultSettings() seed used as the
	// unmarshal target above. Downstream code can't tolerate an empty agent
	// name, so normalise both shapes to "claude".
	if s.DefaultAgent == "" {
		s.DefaultAgent = "claude"
	}
	return s, nil
}

// Save writes settings to the default path.
func Save(s Settings) error {
	p, err := Path()
	if err != nil {
		return err
	}
	return SaveTo(p, s)
}

// SaveTo writes settings to a specific path, creating parent directories as needed.
func SaveTo(path string, s Settings) error {
	if err := guardRealDefaultWrite(
		path,
		testing.Testing(),
		refusesDefaultSettingsWrite(),
		realDefaultPath,
		initEnvSettingsPath,
	); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	_ = syncDir(dir)
	return nil
}

func syncDir(dir string) error {
	cleaned := filepath.Clean(dir)
	f, err := os.Open(cleaned)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return syncDirectory(f)
}

// syncDirectory preserves the one platform-specific directory-sync error that
// is safe to ignore while returning every other failure to the caller.
func syncDirectory(f interface{ Sync() error }) error {
	if err := f.Sync(); err != nil && err != io.ErrUnexpectedEOF {
		return err
	}
	return nil
}
