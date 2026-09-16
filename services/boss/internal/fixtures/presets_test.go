package fixtures

import (
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	// Test-only import. The fixtures package itself stays on pb types (see the
	// SeedKind comment); this is the guard below reading the real probe budget
	// instead of duplicating the literal.
	"github.com/recurser/boss/internal/preflight"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// allPresetNames is the exact, sorted set the registry must expose.
var allPresetNames = []string{"accounts-superseded", "archive-signal", "async-create", "boxed-approval", "busy", "cloud-error", "demo", "empty", "errored-status", "http-endpoints", "live-past-failure", "login", "onboarding", "question-row", "repo-organization", "respawn-history", "resurrect-progress", "rotation-history", "setup-progress", "slow-agent-probe", "transient-pr-failure", "waiting-callback", "wedged-daemon"}

func TestPresetsExactSet(t *testing.T) {
	got := make([]string, 0, len(Presets()))
	for name := range Presets() {
		got = append(got, name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, allPresetNames) {
		t.Fatalf("preset names = %v, want %v", got, allPresetNames)
	}
}

func TestPresetsDeclareSeedAndEnv(t *testing.T) {
	for name, p := range Presets() {
		if p.World == nil {
			t.Errorf("preset %q: nil World builder", name)
		}
		if p.DefaultEnv == nil {
			t.Errorf("preset %q: DefaultEnv must be non-nil (empty map ok)", name)
		}
		if p.SeedKind != SeedAcknowledged && p.SeedKind != SeedFirstRun {
			t.Errorf("preset %q: invalid SeedKind %v", name, p.SeedKind)
		}
	}
}

func TestPresetWorldsDeterministic(t *testing.T) {
	for name, p := range Presets() {
		w1, w2 := p.World(), p.World()
		if len(w1.Repos) != len(w2.Repos) || len(w1.Sessions) != len(w2.Sessions) ||
			len(w1.Chats) != len(w2.Chats) || len(w1.CronJobs) != len(w2.CronJobs) {
			t.Fatalf("preset %q: world lengths differ across two calls", name)
		}
		for i := range w1.Repos {
			if !proto.Equal(w1.Repos[i], w2.Repos[i]) {
				t.Errorf("preset %q: repo[%d] differs across calls", name, i)
			}
		}
		for i := range w1.Sessions {
			if !proto.Equal(w1.Sessions[i], w2.Sessions[i]) {
				t.Errorf("preset %q: session[%d] differs across calls", name, i)
			}
		}
		for i := range w1.Chats {
			if !proto.Equal(w1.Chats[i], w2.Chats[i]) {
				t.Errorf("preset %q: chat[%d] differs across calls", name, i)
			}
		}
		for i := range w1.CronJobs {
			if !proto.Equal(w1.CronJobs[i], w2.CronJobs[i]) {
				t.Errorf("preset %q: cron[%d] differs across calls", name, i)
			}
		}
	}
}

func TestEmptyPresetHasNoEntities(t *testing.T) {
	w := Presets()["empty"].World()
	if n := len(w.Repos) + len(w.Sessions) + len(w.Chats) + len(w.CronJobs); n != 0 {
		t.Fatalf("empty preset world has %d entities, want 0: %+v", n, w)
	}
}

func TestBusyWorldCoversEveryState(t *testing.T) {
	w := Presets()["busy"].World()
	if len(w.Sessions) < 12 {
		t.Fatalf("busy world has %d sessions, want >= 12", len(w.Sessions))
	}
	seen := map[pb.SessionState]bool{}
	longTitle := false
	for _, s := range w.Sessions {
		seen[s.GetState()] = true
		if len(s.GetTitle()) > 60 {
			longTitle = true
		}
	}
	// Every meaningful (non-UNSPECIFIED) state 1..ORPHANED must be present.
	for st := pb.SessionState(1); st <= pb.SessionState_SESSION_STATE_ORPHANED; st++ {
		if !seen[st] {
			t.Errorf("busy world missing session state %v", st)
		}
	}
	if !longTitle {
		t.Errorf("busy world has no session title > 60 chars (truncation demo)")
	}
	// "multiple repos/chats/crons"
	if len(w.Repos) < 2 || len(w.Chats) < 2 || len(w.CronJobs) < 2 {
		t.Errorf("busy world wants multiple repos/chats/crons, got %d/%d/%d",
			len(w.Repos), len(w.Chats), len(w.CronJobs))
	}
}

func TestBusyWorldDirect(t *testing.T) {
	// BusyWorld is exported so scenarios/tests can build the busy dataset
	// without going through the registry.
	w := BusyWorld()
	if len(w.Sessions) < 12 {
		t.Fatalf("BusyWorld() has %d sessions, want >= 12", len(w.Sessions))
	}
}

// TestRotationHistoryWorldSeedsRotationEvents pins the BOS-432 chat-picker proof
// preset: a single session whose newest rotation event is a generic
// (UNSPECIFIED) BOS-409 stale-port notice carrying the whole message in Detail,
// plus at least one chat so the chat-picker view renders the action bar the
// rotation block now sits beneath.
func TestRotationHistoryWorldSeedsRotationEvents(t *testing.T) {
	w := Presets()["rotation-history"].World()
	if len(w.Sessions) != 1 {
		t.Fatalf("rotation-history world has %d sessions, want 1", len(w.Sessions))
	}
	if len(w.Chats) == 0 {
		t.Fatal("rotation-history world has no chats; chat-picker needs at least one")
	}
	evs := w.Sessions[0].GetRotationEvents()
	if len(evs) == 0 {
		t.Fatal("rotation-history session has no rotation events")
	}
	first := evs[0]
	if first.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED {
		t.Errorf("newest event outcome = %v, want UNSPECIFIED", first.GetOutcome())
	}
	if !strings.Contains(first.GetDetail(), "stale failover-proxy port") {
		t.Errorf("newest event detail = %q, want the BOS-409 stale-port message", first.GetDetail())
	}
}

func TestDemoDefaultEnv(t *testing.T) {
	got := Presets()["demo"].DefaultEnv
	want := map[string]string{"BOSS_CLOUD_ACCESS_E2E_SEQUENCE": "active"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("demo DefaultEnv = %v, want %v", got, want)
	}
}

func TestDemoPresetSeedsDemoWorld(t *testing.T) {
	// demo preset must preserve today's behavior: the full demo world.
	got := Presets()["demo"].World()
	want := DemoWorld()
	if len(got.Sessions) != len(want.Sessions) || len(got.Repos) != len(want.Repos) {
		t.Fatalf("demo preset world != DemoWorld(): got %d repos/%d sessions, want %d/%d",
			len(got.Repos), len(got.Sessions), len(want.Repos), len(want.Sessions))
	}
	if Presets()["demo"].SeedKind != SeedAcknowledged {
		t.Errorf("demo preset SeedKind = %v, want SeedAcknowledged", Presets()["demo"].SeedKind)
	}
}

func TestOnboardingUsesFirstRunSeed(t *testing.T) {
	if got := Presets()["onboarding"].SeedKind; got != SeedFirstRun {
		t.Fatalf("onboarding SeedKind = %v, want SeedFirstRun", got)
	}
}

func TestLookupPresetUnknownListsNames(t *testing.T) {
	_, err := LookupPreset("does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown preset")
	}
	for _, n := range allPresetNames {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("unknown-preset error missing valid name %q: %v", n, err)
		}
	}
}

