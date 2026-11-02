package accounts

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// syncedRegistryStub serves a canned registry read and counts reads. dialErr
// fails the dial, as a signed-out user's does, before any read.
type syncedRegistryStub struct {
	resp    *pb.ListSyncedAgentAccountsResponse
	err     error
	dialErr error
	calls   int
}

func (s *syncedRegistryStub) DialSyncedRegistry(context.Context) (SyncedRegistryRead, error) {
	if s.dialErr != nil {
		return nil, s.dialErr
	}
	return func(context.Context) (*pb.ListSyncedAgentAccountsResponse, error) {
		s.calls++
		return s.resp, s.err
	}, nil
}

// slowDialRegistry dials for longer than the registry deadline, as a login
// refresh behind a held lock can, and records the dial context's state when
// it finishes and the deadline the read ran under.
type slowDialRegistry struct {
	dialFor      time.Duration
	dialErr      error
	readDeadline time.Duration
}

func (s *slowDialRegistry) DialSyncedRegistry(ctx context.Context) (SyncedRegistryRead, error) {
	select {
	case <-time.After(s.dialFor):
	case <-ctx.Done():
	}
	s.dialErr = ctx.Err()
	return func(readCtx context.Context) (*pb.ListSyncedAgentAccountsResponse, error) {
		if deadline, ok := readCtx.Deadline(); ok {
			s.readDeadline = time.Until(deadline)
		}
		return &pb.ListSyncedAgentAccountsResponse{}, nil
	}, nil
}

func registryRow(id, label string, members int) *pb.SyncedAgentAccount {
	row := &pb.SyncedAgentAccount{Id: id, Label: label, SyncScope: "all_daemons"}
	for i := range members {
		row.Members = append(row.Members, &pb.SyncedAgentAccountMember{DaemonId: fmt.Sprintf("daemon-%d", i)})
	}
	return row
}

// syncAccounts are one account per classification: a two-member row, a
// one-member row, no synced id and a stale synced id.
func syncAccounts() []*pb.AgentAccount {
	return []*pb.AgentAccount{
		{Id: "shared", Label: "shared", Provider: "claude", Status: "active", SyncedAgentAccountId: "row-shared"},
		{Id: "solo", Label: "solo", Provider: "claude", Status: "active", SyncedAgentAccountId: "row-solo"},
		{Id: "plain", Label: "plain", Provider: "codex", Status: "active"},
		{Id: "stale", Label: "stale", Provider: "codex", Status: "active", SyncedAgentAccountId: "row-gone"},
	}
}

func syncRegistryFixture(withSuggestion bool) *pb.ListSyncedAgentAccountsResponse {
	resp := &pb.ListSyncedAgentAccountsResponse{Accounts: []*pb.SyncedAgentAccount{
		registryRow("row-shared", "shared", 2),
		registryRow("row-solo", "solo", 1),
		registryRow("row-peer", "solo", 1),
	}}
	if withSuggestion {
		resp.Suggestions = []*pb.SyncedAgentAccountSuggestion{{FirstId: "row-peer", SecondId: "row-solo", Reason: "matching_label"}}
	}
	return resp
}

// seedSyncedList builds a list whose registry read has been dispatched and,
// when deliver is set, has landed.
func seedSyncedList(t *testing.T, reg *syncedRegistryStub, deliver bool) AgentAccountsListModel {
	t.Helper()
	m := NewAgentAccountsListModel(&accountsStub{accounts: syncAccounts()}, context.Background())
	m.width, m.height = 260, 24
	if reg != nil {
		m.SetSyncedRegistry(reg, "https://app.example.test/settings/accounts")
	}
	updated, _ := m.Update(AgentAccountsLoadedMsg{Accounts: syncAccounts()})
	m = updated.(AgentAccountsListModel)
	if deliver {
		updated, _ = m.Update(m.sync.fetchCmd(context.Background())())
		m = updated.(AgentAccountsListModel)
	}
	return m
}

func syncCell(t *testing.T, m AgentAccountsListModel, id string) string {
	t.Helper()
	for _, a := range m.accounts {
		if a.GetId() == id {
			return m.sync.cell(a)
		}
	}
	t.Fatalf("no account %q", id)
	return ""
}

