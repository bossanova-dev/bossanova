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
}

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
			DefaultEnv: map[string]string{},
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
	}
}

// LongCloudAccessError is the verbatim cloud-access failure from the BOS-507
// report: 173 columns on its own. Rendered through cloudAccessUnavailableLine it
// produces a 243-column status line — far wider than the 120-column proof
// terminal — so it is the fixture that makes the wrap visible.
const LongCloudAccessError = `refresh token: token request: Post "https://api.workos.com/user_management/authenticate": dial tcp: lookup api.workos.com: no such host (run 'boss login' to re-authenticate)`

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
