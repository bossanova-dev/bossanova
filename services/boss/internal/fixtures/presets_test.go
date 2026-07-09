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
var allPresetNames = []string{"busy", "demo", "empty", "login", "onboarding"}

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
