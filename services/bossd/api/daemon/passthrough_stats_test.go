package server

import (
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

// --- RepairDoctor informational check ---

type fakePTProvider struct {
	snap []domain.PassthroughSessionSnapshot
}

func (f fakePTProvider) PassthroughStatsSnapshot() []domain.PassthroughSessionSnapshot { return f.snap }

func TestPassthroughStatsCheck_alwaysOk(t *testing.T) {
	// nil provider (proxy unwired) → Ok, names the disabled state.
	c := passthroughStatsCheck(nil)
	if !c.Ok || !strings.Contains(c.Detail, "not wired") {
		t.Fatalf("nil provider check = %+v, want Ok + 'not wired'", c)
	}
	// empty snapshot → Ok, clean detail.
	c = passthroughStatsCheck(fakePTProvider{})
	if !c.Ok || !strings.Contains(c.Detail, "no upstream errors") {
		t.Fatalf("empty check = %+v, want Ok + clean detail", c)
	}
	// populated → STILL Ok (informational only), detail summarizes.
	c = passthroughStatsCheck(fakePTProvider{snap: []domain.PassthroughSessionSnapshot{
		{DisplayID: "sess-a", Total: 5, Classes: []domain.PassthroughClassCount{{Class: "500", Count: 5}}},
	}})
	if !c.Ok {
		t.Fatalf("a populated tally must NOT redden the doctor: %+v", c)
	}
	if !strings.Contains(c.Detail, "500=5") {
		t.Fatalf("detail must summarize the classes: %q", c.Detail)
	}
}

type fakeFixedPortProvider struct {
	lost             bool
	configured, live int
}

func (f fakeFixedPortProvider) FixedPortLost() bool { return f.lost }
func (f fakeFixedPortProvider) ConfiguredPort() int { return f.configured }
func (f fakeFixedPortProvider) Port() int           { return f.live }

func TestFixedPortCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider fixedPortStatusProvider
		ok       bool
		details  []string
	}{
		{"unwired", nil, true, []string{"not wired"}},
		{"ephemeral", fakeFixedPortProvider{live: 51234}, true, []string{"configured ephemeral", "changes on restart"}},
		{"fixed", fakeFixedPortProvider{configured: 44127, live: 44127}, true, []string{"bound fixed port 44127"}},
		{"lost", fakeFixedPortProvider{lost: true, configured: 44127, live: 51234}, false,
			[]string{"44127", "51234", "another listener", "startup sweep", "next restart", "lsof -nP -iTCP:44127 -sTCP:LISTEN", "restart bossd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := fixedPortCheck(tc.provider)
			if check.Name != "failover proxy fixed port" || check.Ok != tc.ok {
				t.Fatalf("check = %+v, want name and Ok=%v", check, tc.ok)
			}
			for _, detail := range tc.details {
				if !strings.Contains(check.Detail, detail) {
					t.Errorf("detail %q must contain %q", check.Detail, detail)
				}
			}
		})
	}
}
