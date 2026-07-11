package fixtures_test

import (
	"testing"

	"github.com/recurser/boss/internal/fixtures"
)

func TestDemoWorldShape(t *testing.T) {
	w := fixtures.DemoWorld()

	if len(w.Repos) < 2 {
		t.Fatalf("want >=2 repos for a realistic repo list, got %d", len(w.Repos))
	}

	var archived, active int
	for _, s := range w.Sessions {
		if s.ArchivedAt != nil {
			archived++
		} else {
			active++
		}
	}
	if active < 2 {
		t.Errorf("want >=2 active sessions for the home screen, got %d", active)
	}
	if archived < 1 {
		t.Errorf("want >=1 archived session so the Trash screen is non-empty, got %d", archived)
	}

	if len(w.Chats) < 2 {
		t.Errorf("want >=2 chats for the chat picker, got %d", len(w.Chats))
	}
	// Every chat must reference an existing session, else the picker renders empty.
	sessionIDs := map[string]bool{}
	for _, s := range w.Sessions {
		sessionIDs[s.Id] = true
	}
	for _, c := range w.Chats {
		if !sessionIDs[c.SessionId] {
			t.Errorf("chat %q references unknown session %q", c.Id, c.SessionId)
		}
	}

	if len(w.CronJobs) < 1 {
		t.Errorf("want >=1 cron job so the Scheduled Jobs screen is non-empty, got %d", len(w.CronJobs))
	}

	// The Settings → Accounts list (BOS-265) needs >=2 accounts covering both
	// providers so the populated-state proof shows a "codex" cell and a label.
	if len(w.Accounts) < 2 {
		t.Fatalf("want >=2 accounts for the Accounts list, got %d", len(w.Accounts))
	}
	providers := map[string]bool{}
	for _, a := range w.Accounts {
		providers[a.Provider] = true
		if a.Label == "" {
			t.Errorf("account %q has an empty label; the list uses label as the identity column", a.Id)
		}
	}
	for _, want := range []string{"claude", "codex"} {
		if !providers[want] {
			t.Errorf("want an account with provider %q for the Accounts proof, got providers %v", want, providers)
		}
	}
}

func TestDemoWorldDeterministic(t *testing.T) {
	// Two calls must be byte-identical so screenshots never vary run-to-run.
	a, b := fixtures.DemoWorld(), fixtures.DemoWorld()
	if a.Sessions[0].CreatedAt.AsTime() != b.Sessions[0].CreatedAt.AsTime() {
		t.Fatal("DemoWorld timestamps are not deterministic")
	}
}