func TestLookupPresetKnown(t *testing.T) {
	p, err := LookupPreset("demo")
	if err != nil {
		t.Fatalf("LookupPreset(demo): %v", err)
	}
	if p.World == nil {
		t.Fatal("LookupPreset(demo) returned zero Preset")
	}
}

func TestPresetNamesSorted(t *testing.T) {
	if got := PresetNames(); !reflect.DeepEqual(got, allPresetNames) {
		t.Fatalf("PresetNames() = %v, want %v", got, allPresetNames)
	}
}

// TestSetupProgressWorldScriptsOneBarSequence pins the frame shape the BOS-1237
// proof scenario replays: an accepted SessionCreated, a non-progress setup line,
// ten bar redraws that differ only in fill and percentage, a closing non-progress
// line, and a settled SessionCreated.
//
// The counts are load-bearing for the capture, not decoration. Ten redraws are
// what the report showed evicting every other line from the pane's ten-element
// window; the leading non-progress line is the evidence token that proves the
// eviction is gone; and the trailing line is what gives the 100% bar screen time
// before the settled frame navigates the wizard onward.
func TestSetupProgressWorldScriptsOneBarSequence(t *testing.T) {
	w := SetupProgressWorld()
	if w.CreateSessionFrameDelay <= 0 {
		t.Fatal("CreateSessionFrameDelay must be positive or no frame is capturable")
	}

	var setupTexts []string
	created := 0
	for _, f := range w.CreateSessionScript {
		if out := f.GetSetupOutput(); out != nil {
			setupTexts = append(setupTexts, out.GetText())
		}
		if f.GetSessionCreated() != nil {
			created++
		}
	}
	if created != 2 {
		t.Errorf("SessionCreated frames = %d, want 2 (accepted then settled)", created)
	}
	if len(setupTexts) != 12 {
		t.Fatalf("setup frames = %d, want 12 (one line, ten bars, one line):\n%v", len(setupTexts), setupTexts)
	}
	if setupTexts[0] != "added 25 packages in 4s" {
		t.Errorf("first setup frame = %q, want the non-progress line the pane used to evict", setupTexts[0])
	}
	if last := setupTexts[len(setupTexts)-1]; last != "done in 12.4s" {
		t.Errorf("last setup frame = %q, want the trailing non-progress line", last)
	}

	bars := setupTexts[1 : len(setupTexts)-1]
	if !strings.Contains(bars[0], " 10% of 94.7 MiB") {
		t.Errorf("first bar = %q, want the 10%% redraw", bars[0])
	}
	if !strings.Contains(bars[len(bars)-1], "100% of 94.7 MiB") {
		t.Errorf("last bar = %q, want the 100%% redraw", bars[len(bars)-1])
	}
	// A bar that soft-wraps at the scenario's capture width renders as two rows,
	// which is indistinguishable from the defect this proof is evidence against.
	// The bound is the scenario's 100-column capture minus the setup pane's
	// four-space indent (newsession_view.go renders each line with PaddingLeft(4)).
	const maxBarColumns = 100 - 4
	for _, bar := range bars {
		if got := len([]rune(bar)); got > maxBarColumns {
			t.Errorf("bar is %d columns, too wide to stay on one row at a 100-column capture: %q", got, bar)
		}
	}
}

