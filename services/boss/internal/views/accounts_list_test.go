package views

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// accountsStub is a minimal BossClient fake for the accounts list view. It
// embeds the interface (nil) so only the two methods the view uses are safe to
// call; any other call panics, which keeps the fake honest about the view's
// dependencies.
type accountsStub struct {
	client.BossClient

	accounts     []*pb.Account
	listErr      error
	testResp     *pb.TestAccountResponse
	testErr      error
	listCalls    int
	refreshCalls int
	testCalls    int
	testedID     string

	// BOS-268: disable/enable + remove + bound-session RPC recording.
	sessions      []*pb.Session
	sessionsErr   error
	sessionsCalls int
	updateReqs    []*pb.UpdateAccountRequest
	updateErr     error
	removeIDs     []string
	removeErr     error
}

func (s *accountsStub) ListAccounts(_ context.Context, _ string, refresh bool) ([]*pb.Account, error) {
	s.listCalls++
	if refresh {
		s.refreshCalls++
	}
	return s.accounts, s.listErr
}

func (s *accountsStub) TestAccount(_ context.Context, id string) (*pb.TestAccountResponse, error) {
	s.testCalls++
	s.testedID = id
	return s.testResp, s.testErr
}

func (s *accountsStub) ListSessions(_ context.Context, _ *pb.ListSessionsRequest, _ client.SessionReadOptions) ([]*pb.Session, error) {
	s.sessionsCalls++
	return s.sessions, s.sessionsErr
}

func (s *accountsStub) UpdateAccount(_ context.Context, req *pb.UpdateAccountRequest) (*pb.Account, error) {
	s.updateReqs = append(s.updateReqs, req)
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	return &pb.Account{Id: req.GetId(), Status: req.GetStatus()}, nil
}

func (s *accountsStub) RemoveAccount(_ context.Context, id string) error {
	s.removeIDs = append(s.removeIDs, id)
	return s.removeErr
}

// seedAccountsList returns a loaded AccountsListModel with the stub's accounts.
func seedAccountsList(t *testing.T, stub *accountsStub) AccountsListModel {
	t.Helper()
	m := NewAccountsListModel(stub, context.Background())
	m.height = 24
	m.width = 100
	updated, _ := m.Update(accountsLoadedMsg{accounts: stub.accounts})
	return updated.(AccountsListModel)
}

func accountsListFixture() []*pb.Account {
	return []*pb.Account{
		{
			Id:       "acc-claude",
			Provider: "claude",
			Label:    "Claude Prod",
			Status:   "active",
			Health:   "ok",
		},
		{
			Id:            "acc-codex",
			Provider:      "codex",
			Label:         "Codex CI",
			Status:        "disabled",
			Health:        "failed",
			CooldownUntil: timestamppb.New(time.Now().Add(90 * time.Minute)),
			LastTestError: "invalid key",
		},
	}
}

func TestAccountsList_LoadedState_RendersRows(t *testing.T) {
	stub := &accountsStub{accounts: accountsListFixture()}
	m := seedAccountsList(t, stub)

	content := m.View().Content
	for _, want := range []string{
		"LABEL", "PROVIDER", "STATUS", "HEALTH", "UTIL5H", "UTIL7D", "COOLDOWN", "LAST TEST",
		"Claude Prod", "Codex CI",
		"claude", "codex",
		"ok", "failed",
		"invalid key",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("loaded view missing %q\n%s", want, content)
		}
	}
	// The cooling account's COOLDOWN cell shows a relative future time.
	if !strings.Contains(content, "in 1h") {
		t.Fatalf("loaded view missing cooldown relative-time cell %q\n%s", "in 1h", content)
	}
}

// TestAccountsList_ActionBar_SpaceToggle locks the unified toggle label
// (BOS-392): the populated accounts action bar renders "[space] toggle" and no
// longer renders the old "[x] disable/enable" wording, matching the cron list.
func TestAccountsList_ActionBar_SpaceToggle(t *testing.T) {
	stub := &accountsStub{accounts: accountsListFixture()}
	m := seedAccountsList(t, stub)

	content := m.View().Content
	if !strings.Contains(content, "[space] toggle") {
		t.Fatalf("accounts action bar must render %q\n%s", "[space] toggle", content)
	}
	if strings.Contains(content, "[x] disable/enable") {
		t.Fatalf("accounts action bar must not render the old %q label\n%s", "[x] disable/enable", content)
	}
}

