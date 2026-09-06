package views

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	// Wide enough for the whole column set (BOS-1142 added CHECK and CHECKED),
	// so a test asserting on a column is testing the cell rather than the
	// responsive drop order — that ordering has its own test.
	m.width = 170
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

func responsiveAccountsFixture() []*pb.Account {
	return []*pb.Account{{
		Id:            "account-1",
		Label:         strings.Repeat("l", 40),
		Provider:      strings.Repeat("p", 12),
		Status:        "disabled",
		Health:        "unhealthy",
		LastTestError: strings.Repeat("x", 22),
	}}
}

func TestAccountsListRebuildTable_FitsColumnsToTerminalWidth(t *testing.T) {
	// BOS-1142 added CHECK and CHECKED. CHECK sits at priority 4 so it is given
	// up just before HEALTH but after the pure-diagnostic columns: the health
	// cell refuses a dominant green once a check has failed, so a terminal too
	// narrow for CHECK still carries the signal.
	wantTitles := map[int][]string{
		0:   {" ", "LABEL", "PROVIDER", "STATUS", "HEALTH", "CHECK", "CHECKED", "UTIL5H", "UTIL7D", "AGE", "COOLDOWN", "LAST TEST"},
		60:  {" ", "LABEL", "STATUS", "UTIL5H"},
		72:  {" ", "LABEL", "STATUS", "UTIL5H", "COOLDOWN"},
		80:  {" ", "LABEL", "STATUS", "HEALTH", "UTIL5H", "COOLDOWN"},
		100: {" ", "LABEL", "STATUS", "HEALTH", "CHECK", "UTIL5H", "COOLDOWN"},
		140: {" ", "LABEL", "PROVIDER", "STATUS", "HEALTH", "CHECK", "UTIL5H", "AGE", "COOLDOWN", "LAST TEST"},
		170: {" ", "LABEL", "PROVIDER", "STATUS", "HEALTH", "CHECK", "CHECKED", "UTIL5H", "UTIL7D", "AGE", "COOLDOWN", "LAST TEST"},
	}

	var unfitted []table.Column
	for _, width := range []int{0, 60, 72, 80, 100, 140, 170} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			m := NewAccountsListModel(&accountsStub{accounts: responsiveAccountsFixture()}, context.Background())
			m.accounts = responsiveAccountsFixture()
			m.rebuildTable()
			if width > 0 {
				updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
				m = updated.(AccountsListModel)
			}

			cols := m.table.Columns()
			assertTableRowsMatchColumns(t, cols, m.table.Rows())
			if width > 0 && columnsWidth(cols) > width {
				t.Fatalf("columns width = %d, want <= terminal width %d", columnsWidth(cols), width)
			}
			if !slices.Contains(columnTitles(cols), "LABEL") {
				t.Fatalf("titles = %v, want priority-0 LABEL retained", columnTitles(cols))
			}
			if width == 72 && !slices.Contains(columnTitles(cols), "STATUS") {
				t.Fatalf("titles = %v, want STATUS retained at 72 columns", columnTitles(cols))
			}
			if width == 0 {
				unfitted = append([]table.Column(nil), cols...)
			}
			if got := columnTitles(cols); !slices.Equal(got, wantTitles[width]) {
				t.Fatalf("width %d titles = %v, want %v", width, got, wantTitles[width])
			}
			if width == 170 && !reflect.DeepEqual(cols, unfitted) {
				t.Fatalf("170-column set = %#v, want byte-identical unfitted %#v", cols, unfitted)
			}
		})
	}
}