// TestQuestionRowWorldSeedsMixedStatuses pins the BOS-494 chat-picker proof
// preset: one session with a newer working chat and an older question chat,
// each carrying a heartbeat status, so the picker can preselect the question
// row.
func TestQuestionRowWorldSeedsMixedStatuses(t *testing.T) {
	w := Presets()["question-row"].World()
	if len(w.Sessions) != 1 {
		t.Fatalf("question-row world has %d sessions, want 1", len(w.Sessions))
	}
	if len(w.Chats) != 2 {
		t.Fatalf("question-row world has %d chats, want 2", len(w.Chats))
	}
	// Statuses must include exactly one working and one question chat.
	byStatus := map[pb.ChatStatus]string{}
	for _, e := range w.ChatStatuses {
		byStatus[e.GetStatus()] = e.GetAgentSessionId()
	}
	if byStatus[pb.ChatStatus_CHAT_STATUS_WORKING] == "" {
		t.Error("question-row world has no working chat status")
	}
	qAgent := byStatus[pb.ChatStatus_CHAT_STATUS_QUESTION]
	if qAgent == "" {
		t.Fatal("question-row world has no question chat status")
	}
	// The question chat must be the older one (created before the working chat),
	// so the picker sorts it below the working chat and the fix is exercised.
	var working, question *pb.ClaudeChat
	for _, c := range w.Chats {
		switch c.GetAgentSessionId() {
		case qAgent:
			question = c
		case byStatus[pb.ChatStatus_CHAT_STATUS_WORKING]:
			working = c
		}
	}
	if working == nil || question == nil {
		t.Fatalf("question-row chats do not match the seeded statuses")
	}
	if !question.GetCreatedAt().AsTime().Before(working.GetCreatedAt().AsTime()) {
		t.Errorf("question chat must be older than the working chat so it sorts below it")
	}
}