func TestAccountsList_RendersUsagePercentages(t *testing.T) {
	// Usage %/reset come straight from the persisted UsageSnapshot, mirroring
	// the CLI account list (UTIL5H / UTIL7D). 0.42 → "42%", 0.93 → "93%".
	acct := &pb.Account{
		Id:       "acc-usage",
		Provider: "claude",
		Label:    "Usage Acct",
		Status:   "active",
		Health:   "ok",
		Usage: &pb.UsageSnapshot{
			Util_5H:   0.42,
			Util_7D:   0.93,
			Reset_5H:  timestamppb.New(time.Now().Add(3 * time.Hour)),
			Reset_7D:  timestamppb.New(time.Now().Add(5 * 24 * time.Hour)),
			Status:    "active",
			FetchedAt: timestamppb.New(time.Now().Add(-2 * time.Minute)),
		},
	}
	stub := &accountsStub{accounts: []*pb.Account{acct}}
	m := seedAccountsList(t, stub)

	content := m.View().Content
	for _, want := range []string{"UTIL5H", "UTIL7D", "42%", "93%"} {
		if !strings.Contains(content, want) {
			t.Fatalf("accounts list missing usage token %q\n%s", want, content)
		}
	}
}

func TestAccountsList_RendersUsageAgeColumn(t *testing.T) {
	// The list carries an AGE column: a compact freshness age since the usage
	// snapshot's fetched_at for a probed account, and an em dash for a
	// never-probed one (nil UsageSnapshot).
	probed := &pb.Account{
		Id:       "acc-probed",
		Provider: "claude",
		Label:    "Probed",
		Status:   "active",
		Health:   "ok",
		Usage: &pb.UsageSnapshot{
			Util_5H:   0.42,
			Util_7D:   0.93,
			Status:    "active",
			FetchedAt: timestamppb.New(time.Now().Add(-4 * time.Minute)),
		},
	}
	neverProbed := &pb.Account{Id: "acc-none", Provider: "codex", Label: "No Usage", Status: "active", Health: "ok"}
	stub := &accountsStub{accounts: []*pb.Account{probed, neverProbed}}
	m := seedAccountsList(t, stub)

	content := m.View().Content
	if !strings.Contains(content, "AGE") {
		t.Fatalf("accounts list missing AGE column header\n%s", content)
	}
	// The probed row shows a real compact age token.
	if !strings.Contains(content, "4m") {
		t.Fatalf("probed account missing usage age token %q\n%s", "4m", content)
	}
	// The never-probed row shows an em dash (U+2014), not a fabricated age.
	if !strings.Contains(content, "—") {
		t.Fatalf("never-probed account missing em-dash age cell\n%s", content)
	}
}

func TestAccountsList_UsageDashWhenUnprobed(t *testing.T) {
	// No UsageSnapshot (never probed) → the util cells render an em dash, not a
	// fabricated 0%.
	acct := &pb.Account{Id: "acc-x", Provider: "claude", Label: "No Usage", Status: "active", Health: "ok"}
	stub := &accountsStub{accounts: []*pb.Account{acct}}
	m := seedAccountsList(t, stub)

	content := m.View().Content
	if strings.Contains(content, "0%") {
		t.Fatalf("un-probed account must not render a fabricated 0%%\n%s", content)
	}
}

func TestAccountsList_EmptyState_ExactSentence(t *testing.T) {
	stub := &accountsStub{}
	m := seedAccountsList(t, stub)

	content := m.View().Content
	if !strings.Contains(content, emptyAccountsMessage) {
		t.Fatalf("empty view missing exact sentence %q\n%s", emptyAccountsMessage, content)
	}
}

func TestAccountsList_LoadingAffordance(t *testing.T) {
	stub := &accountsStub{accounts: accountsListFixture()}
	m := NewAccountsListModel(stub, context.Background())

	// Before the first accountsLoadedMsg, the view shows a loading affordance.
	content := m.View().Content
	if !strings.Contains(content, "Loading accounts") {
		t.Fatalf("initial view missing loading affordance\n%s", content)
	}
}

func TestAccountsList_ErrorState(t *testing.T) {
	stub := &accountsStub{listErr: errors.New("boom")}
	m := NewAccountsListModel(stub, context.Background())
	updated, _ := m.Update(accountsLoadedMsg{err: stub.listErr})
	m = updated.(AccountsListModel)

	content := m.View().Content
	if !strings.Contains(content, "boom") {
		t.Fatalf("error view missing error text\n%s", content)
	}
	if !strings.Contains(content, "[esc] back") {
		t.Fatalf("error view missing [esc] back action bar\n%s", content)
	}
}