func TestAccountsListRebuildTable_ResizeKeepsSelectedRowVisible(t *testing.T) {
	const cursor = 25
	accounts := make([]*pb.Account, 50)
	for i := range accounts {
		accounts[i] = &pb.Account{Id: fmt.Sprintf("account-%d", i), Label: fmt.Sprintf("account-%02d", i), Provider: "claude"}
	}

	m := NewAccountsListModel(&accountsStub{accounts: accounts}, context.Background())
	m.accounts, m.width, m.height = accounts, 140, 13
	m.rebuildTable()
	m.table.SetCursor(cursor)
	m.table.MoveDown(0)
	updateCursorColumn(&m.table)

	for _, width := range []int{72, 140} {
		m.width = width
		m.rebuildTable()
		if got := m.table.Cursor(); got != cursor {
			t.Fatalf("cursor after resize to %d = %d, want %d", width, got, cursor)
		}
		if got := stripANSI(m.table.View()); !strings.Contains(got, "account-25") {
			t.Fatalf("selected row is outside the %d-column viewport:\n%s", width, got)
		}
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

// TestAccountLastTestCellMasksInjectionFailure pins BOS-973 on the TUI surface:
// the recorded credential-injection reason reaches the LAST TEST cell ONLY
// through maskTestError, so it is redacted, collapsed to one line, and
// truncated to the column — never rendered raw. The reason is a materialize
// error carrying filesystem paths, so masking is what keeps a future,
// less-careful reason string from leaking anything secret-shaped.
func TestAccountLastTestCellMasksInjectionFailure(t *testing.T) {
	const reason = "credential injection failed: materialize codex account: " +
		"project codex base home: refusing account projection\n" +
		"\t\"~/.config/accounts/codex/acct-codex-2/config.toml\": existing entry is not a symlink"
	a := &pb.Account{Id: "acct-codex-2", Provider: "codex", Health: "failed", LastTestError: reason}

	got := accountLastTestCell(a)
	if got == reason {
		t.Fatal("LAST TEST cell rendered the raw reason; it must go through maskTestError")
	}
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("LAST TEST cell is not collapsed to one line: %q", got)
	}
	if lipgloss.Width(got) > 22 {
		t.Fatalf("LAST TEST cell width %d exceeds the column budget: %q", lipgloss.Width(got), got)
	}
	// The operator must still be able to tell this apart from a login failure:
	// the injection prefix is what leads the cell.
	if !strings.HasPrefix(got, "credential inject") {
		t.Fatalf("LAST TEST cell = %q, want it to lead with the injection prefix", got)
	}

	// The detail screen has a wider budget and shows the full prefix, which is
	// what tells the operator this is an injection failure rather than a
	// rejected credential.
	detail := accountLastTestedDetail(a)
	if !strings.Contains(detail, "credential injection failed") {
		t.Fatalf("detail health line = %q, want the full injection prefix", detail)
	}
	if !strings.HasPrefix(detail, "failed · ") {
		t.Fatalf("detail last-tested line = %q, want the failed framing", detail)
	}
}

// ListSessionsWithReadFailures satisfies the BossClient seam: this fake reads
// one place, so it never reports a partial read.
func (s *accountsStub) ListSessionsWithReadFailures(ctx context.Context, req *pb.ListSessionsRequest, opts client.SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error) {
	sessions, err := s.ListSessions(ctx, req, opts)
	return sessions, nil, err
}

// --- BOS-1142: credential-check state and the reauthenticate key ---

// authCheckedAccount builds a codex account carrying a durable auth-check
// verdict, so a test can pin what each verdict renders as.
func authCheckedAccount(id, outcome, class string, checkedAgo time.Duration) *pb.Account {
	a := &pb.Account{
		Id:       id,
		Provider: "codex",
		Label:    id,
		Status:   "active",
		Health:   "ok",
	}
	if outcome != "" {
		a.AuthCheck = &pb.AuthCheck{
			Outcome:      outcome,
			FailureClass: class,
			CheckedAt:    timestamppb.New(time.Now().Add(-checkedAgo)),
		}
	}
	return a
}

func TestAccountsListNeverCheckedIsDistinctFromCheckedAndClean(t *testing.T) {
	// BOS-892: an account nobody ever verified and an account verified clean are
	// different facts. If they render the same, an operator reads an unproven
	// credential as proven.
	never := authCheckedAccount("acct-never", "", "", 0)
	clean := authCheckedAccount("acct-clean", authCheckHealthy, "", 4*time.Minute)

	if got := accountCheckLabel(never); got != "never checked" {
		t.Fatalf("never-checked label = %q, want %q", got, "never checked")
	}
	if got := accountCheckLabel(clean); got != "ok" {
		t.Fatalf("checked-clean label = %q, want %q", got, "ok")
	}
	if accountCheckLabel(never) == accountCheckLabel(clean) {
		t.Fatal("never-checked and checked-clean render identically")
	}
	if got := accountCheckAgeCell(never); got != "—" {
		t.Fatalf("never-checked age = %q, want an em dash (no age may be claimed)", got)
	}
	if got := accountCheckAgeCell(clean); got == "—" {
		t.Fatal("checked-clean account rendered no check age")
	}

	stub := &accountsStub{accounts: []*pb.Account{never, clean}}
	m := seedAccountsList(t, stub)
	content := stripANSI(m.View().Content)
	for _, want := range []string{"CHECK", "CHECKED", "never checked"} {
		if !strings.Contains(content, want) {
			t.Fatalf("accounts list missing %q\n%s", want, content)
		}
	}
}

func TestAccountsListFailedCheckIsNeverADominantGreenRow(t *testing.T) {
	// The whole point of BOS-1142: a row whose credential the provider rejected
	// must not be painted with the success accent, whatever the health column
	// still says.
	failed := authCheckedAccount("acct-bad", authCheckAuthInvalid, "auth_invalidated", 3*time.Minute)
	failed.LastTestError = "credential injection failed: materialize codex credentials"

	if got := accountCheckLabel(failed); got != "failed:auth_invalidated" {
		t.Fatalf("failed-check label = %q, want the class inline", got)
	}
	if !accountCheckFailed(failed) {
		t.Fatal("accountCheckFailed did not recognise an auth_invalid verdict")
	}

	green := styleStatusSuccess.Render("ok")
	if got := accountHealthCellFor(failed, "ok"); got == green {
		t.Fatalf("health cell = %q, want the success accent withheld after a failed check", got)
	}
	// A transient verdict is NOT a credential fault (BOS-881), so it must not be
	// painted as one -- but it is not proof of health either, so it does not earn
	// the success accent. It reads as "ok?": no verdict, not an accusation.
	transient := authCheckedAccount("acct-blip", "transient", "transient_provider", time.Minute)
	got := accountHealthCellFor(transient, "ok")
	if got == green {
		t.Fatalf("health cell for a transient verdict = %q, want the success accent withheld", got)
	}
	if plain := stripANSI(got); !strings.Contains(plain, "ok?") {
		t.Fatalf("health cell for a transient verdict = %q, want the unproven mark \"ok?\"", plain)
	}
	if plain := stripANSI(got); strings.Contains(plain, "failed") {
		t.Fatalf("health cell for a transient verdict = %q, must not read as a credential fault", plain)
	}

	stub := &accountsStub{accounts: []*pb.Account{failed}}
	m := seedAccountsList(t, stub)
	content := m.View().Content
	if strings.Contains(content, green) {
		t.Fatalf("failed-check row still renders a dominant green ok cell\n%s", content)
	}
	plain := stripANSI(content)
	if !strings.Contains(plain, "failed:auth_invalidated") {
		t.Fatalf("failed-check row lost its failure class\n%s", plain)
	}
}

func TestAccountsListRedactedDiagnosticLeadsWithItsPrefix(t *testing.T) {
	// The injection prefix is what tells an operator this was a materialization
	// failure rather than a rejected login. It has to survive both the mask and
	// the column budget, so it must LEAD the cell.
	a := authCheckedAccount("acct-bad", authCheckAuthInvalid, "auth_invalidated", time.Minute)
	a.LastTestError = "credential injection failed: Authorization: Bearer sk-live-abcdef0123456789 rejected"

	cell := accountLastTestCell(a)
	if !strings.HasPrefix(cell, "credential injectio") {
		t.Fatalf("last-test cell = %q, want it to lead with the injection prefix", cell)
	}
	if strings.Contains(cell, "sk-live-abcdef0123456789") {
		t.Fatalf("last-test cell leaked credential-shaped text: %q", cell)
	}
	if lipgloss.Width(cell) > 22 {
		t.Fatalf("last-test cell width = %d, want <= the 22-cell column budget", lipgloss.Width(cell))
	}

	detail := accountCheckedDetail(a)
	if !strings.Contains(detail, "failed:auth_invalidated") {
		t.Fatalf("detail line = %q, want the verdict and class", detail)
	}
	if strings.Contains(detail, "sk-live-abcdef0123456789") {
		t.Fatalf("detail line leaked credential-shaped text: %q", detail)
	}
}

func TestAccountsListReauthKeyDispatchesOnceAndIsGuarded(t *testing.T) {
	stub := &accountsStub{accounts: []*pb.Account{
		authCheckedAccount("acct-bad", authCheckAuthInvalid, "auth_invalidated", time.Minute),
	}}
	m := seedAccountsList(t, stub)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	m = updated.(AccountsListModel)
	if cmd == nil {
		t.Fatal("[R] produced no command")
	}
	msg, ok := cmd().(switchViewMsg)
	if !ok {
		t.Fatalf("[R] produced %T, want a switchViewMsg", cmd())
	}
	if msg.view != ViewAccountRegister || msg.reauthAccountID != "acct-bad" {
		t.Fatalf("switch = %+v, want the register view in reauth mode for acct-bad", msg)
	}
	if msg.returnView != ViewAccounts {
		t.Fatalf("returnView = %v, want ViewAccounts", msg.returnView)
	}

	// A second press before the switch lands must not open a second device
	// login against the same account.
	_, cmd2 := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd2 != nil {
		t.Fatalf("second [R] press dispatched %T, want it suppressed while in flight", cmd2())
	}
}

func TestAccountsListReauthRefusesANonCodexAccountInPlace(t *testing.T) {
	// Refusing before the flow starts is the point: the operator is not sent to
	// a browser for a provider whose device login does not exist here.
	claude := &pb.Account{Id: "acct-claude", Provider: "claude", Label: "Claude Prod", Status: "active", Health: "ok"}
	stub := &accountsStub{accounts: []*pb.Account{claude}}
	m := seedAccountsList(t, stub)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	m = updated.(AccountsListModel)
	if cmd != nil {
		t.Fatalf("[R] on a claude account dispatched %T, want a refusal in place", cmd())
	}
	content := stripANSI(m.View().Content)
	if !strings.Contains(content, "boss account refresh") {
		t.Fatalf("refusal does not name the alternative:\n%s", content)
	}
}

func TestAccountsListReauthKeyIsDistinctFromTheRefreshUsageKey(t *testing.T) {
	// Regression pin: [r] re-probes usage read-only and touches no credential;
	// [R] replaces one. Collapsing them would let a routine usage probe
	// overwrite a stored credential.
	stub := &accountsStub{accounts: []*pb.Account{
		authCheckedAccount("acct-bad", authCheckAuthInvalid, "auth_invalidated", time.Minute),
	}}
	m := seedAccountsList(t, stub)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	m = updated.(AccountsListModel)
	if cmd == nil {
		t.Fatal("[r] produced no command")
	}
	if _, ok := cmd().(switchViewMsg); ok {
		t.Fatal("[r] switched views; the refresh-usage key must stay a read-only probe")
	}
	if m.reauthing["acct-bad"] {
		t.Fatal("[r] marked the account as reauthenticating")
	}

	bar := stripANSI(m.View().Content)
	if !strings.Contains(bar, "[R]eauth") || !strings.Contains(bar, "[r]efresh") {
		t.Fatalf("action bar does not offer both verbs:\n%s", bar)
	}
}

// TestAccountsListRebuildTable_SupersededSurvivesCheckColumnDrop pins the
// responsive half of BOS-1175.
//
// CHECK is priority 4 / minWidth 1 while HEALTH is priority 3, so a narrow
// terminal drops CHECK first — and CHECK is the cell that spells the class out.
// accountCheckSeverity deliberately keeps a superseded credential at
// checkSeverityOK (eligibility is unaffected), so without the HEALTH mark this
// row renders a plain green "ok" indistinguishable from a clean account once
// CHECK is gone. Every other test for this feature exercises the pure helpers,
// none of which see fitColumnsIndexed.
func TestAccountsListRebuildTable_SupersededSurvivesCheckColumnDrop(t *testing.T) {
	superseded := &pb.Account{
		Id: "acct-superseded", Label: "superseded", Provider: "codex", Health: "ok",
		AuthCheck: &pb.AuthCheck{Outcome: "healthy", FailureClass: AuthCheckClassSuperseded},
	}
	clean := &pb.Account{
		Id: "acct-clean", Label: "cleanacct", Provider: "codex", Health: "ok",
		AuthCheck: &pb.AuthCheck{Outcome: "healthy"},
	}
	accounts := []*pb.Account{superseded, clean}

	m := NewAccountsListModel(&accountsStub{accounts: accounts}, context.Background())
	m.accounts, m.height = accounts, 13

	for _, width := range []int{60, 72} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			m.width = width
			m.rebuildTable()

			titles := columnTitles(m.table.Columns())
			if slices.Contains(titles, "CHECK") {
				t.Skipf("CHECK survives at width %d (titles %v); this test only covers the dropped case", width, titles)
			}
			if !slices.Contains(titles, "HEALTH") {
				t.Fatalf("width %d dropped HEALTH as well (titles %v); the mark has nowhere to live", width, titles)
			}

			view := stripANSI(m.table.View())
			if !strings.Contains(view, "ok"+healthSupersededMark) {
				t.Fatalf("width %d: superseded row lost its HEALTH mark %q once CHECK dropped:\n%s", width, healthSupersededMark, view)
			}
			// The clean row must stay unmarked, or the mark says nothing.
			if strings.Count(view, "ok"+healthSupersededMark) != 1 {
				t.Fatalf("width %d: want exactly one marked row, got %d:\n%s", width, strings.Count(view, "ok"+healthSupersededMark), view)
			}
		})
	}
}