// TestBoxedApprovalWorldSeedsQuestionStatus pins the BOS-1266 proof preset the
// way every other proof world in this package is pinned.
//
// It exists because BoxedApprovalWorld DERIVES its status: chatStatusForPane
// runs the shared detector over the captured pane and falls back to
// CHAT_STATUS_IDLE when it stops recognising the boxed approval menu. That
// degraded world is still deterministic, still well-formed and still exactly
// one session with one chat, so TestPresetsExactSet, TestPresetsDeclareSeedAndEnv
// and TestPresetWorldsDeterministic all pass identically over it. Only the proof
// scenario reddens, and the proof harness is not in `make test` or CI -- so
// without this pin the derivation can go vacuous with every gate green.
//
// DisplayLabel is asserted alongside the status because BoxedApprovalSessions
// runs the real displaystatus.Compute cascade rather than hand-writing the
// triple; pinning the label keeps the whole chain (pane -> statusdetect ->
// CHAT_STATUS_QUESTION -> displaystatus -> "? question") under a unit gate.
func TestBoxedApprovalWorldSeedsQuestionStatus(t *testing.T) {
	// Drift control for the BoxedApprovalPane copy in fixtures.go. The derived
	// status below stays true even if the capture loses its border chrome,
	// because statusdetect's Pattern 1 recognises the UNBORDERED shape too --
	// so the whole world would keep rendering "? question" while no longer
	// exercising the boxed path BOS-1266 exists to cover. The border rune
	// immediately before the highlighted selector is the discriminator.
	if !strings.Contains(BoxedApprovalPane, "│ \x1b[7m❯") {
		t.Fatal("BoxedApprovalPane lost the border rune before its selector; the boxed path is untested here")
	}
	w := Presets()["boxed-approval"].World()
	if len(w.Sessions) != 1 {
		t.Fatalf("boxed-approval world has %d sessions, want 1", len(w.Sessions))
	}
	if len(w.Chats) != 1 {
		t.Fatalf("boxed-approval world has %d chats, want 1", len(w.Chats))
	}
	if len(w.ChatStatuses) != 1 {
		t.Fatalf("boxed-approval world has %d chat statuses, want 1", len(w.ChatStatuses))
	}
	// Not merely "non-IDLE": the proof asserts the QUESTION label specifically,
	// and any other status would render a different STATUS column.
	if got := w.ChatStatuses[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("chat status = %v, want CHAT_STATUS_QUESTION; the shared detector no longer recognises the captured approval menu", got)
	}
	// Measured, not assumed: displaystatus.Compute renders the QUESTION status
	// with its glyph attached, so the reachable value is "? question" -- which is
	// also the string the proof scenario asserts on.
	if got := w.Sessions[0].GetDisplayLabel(); got != "? question" {
		t.Errorf("session DisplayLabel = %q, want \"? question\"; the derived status no longer reaches the STATUS column", got)
	}
}

// TestHTTPEndpointsWorldSeedsEndpoints pins the BOS-474 proof fixture: exactly
// one session carrying the two ports the scenario asserts on, with loopback
// HTTP URLs so the TUI renders them as clickable links.
func TestHTTPEndpointsWorldSeedsEndpoints(t *testing.T) {
	w := HTTPEndpointsWorld()
	var withEndpoints []*pb.Session
	for _, s := range w.Sessions {
		if len(s.GetHttpEndpoints()) > 0 {
			withEndpoints = append(withEndpoints, s)
		}
	}
	if len(withEndpoints) != 1 {
		t.Fatalf("sessions with endpoints = %d, want exactly 1", len(withEndpoints))
	}
	sess := withEndpoints[0]
	var ports []uint32
	for _, ep := range sess.GetHttpEndpoints() {
		ports = append(ports, ep.GetPort())
		if !strings.HasPrefix(ep.GetUrl(), "http://") {
			t.Errorf("endpoint %d url = %q, want an http:// URL so the TUI links it", ep.GetPort(), ep.GetUrl())
		}
	}
	if !reflect.DeepEqual(ports, []uint32{3000, 5173}) {
		t.Errorf("ports = %v, want [3000 5173]", ports)
	}
	if len(w.Chats) == 0 {
		t.Error("HTTPEndpointsWorld has no chats; the scenario must be able to open the session's chat picker")
	}
}