func TestSyncedListClassifiesEachRow_Accounts_AC225601(t *testing.T) {
	m := seedSyncedList(t, &syncedRegistryStub{resp: syncRegistryFixture(false)}, true)
	want := map[string]display.AgentAccountSync{
		"shared": {State: display.AgentAccountSyncSynced, MemberCount: 2},
		"solo":   {State: display.AgentAccountSyncLocalOnly, MemberCount: 1},
		"plain":  {State: display.AgentAccountSyncLocalOnly},
		"stale":  {State: display.AgentAccountSyncLocalOnly},
	}
	for _, a := range m.accounts {
		got := m.sync.resolve(a)
		if got.State != want[a.GetId()].State || got.MemberCount != want[a.GetId()].MemberCount {
			t.Errorf("%s resolved to %+v, want %+v", a.GetId(), got, want[a.GetId()])
		}
	}
	if cell := syncCell(t, m, "shared"); !strings.Contains(cell, "2") {
		t.Errorf("shared SYNC cell %q does not carry its member count", cell)
	}
	for _, id := range []string{"solo", "plain", "stale"} {
		if cell := syncCell(t, m, id); cell != syncCell(t, m, "plain") {
			t.Errorf("%s SYNC cell %q differs from the local-only cell", id, cell)
		}
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(AgentAccountsListModel)
	titles := columnTitles(m.table.Columns())
	if !slices.Contains(titles, "UTIL5H") || !slices.Contains(titles, "UTIL7D") {
		t.Fatalf("80-column titles = %v, want both usage columns", titles)
	}
	assertNoOverflow(t, m, 80)
}

func TestSyncedListNeverShowsUnknownAsSynced_Accounts_AC225601(t *testing.T) {
	cases := map[string]*syncedRegistryStub{
		"no client":   nil,
		"signed out":  {dialErr: errors.New("not logged in")},
		"read failed": {err: rpcerr.New(rpcerr.Unavailable, errors.New("bosso down"))},
	}
	for name, reg := range cases {
		t.Run(name, func(t *testing.T) {
			m := seedSyncedList(t, reg, reg != nil)
			for _, id := range []string{"shared", "solo", "stale"} {
				a := m.accounts[slices.IndexFunc(m.accounts, func(a *pb.AgentAccount) bool { return a.GetId() == id })]
				if got := m.sync.resolve(a).State; got != display.AgentAccountSyncUnknown {
					t.Errorf("%s resolved to %q, want unknown", id, got)
				}
				if strings.HasPrefix(m.sync.cell(a), "synced") {
					t.Errorf("%s renders as synced without a registry", id)
				}
			}
		})
	}
}

func TestSyncedListPendingReadAndRefetch_Accounts_AC225601(t *testing.T) {
	reg := &syncedRegistryStub{resp: syncRegistryFixture(false)}
	m := seedSyncedList(t, reg, false)
	if got := syncCell(t, m, "shared"); got != "…" {
		t.Errorf("in-flight SYNC cell = %q, want an ellipsis", got)
	}
	if got := syncCell(t, m, "plain"); got == "…" {
		t.Error("an account with no synced id waits on the registry")
	}
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init returned no command")
	}
	runBatch(m.Init())
	if reg.calls != 1 {
		t.Fatalf("Init registry reads = %d, want 1", reg.calls)
	}
	m = seedSyncedList(t, reg, true)
	reg.calls = 0
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	runBatch(cmd)
	if reg.calls != 1 {
		t.Fatalf("[r] registry reads = %d, want 1", reg.calls)
	}
}

func TestSyncedListSuggestionHint_Accounts_AC225601(t *testing.T) {
	without := seedSyncedList(t, &syncedRegistryStub{resp: syncRegistryFixture(false)}, true)
	if hint := without.sync.footerText(without.accounts); hint != "" {
		t.Errorf("hint shown with no suggestion: %q", hint)
	}
	if strings.Contains(syncCell(t, without, "solo"), "link") {
		t.Error("link marker shown with no suggestion")
	}

	with := seedSyncedList(t, &syncedRegistryStub{resp: syncRegistryFixture(true)}, true)
	hint := with.sync.footerText(with.accounts)
	if !strings.Contains(hint, "Settings → Accounts") || !strings.Contains(hint, "https://app.example.test/settings/accounts") {
		t.Errorf("hint %q does not name the web location", hint)
	}
	if !strings.Contains(flattenPrompt(with.View().Content), flattenPrompt(hint)) {
		t.Errorf("list view does not show the hint:\n%s", with.View().Content)
	}
	if !strings.Contains(syncCell(t, with, "solo"), "link") || strings.Contains(syncCell(t, with, "shared"), "link") {
		t.Error("link marker is not on exactly the suggested account")
	}
	updated, _ := with.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	assertNoOverflow(t, updated.(AgentAccountsListModel), 80)
}