// TestAccountsListRefreshChainUnprovenRendersAsItself is the BOS-1174 TUI
// mirror: a new daemon outcome is INVISIBLE on this surface until the mirror
// knows it, so it ships in the same change as the daemon that emits it.
//
// The state is a warning, not an accusation and not a green. The credential
// answered — so nothing here may say "failed" — but its refresh chain was never
// observed working, so nothing here may claim the confidence a green would.
// That is exactly the undetermined tier the "?" veto mark already exists for.
func TestAccountsListRefreshChainUnprovenRendersAsItself(t *testing.T) {
	unproven := authCheckedAccount(
		"acct-unproven", authCheckRefreshChainUnproven, "refresh_not_observed", 5*time.Minute)

	// The cell names the state itself. The failure class is deliberately NOT
	// appended: "refresh_chain_unproven" already is the whole message, and the
	// pair would be 43 columns against a CHECK budget of 24 — the BOS-892
	// truncation trap, which would eat the identifying half of the label.
	if got := accountCheckLabel(unproven); got != authCheckRefreshChainUnproven {
		t.Fatalf("label = %q, want the outcome rendered as itself (%q)", got, authCheckRefreshChainUnproven)
	}
	if got := accountCheckSeverity(unproven); got != checkSeverityUndetermined {
		t.Fatalf("severity = %v, want undetermined (a warning, not a verdict)", got)
	}
	if accountCheckFailed(unproven) {
		t.Fatal("an unproven refresh chain is not a confirmed credential fault")
	}

	// The health cell keeps its ok but wears the unproven mark, matching the
	// web fold's "Unverified" amber.
	if got, want := accountHealthCellFor(unproven, "ok"),
		styleStatusWarning.Render("ok"+healthVetoUnprovenMark); got != want {
		t.Fatalf("health cell = %q, want %q", got, want)
	}
	if got := accountHealthCellFor(unproven, "ok"); got == styleStatusSuccess.Render("ok") {
		t.Fatal("an unproven refresh chain must not keep the dominant green")
	}

	// And it survives to the screen intact rather than being truncated away.
	stub := &accountsStub{accounts: []*pb.Account{unproven}}
	m := seedAccountsList(t, stub)
	content := stripANSI(m.View().Content)
	if !strings.Contains(content, authCheckRefreshChainUnproven) {
		t.Fatalf("accounts list did not render %q\n%s", authCheckRefreshChainUnproven, content)
	}
}