// TestCloudErrorPresetPinsLongFailure pins the BOS-507 wrap proof preset: the
// cloud-access probe must fail (sequence "error", not "active") with the long
// report failure, on a world that has sessions so the home table is drawn and
// the status wrap width has a table to track.
func TestCloudErrorPresetPinsLongFailure(t *testing.T) {
	p, err := LookupPreset("cloud-error")
	if err != nil {
		t.Fatalf("LookupPreset(cloud-error): %v", err)
	}
	if got := p.DefaultEnv["BOSS_CLOUD_ACCESS_E2E_SEQUENCE"]; got != "error" {
		t.Errorf("cloud sequence = %q, want %q", got, "error")
	}
	if got := p.DefaultEnv["BOSS_CLOUD_ACCESS_E2E_ERROR_MESSAGE"]; got != LongCloudAccessError {
		t.Errorf("error message = %q, want the long report failure", got)
	}
	// The rendered line is "Cloud access status unavailable: <err>. Local
	// sessions are still available." — wider than the 120-column proof
	// terminal, which is what makes the wrap visible.
	rendered := "Cloud access status unavailable: " + LongCloudAccessError + ". Local sessions are still available."
	if len(rendered) <= 120 {
		t.Errorf("rendered cloud error is %d columns, want > 120 so it wraps in the proof harness", len(rendered))
	}
	if len(p.World().Sessions) == 0 {
		t.Error("cloud-error world has no sessions; the home table must be drawn for the wrap width to track it")
	}
}

// TestWedgedDaemonPresetSeedsAWorld guards the premise of the BOS-723 scenario:
// the daemon-down screen REPLACES the home table, so a preset with an empty
// world would let scene 1 "pass" against a board that never had rows to lose,
// and scene 3's recovery would have nothing to repopulate.
func TestWedgedDaemonPresetSeedsAWorld(t *testing.T) {
	p, err := LookupPreset("wedged-daemon")
	if err != nil {
		t.Fatalf("LookupPreset(wedged-daemon): %v", err)
	}
	world := p.World()
	if len(world.Sessions) == 0 {
		t.Fatal("wedged-daemon seeds no sessions; the home table would be empty before the wedge")
	}
	if len(world.Repos) == 0 {
		t.Fatal("wedged-daemon seeds no repos; the seeded sessions would have no repo to render under")
	}
	// The shrunk client bound is what makes the bounded failure land inside a
	// step's timeout budget; without it the scenario is unrunnable.
	if got := p.DefaultEnv["BOSS_RPC_DEADLINE_E2E"]; got == "" {
		t.Fatal("wedged-daemon must pin BOSS_RPC_DEADLINE_E2E; the 30s production bound outlasts the schema's step cap")
	}
}

// TestSlowAgentProbePresetLiftsEveryPreflightShortCircuit guards the BOS-976
// proof preset against the failure mode that makes a preflight scenario useless:
// boss's agent probe is guarded by THREE independent short-circuits, and any one
// of them left in place produces a boss that boots straight to the home board
// while the scenario waits for a screen that will never render. Each assertion
// below names the short-circuit it lifts, so a regression says which one came
// back rather than just "the scenario timed out".
func TestSlowAgentProbePresetLiftsEveryPreflightShortCircuit(t *testing.T) {
	p, err := LookupPreset("slow-agent-probe")
	if err != nil {
		t.Fatalf("LookupPreset(slow-agent-probe): %v", err)
	}

	// 1. enabledAgentProviders intersects settings.Plugins with the daemon's
	//    ListAgents inventory. MockDaemon answers with an empty list unless a
	//    world seeds one, so without this every plugin is filtered out.
	world := p.World()
	var agentNames []string
	for _, a := range world.Agents {
		agentNames = append(agentNames, a.GetName())
	}
	if !slices.Contains(agentNames, "claude") {
		t.Errorf("world must seed the claude agent for ListAgents; got %v", agentNames)
	}

	// 2. The same intersection needs the plugin ENABLED in settings.json. The
	//    seeded acknowledged settings carry no plugins key at all.
	plugins, ok := p.SettingsOverrides["plugins"].([]map[string]any)
	if !ok {
		t.Fatalf("SettingsOverrides[plugins] = %#v, want []map[string]any", p.SettingsOverrides["plugins"])
	}
	enabledClaude := false
	for _, pl := range plugins {
		if pl["name"] == "claude" && pl["enabled"] == true {
			enabledClaude = true
		}
	}
	if !enabledClaude {
		t.Errorf("SettingsOverrides must enable the claude plugin; got %#v", plugins)
	}

	// 3. checkAgentResolvable returns nil immediately for an empty login shell,
	//    and only bash sources $HOME/.bashrc (loginshell.CommandLine), which is
	//    what the seeded rc relies on to be slow at all.
	shell, _ := p.SettingsOverrides["login_shell"].(string)
	if filepath.Base(shell) != "bash" {
		t.Errorf("login_shell = %q, want a bash so the seeded .bashrc is sourced", shell)
	}

	// The rc itself must be seeded at the path bash's prologue sources.
	rc, ok := p.HomeFiles[".bashrc"]
	if !ok {
		t.Fatalf("HomeFiles must seed .bashrc; got %v", p.HomeFiles)
	}
	if !strings.Contains(rc, "sleep") {
		t.Errorf(".bashrc must block; got %q", rc)
	}

	// The bridge's first-frame wait has to outlast a startup that is slow BY
	// DESIGN — boss paints nothing until the probe gives up, so a default
	// BootWait would fail the bridge before the screen it is capturing exists.
	if p.BootWait <= DefaultBootWait {
		t.Errorf("BootWait = %s, want more than the default %s", p.BootWait, DefaultBootWait)
	}
}

