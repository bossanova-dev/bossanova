package fixtures_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/fixtures"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gitremote"
	"github.com/recurser/bossalib/sessionreason"
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

func TestDemoWorldSeedsClaudeEffortSetting(t *testing.T) {
	w := fixtures.DemoWorld()
	var effort *pb.UserSetting
	for _, agent := range w.Agents {
		if agent.GetName() != "claude" {
			continue
		}
		for _, setting := range agent.GetUserSettings() {
			if setting.GetKey() == "effort" {
				effort = setting
				break
			}
		}
	}
	if effort == nil {
		t.Fatal("demo world must seed claude effort setting for General Settings proof")
	}
	if effort.GetType() != pb.UserSettingType_USER_SETTING_TYPE_ENUM {
		t.Fatalf("effort setting type = %v, want ENUM", effort.GetType())
	}
	if got := effort.GetDefaultValue(); got != "high" {
		t.Fatalf("effort default = %q, want high", got)
	}
	allowed := effort.GetAllowedValues()
	if len(allowed) == 0 || allowed[0] != "" {
		t.Fatalf("effort allowed_values = %v, want empty string first", allowed)
	}
}

func TestDemoWorldDeterministic(t *testing.T) {
	// Two calls must be byte-identical so screenshots never vary run-to-run.
	a, b := fixtures.DemoWorld(), fixtures.DemoWorld()
	if a.Sessions[0].CreatedAt.AsTime() != b.Sessions[0].CreatedAt.AsTime() {
		t.Fatal("DemoWorld timestamps are not deterministic")
	}
}

// TestTransientPRFailureWorldSeedsBothForms pins the BOS-877 proof preset. The
// scenario compares a transient draft-PR failure against a terminal one in a
// single frame, so the world must seed BOTH — with one row only, the scenario
// would prove the replacement string exists rather than that the two cases
// render differently.
//
// It also pins the terminal row's raw reason short enough to survive the TUI's
// 48-rune hint cap with "Permission denied" still visible, because that is the
// scenario's scene-2 evidence token.
func TestTransientPRFailureWorldSeedsBothForms(t *testing.T) {
	w := fixtures.TransientPRFailureWorld()
	if len(w.Sessions) != 2 {
		t.Fatalf("want exactly 2 sessions (one transient, one terminal), got %d", len(w.Sessions))
	}

	transient, terminal := w.Sessions[0].BlockedReason, w.Sessions[1].BlockedReason
	if !sessionreason.IsDraftPRCreationTransientFailure(transient) {
		t.Fatalf("session[0] reason = %v, want the transient draft-PR form", transient)
	}
	if !sessionreason.IsDraftPRCreationFailure(terminal) {
		t.Fatalf("session[1] reason = %v, want a draft-PR failure", terminal)
	}
	if sessionreason.IsDraftPRCreationTransientFailure(terminal) {
		t.Fatalf("session[1] reason = %q, want the TERMINAL form so the frame shows a contrast", *terminal)
	}

	// The hint the TUI renders for the terminal row is the reason's first line cut
	// to 48 runes; "Permission denied" must still be inside that window.
	//
	// Duplicated rather than imported: the real cap is views.hintReasonMaxRunes in
	// services/boss/internal/views/status.go and it is unexported. The copy is why
	// this guard is one-directional — it catches the fixture literal growing past
	// the cap, but NOT views shrinking the cap under the literal, which would
	// truncate the token away with this test still green. The proof scenario is
	// the backstop for that direction.
	//
	// Asserted whole-fit FIRST, because it is the stronger property and it makes
	// the token check below a plain Contains: truncateHintReason appends an
	// ellipsis past the cap, so a reason that fits at all is rendered entire and
	// there is no window left to slice.
	const hintCapRunes = 48
	if got := len([]rune(*terminal)); got > hintCapRunes {
		t.Fatalf("the terminal row's reason is %d runes, over the %d-rune cap; the hint would render truncated with an ellipsis", got, hintCapRunes)
	}
	if !strings.Contains(*terminal, "Permission denied") {
		t.Fatalf("the terminal row's reason is %q; the scenario's \"Permission denied\" token would not be visible", *terminal)
	}

	// And the two literals must be ones the REAL classifier would sort this way.
	// The fixture calls the sessionreason constructors directly rather than going
	// through bossd's draftPRBlockedReason, so nothing else stops someone making
	// the terminal row "more realistic" by appending git's usual
	// "Could not read from remote repository." line — a transient signature, which
	// would seed a world the production classifier can never emit while every
	// other assertion here stayed green. Run over the whole reason on purpose: the
	// failure prefix and the transient marker carry no signature of their own, so
	// a hit can only come from the seeded stderr.
	if !gitremote.IsTransientMessage(*transient) {
		t.Errorf("the transient row's reason %q carries no gitremote transient signature; bossd would have classified this stderr as TERMINAL", *transient)
	}
	if gitremote.IsTransientMessage(*terminal) {
		t.Errorf("the terminal row's reason %q carries a gitremote transient signature; bossd would have classified this stderr as TRANSIENT", *terminal)
	}

	if len(w.Repos) == 0 {
		t.Error("the world seeds no repos; boss would not land on the home session list the scenario drives")
	}

	// One chat per session is not enough — two chats pointing at the SAME session
	// would satisfy a count check while the other row's picker still read
	// "Loading chats...". Pin the coverage instead.
	covered := map[string]bool{}
	for _, c := range w.Chats {
		covered[c.SessionId] = true
	}
	for _, s := range w.Sessions {
		if !covered[s.Id] {
			t.Errorf("session %q has no chat; its chat picker would render \"Loading chats...\" instead of a populated list", s.Id)
		}
	}
}