func TestAccountsList_Add_OpensRegister(t *testing.T) {
	// `a` launches the native add-account flow (BOS-267): it emits a view switch
	// to ViewAccountRegister (returnView ViewAccounts) and mutates nothing.
	stub := &accountsStub{accounts: accountsListFixture()}
	m := seedAccountsList(t, stub)

	updated, cmd := m.Update(keyPress('a'))
	am := updated.(AccountsListModel)

	if am.Cancelled() {
		t.Fatal("add must not cancel the view")
	}
	if len(am.accounts) != 2 {
		t.Fatalf("add mutated accounts: got %d", len(am.accounts))
	}
	if cmd == nil {
		t.Fatal("add must emit a view-switch command")
	}
	msg, ok := cmd().(switchViewMsg)
	if !ok {
		t.Fatalf("add did not emit switchViewMsg, got %T", cmd())
	}
	if msg.view != ViewAccountRegister {
		t.Fatalf("switch target = %v, want ViewAccountRegister", msg.view)
	}
	if msg.returnView != ViewAccounts {
		t.Fatalf("returnView = %v, want ViewAccounts", msg.returnView)
	}
}

func TestAccountsList_EditKeys_OpenAccountEdit(t *testing.T) {
	// e and enter are combined into a single edit action (there is no separate
	// detail screen); both must open the edit form for the selected account.
	for _, tc := range []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{"enter", tea.KeyPressMsg{Code: tea.KeyEnter}},
		{"e", keyPress('e')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &accountsStub{accounts: accountsListFixture()}
			m := seedAccountsList(t, stub)

			updated, cmd := m.Update(tc.msg)
			am := updated.(AccountsListModel)
			if am.Cancelled() {
				t.Fatalf("%s must not cancel the list", tc.name)
			}
			if cmd == nil {
				t.Fatalf("%s must emit a view-switch command", tc.name)
			}
			msg, ok := cmd().(switchViewMsg)
			if !ok {
				t.Fatalf("%s did not emit switchViewMsg, got %T", tc.name, cmd())
			}
			if msg.view != ViewAccountEdit {
				t.Fatalf("switch target = %v, want ViewAccountEdit", msg.view)
			}
			if msg.returnView != ViewAccounts {
				t.Fatalf("returnView = %v, want ViewAccounts", msg.returnView)
			}
			if msg.account == nil || msg.account.GetId() != am.accounts[0].GetId() {
				t.Fatalf("switch must carry the selected account, got %+v", msg.account)
			}
		})
	}
}

func TestAccountsList_Esc_Cancels(t *testing.T) {
	stub := &accountsStub{accounts: accountsListFixture()}
	m := seedAccountsList(t, stub)

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !updated.(AccountsListModel).Cancelled() {
		t.Fatal("esc must set Cancelled()")
	}
}

func TestAccountsList_Refresh_ReprobesUsage(t *testing.T) {
	// 'r' triggers a usage re-probe: ListAccounts is called with refresh=true
	// (distinct from [t]est, which calls TestAccount). The list shows a
	// refreshing affordance until the reload completes, then a done status.
	stub := &accountsStub{accounts: accountsListFixture()}
	m := seedAccountsList(t, stub)
	before := stub.refreshCalls

	updated, cmd := m.Update(keyPress('r'))
	am := updated.(AccountsListModel)
	if !am.refreshing {
		t.Fatal("r must enter the refreshing state")
	}
	if !strings.Contains(am.View().Content, "Refreshing usage") {
		t.Fatalf("refreshing affordance not rendered\n%s", am.View().Content)
	}
	assertPrecededByBlankLine(t, am.View().Content, "Refreshing usage")
	if cmd == nil {
		t.Fatal("r must emit a refresh command")
	}

	// Run the emitted command(s); the refresh cmd must re-list with refresh=true.
	if msg := runCmd(cmd); msg != nil {
		updated, _ = am.Update(msg)
		am = updated.(AccountsListModel)
	}
	if stub.refreshCalls != before+1 {
		t.Fatalf("refresh must call ListAccounts(refresh=true) exactly once, got %d", stub.refreshCalls-before)
	}
	if am.refreshing {
		t.Fatal("refreshing must clear once the reload completes")
	}
	if !strings.Contains(am.View().Content, "Usage refreshed") {
		t.Fatalf("completed refresh must show a done status\n%s", am.View().Content)
	}
	assertPrecededByBlankLine(t, am.View().Content, "Usage refreshed")
}

