package fixtures

import (
	"fmt"
	"sort"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SeedKind identifies which settings-seeding profile a preset needs.
//
// IMPORT-CYCLE DECISION (BOS-217): the fixtures package deliberately depends
// only on the generated pb types (see the package doc), so it MUST NOT import
// tuitest to call the seed helpers directly — that would break the documented
// "any test package can import fixtures without a cycle" invariant. The two seed
// helpers also have different signatures (SeedSettingsAcknowledged(home,
// worktreeBaseDir) vs SeedFirstRunSettings(home, socketPath)). So instead of a
// SeedSettings func value, each preset declares a SeedKind and the caller (the
// proof-tui-agent bridge in main.go) maps the kind onto the right tuitest
// helper. The registry still owns World + DefaultEnv + SeedKind, which collapses
// the bridge's old fixture switches into a data-driven lookup.
type SeedKind int

const (
	// SeedAcknowledged seeds settings.json with providers_acknowledged=true so
	// the boss subprocess skips the first-run gate. The bridge maps this to
	// tuitest.SeedSettingsAcknowledged(home, worktreeBaseDir).
	SeedAcknowledged SeedKind = iota
	// SeedFirstRun seeds first-run onboarding settings (providers unacknowledged,
	// socket_path set). The bridge maps this to
	// tuitest.SeedFirstRunSettings(home, socketPath).
	SeedFirstRun
)

// Preset is a named fixture profile for the proof-tui-agent bridge: the world to
// seed into the mock daemon, which settings-seed profile to apply, and any
// default env the boss subprocess should carry.
type Preset struct {
	// World builds the deterministic dataset for this preset. It is a func (not a
	// value) so every lookup gets a fresh, independent copy of the pb pointers.
	World func() World
	// SeedKind selects the settings-seed profile; the bridge maps it to the
	// matching tuitest helper.
	SeedKind SeedKind
	// DefaultEnv is merged (as K=V) into the boss subprocess env. Always non-nil
	// (possibly empty) so callers can range over it without a nil check.
	DefaultEnv map[string]string
	// HomeFiles are extra files the bridge writes into the per-run HOME before
	// boss starts, keyed by a path RELATIVE to HOME. It exists so a preset can
	// stage host state boss reads through a real code path rather than through a
	// test hook — the slow-agent-probe preset seeds a `.bashrc` that sleeps,
	// which is the same thing a slow interactive rc does on a real machine.
	// Data, not a callback, so the fixtures package keeps its "pb types only"
	// dependency rule (see SeedKind) and every preset stays unit-testable.
	HomeFiles map[string]string
	// SettingsOverrides are merged into the settings.json the bridge seeds for
	// SeedAcknowledged presets, on top of the acknowledged/worktree defaults and
	// underneath the scenario's own env-driven overrides. Keys are settings.json
	// field names (config.Settings json tags), so a preset can turn on a plugin
	// or pin a login shell without a new bridge flag per preset.
	//
	// SeedAcknowledged ONLY. The bridge merges this inside that branch of its
	// seed switch (services/boss/cmd/proof-tui-agent/main.go), and SeedFirstRun
	// seeds a fixed unacknowledged settings.json that ignores it. That is
	// enforced, not merely documented: the bridge REFUSES to boot a first-run
	// preset that sets this field (rejectUnhonouredFirstRunFields), because a
	// silently mis-seeded run captures the wrong screen and looks exactly as
	// green as a correct one. A first-run preset that needs overrides wants the
	// bridge taught to honour them, not the guard removed.
	SettingsOverrides map[string]any
	// BootWait overrides how long the bridge waits for boss's first frame before
	// giving up. Zero means DefaultBootWait. Only a preset that deliberately
	// makes startup slow needs to raise it — slow-agent-probe blocks startup on a
	// login-shell probe that must be allowed to run out its full budget, which is
	// longer than the default wait.
	//
	// SeedAcknowledged ONLY, for the same reason and with the same enforcement:
	// a first-run preset never renders the app shell, so the bridge skips the
	// first-frame wait entirely and this value would be dropped.
	BootWait time.Duration
}

// DefaultBootWait is how long the bridge waits for boss's first frame when a
// preset does not raise it. Generous enough for a cold `-tags e2e` binary on a
// loaded CI box, short enough that a preset which never paints fails fast.
const DefaultBootWait = 10 * time.Second

// SlowLoginShellRC is the interactive rc the slow-agent-probe preset seeds as
// `.bashrc` in the per-run HOME. boss's probe runs `bash -l -c` with an explicit
// `. "$HOME/.bashrc"` prologue (loginshell.CommandLine), so this sleep is paid
// before `command -v` is ever reached — exactly the shape of the BOS-976
// incident, where pyenv/nodenv PATH globbing cost ~44s inside the user's rc.
//
// The sleep outlasts the probe budget by a wide margin on purpose. If it ever
// finished FIRST the probe would go on to run `command -v claude`, which fails
// on a CI box with no claude installed, and the scenario would land on the
// not-found screen — a green-looking capture of the wrong screen.
const SlowLoginShellRC = "# proof fixture: a deliberately slow interactive rc (BOS-976)\nsleep 45\n"

// slowAgentProbeShell is the login shell the slow-agent-probe preset pins. It
// must be bash: loginshell.CommandLine only sources $HOME/.bashrc for bash, and
// sourcing that seeded rc is what makes the probe slow.
const slowAgentProbeShell = "/bin/bash"

// emptyWorld builds a world with zero entities. Used by presets that boot boss
// with no seeded domain data (login/onboarding/empty).
func emptyWorld() World { return World{} }

// Presets returns the named fixture preset registry. Adding a preset here makes
// it available via the bridge's -fixture flag with no switch edits.
//
// Behavior preserved from the pre-registry bridge:
//   - demo: full DemoWorld, acknowledged settings, plus the cloud-access default
//     env. This DefaultEnv is where the bridge's cloud-access pin now lives: the
//     Node driver sets BOSS_CLOUD_ACCESS_E2E_SEQUENCE in the BRIDGE env, but
//     BaseHarnessEnv strips it before boss is spawned, so the pre-registry demo
//     boss subprocess ran WITHOUT it and fell back to the real remote cloud
//     client (a live-network dependency). Pinning it here makes demo use the
//     deterministic e2e cloud-access fake — an intentional determinism
//     improvement (BOS-217 Presets decision), not a byte-for-byte no-op.
//   - login: no seeded world, acknowledged settings (matches old behavior where
//     only "demo" seeded a world).
//   - onboarding: no seeded world, first-run settings.
//
// New presets:
//   - empty: zero entities, acknowledged settings.
//   - busy: every meaningful session state on the home board (truncation demo).
func Presets() map[string]Preset {
	return map[string]Preset{
		"demo": {
			World:      DemoWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		"login": {
			World:      emptyWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{},
		},
		"onboarding": {
			World:      emptyWorld,
			SeedKind:   SeedFirstRun,
			DefaultEnv: map[string]string{},
		},
		"empty": {
			World:      emptyWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// async-create: the demo world plus a scripted CreateSession stream that
		// reproduces the BOS-720 daemon contract — SessionCreated (accepted, the
		// row exists but the bootstrap is still running), setup output, then
		// SessionCreated again (settled). The 2s spacing is what makes the
		// intermediate accepted frame capturable rather than a flicker.
		"async-create": {
			World:      AsyncCreateWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// resurrect-progress: the demo world plus a scripted ResurrectSession
		// stream (BOS-984). Before that ticket a restore was a silent spinner
		// until the client deadline fired; this preset stages the progress the
		// streaming RPC now emits so a scenario can assert it.
		"resurrect-progress": {
			World:      ResurrectProgressWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// accounts-superseded: the demo world plus a fourth codex account whose
		// stored refresh chain has been superseded by an ambient `codex login`
		// (BOS-1175). Separate from demo rather than folded into it because demo
		// is the shared baseline every other accounts scenario captures against,
		// and an extra eligible codex account would change what those captures
		// show — including the no-eligible-codex-account hint `boss account ls`
		// renders from the demo world. Carries the same cloud-access e2e pin as
		// demo so the settings screens stay identical.
		"accounts-superseded": {
			World:      SupersededCredentialWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// repo-organization: the demo board with a git origin URL on every repo,
		// for the BOS-1061 repo-settings organization proof. Separate from demo
		// rather than folded into it because an origin URL also switches on the
		// GitHub App status line, and demo is the shared baseline every other
		// scenario captures against. Carries the same cloud-access e2e pin as
		// demo so boss lands on the home session list; the organizations
		// themselves come from the scenario's BOSS_ORG_E2E_* env.
		"repo-organization": {
			World:      RepoOrganizationWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		"busy": {
			World:      BusyWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{},
		},
		// archive-signal: two merged sessions in an archive-after-merge repo that
		// differ only in the daemon-driven archive_pending signal, for the BOS-425
		// chatpicker "Archiving…" proof scenarios. Carries the same cloud-access
		// e2e pin as demo so the boss subprocess uses the deterministic cloud-access
		// fake and lands on the home session list (without it boss falls back to the
		// real remote cloud client and renders a different landing screen).
		"archive-signal": {
			World:      ArchiveSignalWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// rotation-history: one active session carrying rotation history, for the
		// BOS-432 chat-picker proof scenario that shows the rotation block moved to
		// the very bottom of the view (below the [esc] back action bar) with the
		// full BOS-409 stale-port detail string and an ISO date prefix. Carries the
		// same cloud-access e2e pin as demo so boss lands on the home session list.
		"rotation-history": {
			World:      RotationHistoryWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// respawn-history: one active session whose rotation history carries the
		// two BOS-482 respawn-in-place outcomes, for the chat-picker proof scenario
		// that shows both RESPAWNED_SAME_ACCOUNT label shapes ("refreshed auth in
		// place" and "…on <account>") plus the newest RESPAWN_CAP_EXHAUSTED event
		// ("auth-wedge respawn cap reached"), which also fires the home-list
		// needs-attention hint. Carries the same cloud-access e2e pin as demo so
		// boss lands on the home session list.
		"respawn-history": {
			World:      RespawnHistoryWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// question-row: one active session with a newer `working` chat and an
		// older `? question` chat plus heartbeat statuses, for the BOS-494
		// chat-picker proof scenario. Opening the session must land the initial
		// cursor on the older question chat (waiting on the user) rather than the
		// newer working chat. Carries the same cloud-access e2e pin as demo so
		// boss lands on the home session list.
		"question-row": {
			World:      QuestionRowWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// waiting-callback: one session parked on an armed GitHub callback next to
		// one genuinely working session, for the BOS-668 proof scenario. The home
		// STATUS column must read "waiting" (INFO, with spinner) with the reason
		// "awaiting checks_passed_ready on acme/my-app#668" — the reason alone,
		// with no "waiting" label repeated from the STATUS cell — on its own
		// sub-row, and the chat picker must show the same reason line above a
		// "waiting" chat row — not the "stopped" badge a missing WAITING case
		// used to produce. Carries the same cloud-access e2e pin as demo so boss
		// lands on the home session list.
		"waiting-callback": {
			World:      WaitingCallbackWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// errored-status: two errored (orphaned + blocked) sessions whose live
		// chat is working, for the BOS-430 session-list proof scenario. The home
		// STATUS column must show the real "working" status recolored red (danger)
		// with a spinner, plus the red subtext hint, rather than a static
		// "orphaned"/"blocked" label. Carries the same cloud-access e2e pin as demo
		// so the boss subprocess lands on the home session list.
		"errored-status": {
			World:      ErroredStatusWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// live-past-failure: four rows for the BOS-855 proof scenario, built in
		// the errored-status mould. A live "working" row whose residual
		// "finalize failed" hint is DIMMED sits directly above a non-live "idle"
		// row carrying the SAME text at full red, so the contrast is visible in
		// one still; a third row proves the liveness-impeaching exemption (a
		// stalled agent stays bright beside the dimmed one) and a fourth proves
		// the failure was demoted, not deleted (a draft-PR failure still shows as
		// a full-intensity hint under a live "working" label). Carries the same
		// cloud-access e2e pin as demo so boss lands on the home session list.
		"live-past-failure": {
			World:      LivePastFailureWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// transient-pr-failure: two rows for the BOS-877 proof scenario, built in
		// the live-past-failure mould. Both sessions failed their background
		// draft-PR create on an SSH permission error while their chats kept
		// working — so both composites are the WORKING branch's plain SUCCESS
		// "working" (the failure is never transitioned onto the session, and the
		// live chat outranks the draft-PR branch below it) and each row's hint
		// sub-row is its only alarm. Only the first row's stderr carries a
		// transient signature (`Permission denied (publickey)`), so only its reason
		// carries the transient marker and its hint must read
		// "PR retrying — GitHub was unreachable"; the second's stderr
		// (`ssh: Permission denied`) does not, so its hint keeps the raw first
		// line, sized to render whole inside the 48-rune cap. Seeding both
		// is the point: one row alone would prove the string exists rather than
		// that the two cases render differently. Carries the same cloud-access e2e
		// pin as demo so boss lands on the home session list.
		"transient-pr-failure": {
			World:      TransientPRFailureWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// http-endpoints: one session with two machine-local HTTP listeners
		// (:3000, :5173) plus an endpoint-free neighbour, for the BOS-474 /
		// BOS-460 proof scenario. Home must show the clickable ":port" links on
		// an auxiliary row under the owning session, and the chat picker must
		// show the "HTTP" line directly above the chat table. Carries the same
		// cloud-access e2e pin as demo so boss lands on the home session list.
		"http-endpoints": {
			World:      HTTPEndpointsWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
		},
		// cloud-error: the demo board (sessions present, so the home table is
		// drawn and the status wrap width tracks it) with the cloud-access probe
		// failing with the real ~250-column refresh-token error from the BOS-507
		// report. Unlike the other presets this pins the e2e cloud sequence to
		// "error" rather than "active", so cloudGateLine renders its warning
		// branch; the message override supplies the long failure text that must
		// wrap at the table width instead of running off the terminal edge.
		"cloud-error": {
			World:    DemoWorld,
			SeedKind: SeedAcknowledged,
			DefaultEnv: map[string]string{
				"BOSS_CLOUD_ACCESS_E2E_SEQUENCE":      "error",
				"BOSS_CLOUD_ACCESS_E2E_ERROR_MESSAGE": LongCloudAccessError,
			},
		},
		// slow-agent-probe: the demo board behind a login shell that cannot answer
		// inside the agent-probe budget, for the BOS-976 preflight proof. Every
		// short-circuit ahead of the probe has to be lifted for the screen to be
		// reachable at all: the world seeds an agent inventory (ListAgents is
		// empty by default, so no agent is enabled), SettingsOverrides turns the
		// claude plugin on and pins the login shell (an empty login_shell skips
		// the check outright), and HomeFiles seeds the rc that makes that shell
		// slow. Nothing here fakes the screen — boss runs its real probe against a
		// real bash and really times out. BootWait covers the resulting startup
		// pause, which is longer than the bridge's default first-frame wait by
		// construction.
		"slow-agent-probe": {
			World:      SlowAgentProbeWorld,
			SeedKind:   SeedAcknowledged,
			DefaultEnv: map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"},
			HomeFiles:  map[string]string{".bashrc": SlowLoginShellRC},
			SettingsOverrides: map[string]any{
				"login_shell": slowAgentProbeShell,
				// enabledAgentProviders needs the plugin ENABLED in settings and
				// PRESENT in the daemon's inventory; either alone probes nothing.
				"plugins": []map[string]any{
					{"name": "claude", "path": "bossd-plugin-claude", "enabled": true},
				},
			},
			BootWait: 90 * time.Second,
		},
		// wedged-daemon: DemoWorld against a daemon the scenario can wedge on demand
		// via the set_rpc_stall daemon action (BOS-723). The DefaultEnv shrinks the
		// client's unary RPC bound for this e2e build so the bounded failure and the
		// self-recovery are both observable inside a scenario step's timeout budget;
		// the cloud-access pin matches demo so boss lands on the home session list.
		"wedged-daemon": {
			World:    DemoWorld,
			SeedKind: SeedAcknowledged,
			DefaultEnv: map[string]string{
				"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active",
				"BOSS_RPC_DEADLINE_E2E":          "3s",
			},
		},
	}
}

// LongCloudAccessError is the verbatim cloud-access failure from the BOS-507
// report: 173 columns on its own. Rendered through cloudAccessUnavailableLine it
// produces a 243-column status line — far wider than the 120-column proof
// terminal — so it is the fixture that makes the wrap visible.
const LongCloudAccessError = `refresh token: token request: Post "https://api.workos.com/user_management/authenticate": dial tcp: lookup api.workos.com: no such host (run 'boss login' to re-authenticate)`

// SlowAgentProbeWorld builds the BOS-976 preflight dataset: the full demo board
// plus an agent inventory. The board matters even though the proof never shows
// it — it is what boss WOULD render if the probe returned in time, so a capture
// of the timeout screen is a capture of that screen replacing a working one
// rather than of a boss that had nothing to draw.
func SlowAgentProbeWorld() World {
	w := DemoWorld()
	w.Agents = []*pb.AgentInfo{{Name: "claude", Version: "1.0.0"}}
	return w
}

// PresetNames returns the registry's preset names sorted alphabetically. Used
// for the -fixture flag usage string and the unknown-name error message.
func PresetNames() []string {
	presets := Presets()
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LookupPreset resolves a preset by name. On an unknown name it returns an error
// listing every valid preset (sorted) so a boot failure is self-explanatory.
func LookupPreset(name string) (Preset, error) {
	if p, ok := Presets()[name]; ok {
		return p, nil
	}
	return Preset{}, fmt.Errorf("unknown fixture preset %q (valid: %s)", name, strings.Join(PresetNames(), ", "))
}

// BusyWorld builds a deterministic "busy board" dataset: one active session for
// every meaningful (non-UNSPECIFIED) pb.SessionState so the home screen's STATUS
// column exercises every value, including a >60-char title for the truncation
// demo, across multiple repos. Repos/chats/crons reuse the demo helpers so the
// pickers and secondary screens are also populated. Built from pb types and the
// ts()/fixedNow clock only, so it is byte-stable across runs.
func BusyWorld() World {
	return World{
		Repos:    Repos(),
		Sessions: busySessions(),
		Chats:    Chats(),
		CronJobs: CronJobs(),
	}
}

// busySessions returns one session per meaningful SessionState (1..ORPHANED),
// spread across the demo repos, with deterministic ids/titles/timestamps. The
// FINALIZING entry carries an intentionally long title to demonstrate STATUS
// column truncation.
func busySessions() []*pb.Session {
	type spec struct {
		id    string
		repo  int // index into repoNames below
		title string
		state pb.SessionState
		pr    *int32
	}
	repoIDs := []string{"repo-1", "repo-2", "repo-3", "repo-4", "repo-5"}
	repoNames := []string{"my-app", "my-api", "my-web", "mobile-app", "design-system"}

	specs := []spec{
		{"sess-busy-01", 0, "Provision new worktree", pb.SessionState_SESSION_STATE_CREATING_WORKTREE, nil},
		{"sess-busy-02", 1, "Boot the coding agent", pb.SessionState_SESSION_STATE_STARTING_AGENT, nil},
		{"sess-busy-03", 2, "Push feature branch", pb.SessionState_SESSION_STATE_PUSHING_BRANCH, nil},
		{"sess-busy-04", 3, "Open the draft PR", pb.SessionState_SESSION_STATE_OPENING_DRAFT_PR, i32(701)},
		{"sess-busy-05", 4, "Implement the plan", pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, i32(702)},
		{"sess-busy-06", 0, "Wait for CI checks", pb.SessionState_SESSION_STATE_AWAITING_CHECKS, i32(703)},
		{"sess-busy-07", 1, "Fix the failing checks", pb.SessionState_SESSION_STATE_FIXING_CHECKS, i32(704)},
		{"sess-busy-08", 2, "Draft is green", pb.SessionState_SESSION_STATE_GREEN_DRAFT, i32(705)},
		{"sess-busy-09", 3, "Ready for review", pb.SessionState_SESSION_STATE_READY_FOR_REVIEW, i32(706)},
		{"sess-busy-10", 4, "Blocked on merge conflict", pb.SessionState_SESSION_STATE_BLOCKED, i32(707)},
		{"sess-busy-11", 0, "Merged and shipped", pb.SessionState_SESSION_STATE_MERGED, i32(708)},
		{"sess-busy-12", 1, "Closed without merging", pb.SessionState_SESSION_STATE_CLOSED, i32(709)},
		{
			"sess-busy-13", 2,
			"Finalize the sweeping refactor of the authentication and authorization subsystem",
			pb.SessionState_SESSION_STATE_FINALIZING, i32(710),
		},
		{"sess-busy-14", 3, "Orphaned by a daemon restart", pb.SessionState_SESSION_STATE_ORPHANED, nil},
	}

	sessions := make([]*pb.Session, 0, len(specs))
	for idx, s := range specs {
		sessions = append(sessions, &pb.Session{
			Id:              s.id,
			RepoId:          repoIDs[s.repo],
			RepoDisplayName: repoNames[s.repo],
			Title:           s.title,
			BranchName:      "boss/" + s.id,
			State:           s.state,
			PrNumber:        s.pr,
			// Stagger CreatedAt deterministically so the list order is stable.
			CreatedAt: ts(time.Duration(-(idx + 1)) * time.Hour),
		})
	}
	return sessions
}

// repoOrganizationOrigins pins a git origin URL per demo repo id for
// RepoOrganizationWorld. Kept beside the world it feeds so the two cannot drift.
var repoOrganizationOrigins = map[string]string{
	"repo-1": "https://github.com/acme/my-app",
	"repo-2": "https://github.com/acme/my-api",
	"repo-3": "https://github.com/acme/my-web",
	"repo-4": "https://github.com/acme/mobile-app",
	"repo-5": "https://github.com/acme/design-system",
}

// RepoOrganizationWorld is the demo board with a git origin URL on every repo,
// for the BOS-1061 repo-settings organization proof.
//
// The origins are the whole point. A repo-organization mapping is keyed by repo
// origin URL, so the repo settings view hides the organization row entirely for
// a repo that has none — there is no key to look the mapping up by, and a picker
// holding only the Personal row it already displays is the empty picker the
// field is shaped to avoid. The demo repos carry no OriginUrl at all, so against
// demo the field would simply not be on screen. Seeding real origins is what
// makes the row exist and ListOrganizations/GetRepoOrganization run.
func RepoOrganizationWorld() World {
	w := DemoWorld()
	for _, repo := range w.Repos {
		if origin, ok := repoOrganizationOrigins[repo.GetId()]; ok {
			repo.OriginUrl = origin
		}
	}
	return w
}