// TestSlowLoginShellRCOutlastsTheProbeBudget pins the ordering the whole proof
// depends on. The rc sleep must still be running when the probe's deadline
// fires: if it finished first, `command -v claude` would run and fail on a CI
// box with no claude, and the scenario would capture the NOT-FOUND screen while
// looking exactly as green as a correct run.
//
// The budget is READ from preflight rather than copied, because a local copy
// opens this invariant permanently the first time either number moves: raising
// preflight's constant reds its own TestAgentResolveTimeoutBudget, updating that
// literal restores green, and a stale copy here would then let `sleep 45` finish
// BEFORE the probe deadline — at which point `command -v claude` runs, fails on
// a CI box with no claude, and the scenario captures the not-found screen while
// looking exactly as green as a correct run.
//
// The import is test-only, so the fixtures package itself keeps its "pb types
// only" dependency rule (see SeedKind); there was never an import cycle to avoid
// either way, since preflight imports only bossalib.
func TestSlowLoginShellRCOutlastsTheProbeBudget(t *testing.T) {
	agentProbeBudget := preflight.AgentResolveTimeout

	m := regexp.MustCompile(`(?m)^sleep (\d+)$`).FindStringSubmatch(SlowLoginShellRC)
	if m == nil {
		t.Fatalf("SlowLoginShellRC has no `sleep <n>` line: %q", SlowLoginShellRC)
	}
	secs, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parsing sleep duration %q: %v", m[1], err)
	}
	if got := time.Duration(secs) * time.Second; got <= agentProbeBudget {
		t.Errorf("rc sleeps %s, want longer than the %s probe budget", got, agentProbeBudget)
	}
}

// TestPresetHomeFilesAreRelative pins the containment rule the bridge relies on
// when it writes HomeFiles: every key is a path INSIDE the per-run HOME, so a
// preset cannot reach the developer's real dotfiles.
func TestPresetHomeFilesAreRelative(t *testing.T) {
	for name, p := range Presets() {
		for rel := range p.HomeFiles {
			if filepath.IsAbs(rel) {
				t.Errorf("preset %q: HomeFiles key %q is absolute", name, rel)
			}
			if rel == "" || strings.Contains(rel, "..") {
				t.Errorf("preset %q: HomeFiles key %q must be a plain relative path", name, rel)
			}
		}
	}
}

// TestRepoOrganizationPresetSeedsOrigins pins the one property the BOS-1061
// organization proof depends on: every repo carries a git origin URL. The repo
// settings view keys its organization mapping by that URL and loads nothing
// without one, so a preset that lost its origins would still render a
// plausible-looking "None" screen and prove nothing.
func TestRepoOrganizationPresetSeedsOrigins(t *testing.T) {
	w := Presets()["repo-organization"].World()
	if len(w.Repos) == 0 {
		t.Fatalf("repo-organization world has no repos")
	}
	for _, repo := range w.Repos {
		if repo.GetOriginUrl() == "" {
			t.Errorf("repo %q has no origin URL; the organization field cannot load without one", repo.GetId())
		}
	}
	// The demo baseline must stay origin-free, otherwise this preset is
	// redundant and the demo captures silently gained a GitHub App status line.
	for _, repo := range DemoWorld().Repos {
		if repo.GetOriginUrl() != "" {
			t.Errorf("demo repo %q gained an origin URL = %q; that changes every demo capture", repo.GetId(), repo.GetOriginUrl())
		}
	}
}
