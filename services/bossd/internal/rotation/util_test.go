package rotation

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
)

func TestFutureWeeklyReset(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	cases := []struct {
		name    string
		reset   *time.Time
		wantOK  bool
		wantVal time.Time
	}{
		{name: "nil reset ⇒ not urgent", reset: nil, wantOK: false},
		{name: "past reset ⇒ not urgent (window rolled)", reset: &past, wantOK: false},
		{name: "exactly now ⇒ not urgent (strictly future only)", reset: &now, wantOK: false},
		{name: "future reset ⇒ urgent", reset: &future, wantOK: true, wantVal: future},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := FutureWeeklyReset(c.reset, now)
			if ok != c.wantOK {
				t.Fatalf("FutureWeeklyReset ok = %v, want %v", ok, c.wantOK)
			}
			if ok && !got.Equal(c.wantVal) {
				t.Errorf("FutureWeeklyReset t = %v, want %v", got, c.wantVal)
			}
			if !ok && !got.IsZero() {
				t.Errorf("FutureWeeklyReset t = %v, want zero when ok=false", got)
			}
		})
	}
}

func TestUtilizationCapped(t *testing.T) {
	cases := []struct {
		util float64
		want bool
	}{
		{0, false},
		{0.99, false},
		{1.0, true},
		{1.5, true},
	}
	for _, c := range cases {
		if got := UtilizationCapped(c.util); got != c.want {
			t.Errorf("UtilizationCapped(%v) = %v, want %v", c.util, got, c.want)
		}
	}
	if UtilizationCapThreshold != 1.0 {
		t.Errorf("UtilizationCapThreshold = %v, want 1.0", UtilizationCapThreshold)
	}
}

func TestLowerUtilization(t *testing.T) {
	if !LowerUtilization(0.1, 0.2) {
		t.Error("LowerUtilization(0.1, 0.2) = false, want true")
	}
	if LowerUtilization(0.2, 0.1) {
		t.Error("LowerUtilization(0.2, 0.1) = true, want false")
	}
	if LowerUtilization(0.1, 0.1) {
		t.Error("LowerUtilization(0.1, 0.1) = true, want false (strict)")
	}
}

func TestHasRotationHeadroom(t *testing.T) {
	cases := []struct {
		name string
		util float64
		want bool
	}{
		{name: "fresh account is comfortably in band", util: 0, want: true},
		{name: "moderately used is in band", util: 0.35, want: true},
		{name: "exactly at the floor is IN band (>=, not >)", util: 0.75, want: true},
		{name: "a hair past the floor is out of band", util: 0.76, want: false},
		{name: "near-exhausted is out of band", util: 0.90, want: false},
		{name: "capped is out of band", util: 1.0, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasRotationHeadroom(c.util); got != c.want {
				t.Errorf("HasRotationHeadroom(%v) = %v, want %v", c.util, got, c.want)
			}
		})
	}
	if MinRotationHeadroom != 0.25 {
		t.Errorf("MinRotationHeadroom = %v, want 0.25", MinRotationHeadroom)
	}
}

func TestMaxUtilization(t *testing.T) {
	cases := []struct {
		u5h, u7d, want float64
	}{
		{0.4, 0.9, 0.9},
		{0.9, 0.4, 0.9},
		{0.5, 0.5, 0.5},
		{0, 0, 0},
	}
	for _, c := range cases {
		if got := MaxUtilization(c.u5h, c.u7d); got != c.want {
			t.Errorf("MaxUtilization(%v, %v) = %v, want %v", c.u5h, c.u7d, got, c.want)
		}
	}
}

func TestMoreConsumeFirst(t *testing.T) {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	soon, later, used := now.Add(48*time.Hour), now.Add(96*time.Hour), now.Add(-time.Hour)
	tests := []struct {
		name string
		a, b ConsumeFirstCandidate
	}{
		{"weekly expiry before utilization", ConsumeFirstCandidate{Fresh: true, Util7d: .6, Reset7d: &soon}, ConsumeFirstCandidate{Fresh: true, Util7d: .2, Reset7d: &later}},
		{"priority before expiry", ConsumeFirstCandidate{Priority: 0, Fresh: true, Reset7d: &later}, ConsumeFirstCandidate{Priority: 1, Fresh: true, Reset7d: &soon}},
		{"headroom before expiry", ConsumeFirstCandidate{Fresh: true, Util7d: .4, Reset7d: &later}, ConsumeFirstCandidate{Fresh: true, Util7d: .99, Reset7d: &soon}},
		{"fresh before stale", ConsumeFirstCandidate{Fresh: true}, ConsumeFirstCandidate{Reset7d: &soon}},
		{"known before unknown", ConsumeFirstCandidate{Fresh: true, Reset7d: &soon}, ConsumeFirstCandidate{Fresh: true}},
		{"out of band utilization", ConsumeFirstCandidate{Fresh: true, Util7d: .8}, ConsumeFirstCandidate{Fresh: true, Util7d: .9}},
		{"stale expiry", ConsumeFirstCandidate{Reset7d: &soon}, ConsumeFirstCandidate{Reset7d: &later}},
		{"never used first", ConsumeFirstCandidate{}, ConsumeFirstCandidate{LastUsedAt: &used}},
		{"id", ConsumeFirstCandidate{ID: "a"}, ConsumeFirstCandidate{ID: "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !MoreConsumeFirst(tt.a, tt.b, now) || MoreConsumeFirst(tt.b, tt.a, now) {
				t.Fatal("ranking reversed or tied")
			}
			if MoreConsumeFirst(tt.a, tt.a, now) {
				t.Fatal("ranking must be strict")
			}
		})
	}
}

