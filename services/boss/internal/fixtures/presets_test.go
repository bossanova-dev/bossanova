package fixtures

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// allPresetNames is the exact, sorted set the registry must expose.
var allPresetNames = []string{"archive-signal", "busy", "cloud-error", "demo", "empty", "errored-status", "http-endpoints", "login", "onboarding", "question-row", "respawn-history", "rotation-history", "waiting-callback", "wedged-daemon"}

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