func TestSyncedRegistryUnknownReason(t *testing.T) {
	cases := []struct {
		err    error
		signIn bool
	}{
		{errors.New("not logged in"), true},
		{rpcerr.New(rpcerr.Unauthenticated, errors.New("expired")), true},
		{rpcerr.New(rpcerr.Unavailable, errors.New("down")), false},
		{rpcerr.New(rpcerr.PermissionDenied, errors.New("no cloud entitlement")), false},
		{fmt.Errorf("token request: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}), false},
		{fmt.Errorf("token request: %w", context.DeadlineExceeded), false},
	}
	for _, tc := range cases {
		r := newSyncRegistry(&syncedRegistryStub{}, "")
		r.apply(syncedRegistryLoadedMsg{err: tc.err})
		if r.known || r.signIn != tc.signIn {
			t.Errorf("%v: known=%v signIn=%v, want false %v", tc.err, r.known, r.signIn, tc.signIn)
		}
	}
}

func TestAccountEditFetchesRegistry_Accounts_AC225601(t *testing.T) {
	solo := syncAccounts()[1]
	plain := newAccountEditModel(&stubAccountEditClient{}, solo)
	if plain.Init() != nil {
		t.Fatal("Init fetches with no registry client")
	}
	if rows := accountDetailRows(solo, time.Now(), plain.sync); detailValue(rows, "Link suggestion") != "" {
		t.Error("link suggestion row shown with no registry")
	}

	reg := &syncedRegistryStub{resp: syncRegistryFixture(true)}
	m := newAccountEditModel(&stubAccountEditClient{}, solo)
	m.SetSyncedRegistry(reg, "")
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned no fetch command with a registry client")
	}
	updated, _ := m.Update(cmd())
	m = updated.(AgentAccountEditModel)
	if reg.calls != 1 {
		t.Fatalf("registry reads = %d, want 1", reg.calls)
	}
	if got := m.sync.resolve(solo); got.State != display.AgentAccountSyncLocalOnly || len(got.SuggestedPeers) != 1 {
		t.Fatalf("resolved %+v, want local only with one peer", got)
	}
	rows := accountDetailRows(solo, time.Now(), m.sync)
	if link := detailValue(rows, "Link suggestion"); !strings.Contains(link, "Settings → Accounts") {
		t.Errorf("link suggestion row = %q, want the web location", link)
	}
	if !strings.Contains(flattenPrompt(m.View().Content), "Link suggestion") {
		t.Error("detail view omits the link suggestion row")
	}

	shared := syncAccounts()[0]
	sharedModel := newAccountEditModel(&stubAccountEditClient{}, shared)
	sharedModel.SetSyncedRegistry(&syncedRegistryStub{resp: syncRegistryFixture(true)}, "")
	updated, _ = sharedModel.Update(sharedModel.Init()())
	sharedModel = updated.(AgentAccountEditModel)
	if got := sharedModel.sync.resolve(shared); got.State != display.AgentAccountSyncSynced || got.MemberCount != 2 {
		t.Fatalf("resolved %+v, want synced with 2 members", got)
	}
	if detailValue(accountDetailRows(shared, time.Now(), sharedModel.sync), "Link suggestion") != "" {
		t.Error("link suggestion row shown for an account no suggestion involves")
	}
}

func detailValue(rows []ui.KeyValue, key string) string {
	for _, row := range rows {
		if row.Key == key {
			return row.Value
		}
	}
	return ""
}

func assertNoOverflow(t *testing.T, m AgentAccountsListModel, width int) {
	t.Helper()
	for _, line := range strings.Split(m.View().Content, "\n") {
		if lipgloss.Width(line) > width {
			t.Fatalf("width %d overflow: %q", width, line)
		}
	}
}

// runBatch runs cmd and, when it is a batch, every command in it once.
func runBatch(cmd tea.Cmd) {
	msg := runCmd(cmd)
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return
	}
	for _, c := range batch {
		if c != nil {
			c()
		}
	}
}

// The dial may refresh an expired login, which rotates the single-use refresh
// token; a deadline that cancels it after the rotation forces `boss login`. So
// the registry deadline bounds only the read, never the dial.
func TestSyncedRegistryDeadlineSkipsTheDial_Accounts_AC225601(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := &slowDialRegistry{dialFor: 2 * syncedRegistryTimeout}
		r := newSyncRegistry(reg, "")
		msg := r.fetchCmd(context.Background())().(syncedRegistryLoadedMsg)
		if reg.dialErr != nil {
			t.Fatalf("dial context ended with %v, want it to outlive the registry deadline", reg.dialErr)
		}
		if msg.err != nil {
			t.Fatalf("read failed after a slow dial: %v", msg.err)
		}
		if reg.readDeadline != syncedRegistryTimeout {
			t.Errorf("read deadline = %v, want %v from the end of the dial", reg.readDeadline, syncedRegistryTimeout)
		}
		r.apply(msg)
		if !r.known {
			t.Error("registry unknown after a slow dial that succeeded")
		}
	})
}