func TestOver5hThreshold(t *testing.T) {
	now := time.Now()
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	for _, tt := range []struct {
		name string
		c    ConsumeFirstCandidate
		want bool
	}{
		{"at threshold", ConsumeFirstCandidate{Fresh: true, Util5h: .9, Reset5h: &future}, true},
		{"unknown reset", ConsumeFirstCandidate{Fresh: true, Util5h: .95}, true},
		{"reset passed", ConsumeFirstCandidate{Fresh: true, Util5h: .95, Reset5h: &past}, false},
		{"reset now", ConsumeFirstCandidate{Fresh: true, Util5h: .95, Reset5h: &now}, false},
		{"stale", ConsumeFirstCandidate{Util5h: .95, Reset5h: &future}, false},
		{"below", ConsumeFirstCandidate{Fresh: true, Util5h: .89, Reset5h: &future}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Over5hThreshold(tt.c, .9, now); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

// This parity test lives beside the comparator because db's internal tests
// cannot import rotation (the engine already imports db).
func TestConsumeFirstStaleSQLParity(t *testing.T) {
	ctx := context.Background()
	store := db.NewAgentAccountStore(dbtest.New(t))
	now := time.Now().UTC().Truncate(time.Millisecond)
	soon, later, past, used := now.Add(time.Hour), now.Add(2*time.Hour), now.Add(-time.Hour), now.Add(-2*time.Hour)
	fixtures := []struct {
		priority    int
		reset, used *time.Time
	}{
		{1, &soon, nil}, {0, nil, &used}, {0, &later, nil}, {0, &soon, &used}, {0, &soon, nil}, {0, &past, nil}, {0, nil, nil},
	}
	for i, f := range fixtures {
		a, err := store.Create(ctx, db.CreateAgentAccountParams{Provider: domain.AgentAccountProviderClaude, Label: fmt.Sprintf("parity-%d", i), Priority: f.priority})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordUsageProbe(ctx, a.ID, domain.UsageSnapshot{FetchedAt: &past, Reset7d: f.reset}); err != nil {
			t.Fatal(err)
		}
		if f.used != nil {
			at := f.used
			if _, err := store.Update(ctx, a.ID, db.UpdateAgentAccountParams{LastUsedAt: &at}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view := func(a *domain.AgentAccount) ConsumeFirstCandidate {
		c := ConsumeFirstCandidate{ID: a.ID, Priority: a.Priority, LastUsedAt: a.LastUsedAt}
		if a.Usage != nil {
			c.Reset7d = a.Usage.Reset7d
		}
		return c
	}
	slices.SortFunc(rows, func(a, b *domain.AgentAccount) int {
		if MoreConsumeFirst(view(a), view(b), now) {
			return -1
		}
		if MoreConsumeFirst(view(b), view(a), now) {
			return 1
		}
		return 0
	})
	err = store.UpdateInTransaction(ctx, "claude", func(tx db.TxAgentAccountView) error {
		sqlRows, err := tx.ListByProvider(ctx, now)
		if err != nil {
			return err
		}
		if len(sqlRows) != len(rows) {
			t.Fatalf("SQL rows %d comparator rows %d", len(sqlRows), len(rows))
		}
		for i := range rows {
			if rows[i].ID != sqlRows[i].ID {
				t.Fatalf("rank %d comparator %s SQL %s", i, rows[i].Label, sqlRows[i].Label)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestModelWindowFor(t *testing.T) {
	windows := []domain.UsageModelWindow{{ModelName: "FABLE", Util: 1}, {ModelName: "Opus", Util: .8}, {ModelName: "Sonnet", Util: .6}, {ModelName: "extra large", Util: .7}}
	for _, tc := range []struct {
		model string
		want  float64
		ok    bool
	}{
		{"claude-fable-5", 1, true}, {"opus[1m]", .8, true}, {"sonnet", .6, true}, {"haiku", 0, false}, {"", 0, false}, {"claude-fableish", 0, false}, {"x-extra-large-2", .7, true}, {"extra-new-large", 0, false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			w, ok := ModelWindowFor(windows, tc.model)
			if ok != tc.ok || w.Util != tc.want {
				t.Fatalf("got %v, %v", w, ok)
			}
		})
	}
	snap := domain.UsageSnapshot{Util5h: .4, Util7d: .5, ModelWindows: windows}
	for model, want := range map[string]float64{"claude-fable-5": 1, "opus[1m]": .8, "": .5, "haiku": .5} {
		if got := UsageUtilForModel(snap, model); got != want {
			t.Errorf("%q: got %v want %v", model, got, want)
		}
	}
}