// assertPrecededByBlankLine fails if the first line of content containing
// substr is not immediately preceded by a blank (whitespace-only) line. The
// status messages are rendered with lipgloss Padding(0, 2), so the message
// line itself carries leading spaces; the separator line above it must be
// genuinely empty once trimmed.
func assertPrecededByBlankLine(t *testing.T, content, substr string) {
	t.Helper()
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if !strings.Contains(line, substr) {
			continue
		}
		if i == 0 {
			t.Fatalf("line containing %q is the first line; expected a blank line above it\n%s", substr, content)
		}
		if strings.TrimSpace(lines[i-1]) != "" {
			t.Fatalf("line containing %q is not preceded by a blank line (prior line: %q)\n%s", substr, lines[i-1], content)
		}
		return
	}
	t.Fatalf("no line containing %q found\n%s", substr, content)
}

func TestAccountsList_Test_LivePath(t *testing.T) {
	stub := &accountsStub{
		accounts: accountsListFixture(),
		testResp: &pb.TestAccountResponse{Detail: "login OK"},
	}
	m := seedAccountsList(t, stub)

	// 't' marks the highlighted row as testing and fires TestAccount live.
	updated, cmd := m.Update(keyPress('t'))
	am := updated.(AccountsListModel)
	if !am.testing["acc-claude"] {
		t.Fatal("t must mark the highlighted account as testing")
	}
	if !strings.Contains(am.View().Content, "testing") {
		t.Fatalf("testing row affordance not rendered\n%s", am.View().Content)
	}

	msg := runCmd(cmd)
	tested, ok := msg.(accountTestedMsg)
	if !ok {
		t.Fatalf("t command produced %T, want accountTestedMsg", msg)
	}
	if stub.testCalls != 1 || stub.testedID != "acc-claude" {
		t.Fatalf("TestAccount not called live: calls=%d id=%q", stub.testCalls, stub.testedID)
	}

	// On accountTestedMsg the row clears, the detail shows, and accounts refetch.
	priorList := stub.listCalls
	updated, cmd = am.Update(tested)
	am = updated.(AccountsListModel)
	if am.testing["acc-claude"] {
		t.Fatal("accountTestedMsg must clear the testing flag")
	}
	if !strings.Contains(am.View().Content, "login OK") {
		t.Fatalf("test result detail not shown\n%s", am.View().Content)
	}
	refetch := runCmd(cmd)
	if _, ok := refetch.(accountsLoadedMsg); !ok {
		t.Fatalf("accountTestedMsg must trigger a re-fetch, got %T", refetch)
	}
	if stub.listCalls <= priorList {
		t.Fatalf("re-fetch did not call ListAccounts again: before=%d after=%d", priorList, stub.listCalls)
	}
}

// TestAccountsList_Test_MasksSecretDetail proves the live-test transient status
// line masks a secret-bearing daemon Detail. On a failed live smoke test the
// daemon returns TestAccountResponse.Detail byte-identical to last_test_error
// (recordAndRespond → err.Error()), so a secret shape in Detail must be
// redacted before it reaches the status line — not just the LAST TEST column.
func TestAccountsList_Test_MasksSecretDetail(t *testing.T) {
	const secret = "sk-ant-api03-FAKESECRET1234567890abcdef"
	const daemonErr = "auth failed: api_key=" + secret
	stub := &accountsStub{
		accounts: accountsListFixture(),
		testResp: &pb.TestAccountResponse{Detail: daemonErr},
	}
	m := seedAccountsList(t, stub)

	updated, cmd := m.Update(keyPress('t'))
	am := updated.(AccountsListModel)
	tested := runCmd(cmd).(accountTestedMsg)
	updated, _ = am.Update(tested)
	content := updated.(AccountsListModel).View().Content

	if strings.Contains(content, secret) {
		t.Fatalf("live-test status line leaked the raw secret:\n%s", content)
	}
	if !strings.Contains(content, "[REDACTED]") {
		t.Fatalf("live-test status line not masked (no redaction sentinel):\n%s", content)
	}
}

// TestAccountsList_CredentialSafety documents that the list view can never leak
// a secret: the Account proto carries metadata only ("NO credential field.
// Ever." — models.proto), so no secret-shaped field is ever rendered.
func TestAccountsList_CredentialSafety(t *testing.T) {
	stub := &accountsStub{accounts: accountsListFixture()}
	m := seedAccountsList(t, stub)

	content := strings.ToLower(m.View().Content)
	for _, forbidden := range []string{"credential", "password", "sk-ant", "secret", "bearer "} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("rendered accounts view leaked secret-shaped token %q\n%s", forbidden, content)
		}
	}
}
