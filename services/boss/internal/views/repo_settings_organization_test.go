package views

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeOrgClient is the view's RepoOrganizationClient seam. Every method records
// what it was asked so a test can assert which RPC a keystroke reached — the
// difference between "set" and "clear" is the whole of AC#3.
type fakeOrgClient struct {
	orgs    []*pb.Organization
	listErr error

	mapping *pb.RepoOrganizationMapping
	getErr  error

	setErr error

	setCalls   []setOrgCall
	clearCalls []setOrgCall
	listCalls  int
	getCalls   int
}

type setOrgCall struct {
	originURL string
	orgID     string
}

func (f *fakeOrgClient) ListOrganizations(context.Context) ([]*pb.Organization, error) {
	f.listCalls++
	return f.orgs, f.listErr
}

func (f *fakeOrgClient) GetRepoOrganization(_ context.Context, originURL string) (*pb.RepoOrganizationMapping, error) {
	f.getCalls++
	_ = originURL
	return f.mapping, f.getErr
}

func (f *fakeOrgClient) SetRepoOrganization(_ context.Context, originURL, orgID string) (*pb.RepoOrganizationMapping, error) {
	f.setCalls = append(f.setCalls, setOrgCall{originURL: originURL, orgID: orgID})
	if f.setErr != nil {
		return nil, f.setErr
	}
	return &pb.RepoOrganizationMapping{RepoOriginUrl: originURL, OrganizationId: orgID}, nil
}

func (f *fakeOrgClient) ClearRepoOrganization(_ context.Context, originURL, orgID string) error {
	f.clearCalls = append(f.clearCalls, setOrgCall{originURL: originURL, orgID: orgID})
	return nil
}

const testRepoOrigin = "https://github.com/recurser/bossanova"

// newLoadedOrgSettings builds a repo settings model with the organization client
// injected, drives the settings load, and then drives whatever the load chained
// off itself. It returns the settled model so each test starts from the state a
// user actually reaches.
func newLoadedOrgSettings(t *testing.T, org *fakeOrgClient) RepoSettingsModel {
	t.Helper()
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:          "repo-1",
		DisplayName: "Test Repo",
		OriginUrl:   testRepoOrigin,
	}}}
	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	if org != nil {
		m.SetRepoOrganizationClient(org)
	}

	updated, cmd := m.Update(m.Init()())
	m = updated.(RepoSettingsModel)
	for _, msg := range drainCmd(cmd) {
		updated, _ = m.Update(msg)
		m = updated.(RepoSettingsModel)
	}
	return m
}

// drainCmd runs a command and flattens any tea.Batch it produces into the
// concrete messages a running program would deliver. Nil messages (a nil child
// command, e.g. the GitHub status load with no client) are dropped.
func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, child := range batch {
			out = append(out, drainCmd(child)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// orgView renders the settings view to plain text for token assertions.
func orgView(t *testing.T, m RepoSettingsModel) string {
	t.Helper()
	return stripANSI(viewString(m))
}

// TestRepoSettingsOrganization_UnmappedShowsPersonalDefault covers AC#2: the
// field exists and an unmapped repo reads as Personal, not as an empty value.
func TestRepoSettingsOrganization_UnmappedShowsPersonalDefault(t *testing.T) {
	org := &fakeOrgClient{orgs: []*pb.Organization{{Id: "org-acme", Name: "Acme"}}}
	m := newLoadedOrgSettings(t, org)

	out := orgView(t, m)
	if !strings.Contains(out, "Organization: "+repoSettingsNoOrgLabel) {
		t.Errorf("view does not show the Personal default:\n%s", out)
	}
	if strings.Contains(out, "Organization: (not set)") {
		t.Error("unmapped organization rendered as an unset field, want the Personal default")
	}
}

// TestRepoSettingsOrganization_ListIsExactlyTheCallersOrganizations covers AC#4.
// The picker offers Personal plus the ListOrganizations result verbatim — same
// entries, same order, nothing invented and nothing dropped.
func TestRepoSettingsOrganization_ListIsExactlyTheCallersOrganizations(t *testing.T) {
	org := &fakeOrgClient{orgs: []*pb.Organization{
		{Id: "org-acme", Name: "Acme"},
		{Id: "org-recurse", Name: "Recurse"},
	}}
	m := newLoadedOrgSettings(t, org)

	if org.listCalls != 1 {
		t.Fatalf("ListOrganizations called %d times, want exactly 1", org.listCalls)
	}

	choices := m.orgChoices()
	want := []orgChoice{
		{label: repoSettingsNoOrgLabel},
		{id: "org-acme", label: "Acme"},
		{id: "org-recurse", label: "Recurse"},
	}
	if len(choices) != len(want) {
		t.Fatalf("orgChoices = %v, want %v", choices, want)
	}
	for i := range want {
		if choices[i] != want[i] {
			t.Errorf("orgChoices[%d] = %v, want %v", i, choices[i], want[i])
		}
	}

	cursorToRow(t, &m, repoSettingsRowOrganization)
	updated, _ := m.activateRow()
	m = updated.(RepoSettingsModel)
	out := orgView(t, m)
	for _, name := range []string{repoSettingsNoOrgLabel, "Acme", "Recurse"} {
		if !strings.Contains(out, name) {
			t.Errorf("picker does not list %q:\n%s", name, out)
		}
	}
}

// TestRepoSettingsOrganization_SelectingWritesSetRepoOrganization covers the
// first half of AC#3.
func TestRepoSettingsOrganization_SelectingWritesSetRepoOrganization(t *testing.T) {
	org := &fakeOrgClient{orgs: []*pb.Organization{{Id: "org-acme", Name: "Acme"}}}
	m := newLoadedOrgSettings(t, org)

	cursorToRow(t, &m, repoSettingsRowOrganization)
	updated, _ := m.activateRow()
	m = updated.(RepoSettingsModel)
	if !m.orgPickerOpen {
		t.Fatal("activating the organization row did not open the picker")
	}

	// Move off Personal onto Acme, then confirm.
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m = updated.(RepoSettingsModel)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(RepoSettingsModel)

	if len(org.setCalls) != 0 {
		t.Fatal("SetRepoOrganization ran inside Update; it must run in the returned command (AC#7)")
	}
	for _, msg := range drainCmd(cmd) {
		updated, _ = m.Update(msg)
		m = updated.(RepoSettingsModel)
	}

	if len(org.setCalls) != 1 {
		t.Fatalf("SetRepoOrganization calls = %d, want 1", len(org.setCalls))
	}
	if got := org.setCalls[0]; got.orgID != "org-acme" || got.originURL != testRepoOrigin {
		t.Errorf("SetRepoOrganization(%q, %q), want (%q, %q)", got.originURL, got.orgID, testRepoOrigin, "org-acme")
	}
	if len(org.clearCalls) != 0 {
		t.Errorf("ClearRepoOrganization called %d times on a select, want 0", len(org.clearCalls))
	}
	if out := orgView(t, m); !strings.Contains(out, "Organization: Acme") {
		t.Errorf("view does not reflect the new mapping:\n%s", out)
	}
}

// TestRepoSettingsOrganization_ResetToPersonalClearsMapping covers the second
// half of AC#3: Personal is a clear, not a set with an empty id.
func TestRepoSettingsOrganization_ResetToPersonalClearsMapping(t *testing.T) {
	org := &fakeOrgClient{
		orgs:    []*pb.Organization{{Id: "org-acme", Name: "Acme"}},
		mapping: &pb.RepoOrganizationMapping{RepoOriginUrl: testRepoOrigin, OrganizationId: "org-acme"},
	}
	m := newLoadedOrgSettings(t, org)

	if out := orgView(t, m); !strings.Contains(out, "Organization: Acme") {
		t.Fatalf("mapped repo does not render its organization:\n%s", out)
	}

	cursorToRow(t, &m, repoSettingsRowOrganization)
	updated, _ := m.activateRow()
	m = updated.(RepoSettingsModel)
	if m.orgPickerCursor != 1 {
		t.Errorf("picker opened at index %d, want 1 (the mapping in force)", m.orgPickerCursor)
	}

	// Move up to Personal and confirm.
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'k', Text: "k"})
	m = updated.(RepoSettingsModel)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(RepoSettingsModel)
	for _, msg := range drainCmd(cmd) {
		updated, _ = m.Update(msg)
		m = updated.(RepoSettingsModel)
	}

	if len(org.clearCalls) != 1 {
		t.Fatalf("ClearRepoOrganization calls = %d, want 1", len(org.clearCalls))
	}
	// The clear is organization-scoped: it has to name the id that was mapped,
	// not the empty id being selected, or bosso's ownership check has nothing
	// to check against.
	if got := org.clearCalls[0]; got.orgID != "org-acme" || got.originURL != testRepoOrigin {
		t.Errorf("ClearRepoOrganization(%q, %q), want (%q, %q)", got.originURL, got.orgID, testRepoOrigin, "org-acme")
	}
	if len(org.setCalls) != 0 {
		t.Errorf("SetRepoOrganization called %d times on a reset, want 0", len(org.setCalls))
	}
	if out := orgView(t, m); !strings.Contains(out, "Organization: "+repoSettingsNoOrgLabel) {
		t.Errorf("view did not return to the Personal default:\n%s", out)
	}
}

// TestRepoSettingsOrganization_HiddenWithoutCloudClient covers AC#5: a
// local-only / signed-out user reaches the view with no organization row at all
// — no panic, and no picker with nothing in it.
func TestRepoSettingsOrganization_HiddenWithoutCloudClient(t *testing.T) {
	m := newLoadedOrgSettings(t, nil)

	for _, row := range m.visibleRows() {
		if row == repoSettingsRowOrganization {
			t.Fatal("organization row is navigable with no cloud client")
		}
	}
	out := orgView(t, m)
	if strings.Contains(out, "Organization:") {
		t.Errorf("organization field rendered with no cloud client:\n%s", out)
	}
	// The Sentry integration has its own "Organization slug" child; the view must
	// still render normally, so prove we did not just get an empty screen.
	if !strings.Contains(out, "Merge strategy:") {
		t.Errorf("view did not render its ordinary rows:\n%s", out)
	}

	// Walking to the bottom and activating every row must not panic.
	for i := range m.visibleRows() {
		m.cursor = i
		updated, _ := m.activateRow()
		m = updated.(RepoSettingsModel)
	}
}

// TestRepoSettingsOrganization_SetTimeRefusalIsSurfaced covers AC#6 on the write
// path: bosso answers PermissionDenied for a non-member, and that has to land as
// its own line next to the setting rather than a generic RPC banner.
func TestRepoSettingsOrganization_SetTimeRefusalIsSurfaced(t *testing.T) {
	org := &fakeOrgClient{
		orgs:   []*pb.Organization{{Id: "org-acme", Name: "Acme"}},
		setErr: connect.NewError(connect.CodePermissionDenied, errors.New("organization membership required")),
	}
	m := pickFirstOrganization(t, newLoadedOrgSettings(t, org))

	out := orgView(t, m)
	if m.orgRefusal == "" {
		t.Fatal("orgRefusal is empty after a PermissionDenied set")
	}
	if !strings.Contains(out, "refused the mapping to Acme") {
		t.Errorf("refusal is not visible in the view:\n%s", out)
	}
	// bosso answers the same PermissionDenied for a wrong *active* organization
	// and for a lapsed entitlement as it does for real non-membership, and the
	// picker only ever offers organizations ListOrganizations returned — ones
	// the caller demonstrably belongs to. Asserting non-membership here would be
	// wrong on every route the user can actually take.
	if strings.Contains(out, repoSettingsNotAMemberPrefix) {
		t.Errorf("refusal claims non-membership for an organization in the caller's own list:\n%s", out)
	}
}

// TestRepoSettingsOrganization_SetTimeRefusalForAStrangerOrgClaimsNonMembership
// is the other half: when the refused organization really is absent from the
// caller's list, non-membership is established and the line says so.
func TestRepoSettingsOrganization_SetTimeRefusalForAStrangerOrgClaimsNonMembership(t *testing.T) {
	org := &fakeOrgClient{orgs: []*pb.Organization{{Id: "org-acme", Name: "Acme"}}}
	m := newLoadedOrgSettings(t, org)

	// Reach the classifier directly: the picker cannot offer an organization
	// outside the caller's list, so this is the defensive path.
	refusal := m.organizationSetRefusalMessage(
		connect.NewError(connect.CodePermissionDenied, errors.New("organization membership required")),
		"org-stranger",
	)
	if !strings.HasPrefix(refusal, repoSettingsNotAMemberPrefix) {
		t.Errorf("refusal for an organization outside the caller's list = %q, want the non-membership line", refusal)
	}
}

// TestRepoSettingsOrganization_AmbiguousScopeKeepsItsOwnInstruction pins that a
// FailedPrecondition is not rewritten. bosso raises it as "re-authenticate with
// an organization-scoped token", which is the actionable instruction; replacing
// it with a membership sentence would send the user somewhere that cannot help.
func TestRepoSettingsOrganization_AmbiguousScopeKeepsItsOwnInstruction(t *testing.T) {
	org := &fakeOrgClient{
		orgs: []*pb.Organization{{Id: "org-acme", Name: "Acme"}},
		setErr: connect.NewError(connect.CodeFailedPrecondition,
			errors.New("organization scope is ambiguous; re-authenticate with an organization-scoped token")),
	}
	m := pickFirstOrganization(t, newLoadedOrgSettings(t, org))

	if m.orgRefusal != "" {
		t.Errorf("orgRefusal = %q, want the ambiguous-scope error left to the ordinary error path", m.orgRefusal)
	}
	if m.err == nil {
		t.Fatal("the ambiguous-scope error was swallowed instead of reported")
	}
	if out := orgView(t, m); !strings.Contains(out, "re-authenticate with an organization-scoped token") {
		t.Errorf("the server's own instruction is not on screen:\n%s", out)
	}
}

// pickFirstOrganization drives the picker from the settled default onto the
// first real organization and confirms it, draining the resulting command.
func pickFirstOrganization(t *testing.T, m RepoSettingsModel) RepoSettingsModel {
	t.Helper()
	cursorToRow(t, &m, repoSettingsRowOrganization)
	updated, _ := m.activateRow()
	m = updated.(RepoSettingsModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m = updated.(RepoSettingsModel)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(RepoSettingsModel)
	for _, msg := range drainCmd(cmd) {
		updated, _ = m.Update(msg)
		m = updated.(RepoSettingsModel)
	}
	return m
}

// TestRepoSettingsOrganization_MappingReadFailureIsNotPersonal covers the second
// failure shape: the org list loaded but this repo's mapping did not. The
// current value is unknown, and rendering the Personal default would assert the
// one thing the failed read did not establish.
func TestRepoSettingsOrganization_MappingReadFailureIsNotPersonal(t *testing.T) {
	org := &fakeOrgClient{
		orgs:   []*pb.Organization{{Id: "org-acme", Name: "Acme"}},
		getErr: errors.New("mapping boom"),
	}
	m := newLoadedOrgSettings(t, org)

	out := orgView(t, m)
	if strings.Contains(out, "Organization: "+repoSettingsNoOrgLabel) {
		t.Errorf("an unread mapping is rendered as the Personal default:\n%s", out)
	}
	if !strings.Contains(out, "Organization: Unknown") {
		t.Errorf("the unknown mapping is not named as unknown:\n%s", out)
	}
	if !strings.Contains(out, "Could not read this repo's organization") {
		t.Errorf("the mapping failure is reported as something else:\n%s", out)
	}
	if strings.Contains(out, "Could not load organizations") {
		t.Errorf("a mapping failure is reported as a list failure:\n%s", out)
	}
	// The picker is still usable: only the current value is unknown.
	if len(m.orgChoices()) != 2 {
		t.Errorf("orgChoices = %d, want Personal plus the one organization", len(m.orgChoices()))
	}
}

// TestRepoSettingsOrganization_UnknownMappingDoesNotSwallowAReset covers the
// rest of the unknown-mapping invariant, at the two sites where the empty id an
// unread mapping leaves behind is indistinguishable from a genuine Personal one.
// The picker must not mark Personal as in force, and confirming Personal must
// not be discarded as a no-op -- a reset that silently does nothing is worse
// than one that says why it cannot run.
//
// It runs over both causes because organizationMappingUnknown is an OR: a
// ListOrganizations failure returns before GetRepoOrganization is issued, so
// only the list-failure case can catch a refusal that names the wrong read or
// that shadows the one diagnostic the user has.
func TestRepoSettingsOrganization_UnknownMappingDoesNotSwallowAReset(t *testing.T) {
	acme := []*pb.Organization{{Id: "org-acme", Name: "Acme"}}
	cases := []struct {
		name string
		org  *fakeOrgClient
		// underlyingNotice is the load error's own line, which the refusal must
		// sit above rather than replace.
		underlyingNotice string
		// selectable is false when the failure also cost us the list, leaving
		// Personal as the only row.
		selectable bool
	}{
		{
			name:             "mapping read failed",
			org:              &fakeOrgClient{orgs: acme, getErr: errors.New("mapping boom")},
			underlyingNotice: "Could not read this repo's organization",
			selectable:       true,
		},
		{
			name:             "organization list failed",
			org:              &fakeOrgClient{listErr: errors.New("list boom")},
			underlyingNotice: "Could not load organizations",
			selectable:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			org := tc.org
			m := newLoadedOrgSettings(t, org)

			cursorToRow(t, &m, repoSettingsRowOrganization)
			updated, _ := m.activateRow()
			m = updated.(RepoSettingsModel)
			if !m.orgPickerOpen {
				t.Fatal("activating the organization row did not open the picker")
			}
			if out := orgView(t, m); strings.Contains(out, "(current)") {
				t.Errorf("the picker names a current organization it never read:\n%s", out)
			}
			if m.orgPickerCursor != 0 {
				t.Fatalf("picker opened at index %d, want 0 (Personal)", m.orgPickerCursor)
			}

			// Confirming Personal is the reset that must not be swallowed. It
			// cannot be carried out either: ClearRepoOrganization takes the id
			// being cleared, and the unknown state supplied none.
			updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			reset := updated.(RepoSettingsModel)
			for _, msg := range drainCmd(cmd) {
				updated, _ = reset.Update(msg)
				reset = updated.(RepoSettingsModel)
			}
			if len(org.clearCalls) != 0 {
				t.Errorf("ClearRepoOrganization ran with no id to clear (%d calls)", len(org.clearCalls))
			}
			if len(org.setCalls) != 0 {
				t.Errorf("a reset was sent as a set (%d calls)", len(org.setCalls))
			}
			if reset.orgSaving {
				t.Error("the field reports a write in flight for a request that was never sent")
			}
			out := orgView(t, reset)
			if !strings.Contains(out, "nothing this view can reset") {
				t.Errorf("the refused reset is discarded without a word:\n%s", out)
			}
			// One message serves both causes, so it must not name either read:
			// the predicate cannot tell which one failed, and on a list failure
			// the mapping read was never issued at all. The notice line below
			// the refusal legitimately names the read that did fail, and it is
			// capitalised, so the lower-case form can only come from a refusal
			// that went back to naming one.
			if strings.Contains(out, "could not read this repo's organization") {
				t.Errorf("the refusal names a specific read the unknown state cannot establish:\n%s", out)
			}
			// Nor may it assert that no mapping exists. The failure is this
			// view's read, not the server's state: the server may hold a
			// mapping this view never received, so the refusal reports what
			// it could not determine rather than what is not so.
			if strings.Contains(out, "does not know") {
				t.Errorf("the refusal claims the mapping is unknown to the system, not just unread here:\n%s", out)
			}
			// Nor may it take away the diagnostic that says which read failed.
			if !strings.Contains(out, tc.underlyingNotice) {
				t.Errorf("the refusal shadowed the underlying %q notice:\n%s", tc.underlyingNotice, out)
			}

			if !tc.selectable {
				if len(m.orgChoices()) != 1 {
					t.Errorf("orgChoices = %d, want Personal alone when the list failed", len(m.orgChoices()))
				}
				return
			}

			// Picking a real organization from the same unknown state still
			// writes: only the reset lacks the id the server requires.
			updated, _ = m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
			pick := updated.(RepoSettingsModel)
			updated, cmd = pick.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			pick = updated.(RepoSettingsModel)
			for _, msg := range drainCmd(cmd) {
				updated, _ = pick.Update(msg)
				pick = updated.(RepoSettingsModel)
			}
			if len(org.setCalls) != 1 || org.setCalls[0].orgID != "org-acme" {
				t.Fatalf("selecting an organization from an unknown state did not set it: %+v", org.setCalls)
			}
			if out := orgView(t, pick); !strings.Contains(out, "Organization: Acme") {
				t.Errorf("the view does not reflect the organization that was just set:\n%s", out)
			}
		})
	}
}

// TestRepoSettingsOrganization_HiddenWithoutOriginURL covers the other half of
// AC#5's "no empty picker": a signed-in user on a repo with no git origin. The
// mapping is keyed by origin, so there is nothing to map and the picker would
// hold only the Personal row the field already shows.
func TestRepoSettingsOrganization_HiddenWithoutOriginURL(t *testing.T) {
	org := &fakeOrgClient{orgs: []*pb.Organization{{Id: "org-acme", Name: "Acme"}}}
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:          "repo-1",
		DisplayName: "Origin-free Repo",
	}}}
	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	m.SetRepoOrganizationClient(org)

	updated, cmd := m.Update(m.Init()())
	m = updated.(RepoSettingsModel)
	for _, msg := range drainCmd(cmd) {
		updated, _ = m.Update(msg)
		m = updated.(RepoSettingsModel)
	}

	for _, row := range m.visibleRows() {
		if row == repoSettingsRowOrganization {
			t.Fatal("the organization row is navigable for a repo with no origin URL")
		}
	}
	if out := orgView(t, m); strings.Contains(out, "Organization:") {
		t.Errorf("the organization row renders for a repo with no origin URL:\n%s", out)
	}
	if org.listCalls != 0 || org.getCalls != 0 {
		t.Errorf("organizations were fetched for an unmappable repo (list=%d get=%d)", org.listCalls, org.getCalls)
	}
	if out := orgView(t, m); !strings.Contains(out, "Merge strategy:") {
		t.Errorf("the rest of the settings view stopped rendering:\n%s", out)
	}
}

// TestRepoSettingsOrganization_InFlightWriteIsVisibleAndExclusive pins the
// in-flight state: the field says a write is running, and the picker cannot be
// reopened to race a second write against it.
func TestRepoSettingsOrganization_InFlightWriteIsVisibleAndExclusive(t *testing.T) {
	org := &fakeOrgClient{orgs: []*pb.Organization{{Id: "org-acme", Name: "Acme"}}}
	m := newLoadedOrgSettings(t, org)

	cursorToRow(t, &m, repoSettingsRowOrganization)
	updated, _ := m.activateRow()
	m = updated.(RepoSettingsModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m = updated.(RepoSettingsModel)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(RepoSettingsModel)

	if !m.orgSaving {
		t.Fatal("the model does not record that a write is in flight")
	}
	if out := orgView(t, m); !strings.Contains(out, "saving") {
		t.Errorf("the in-flight write is invisible on the field:\n%s", out)
	}

	// A second activation must not reopen the picker while the first write runs.
	updated, secondCmd := m.activateRow()
	second := updated.(RepoSettingsModel)
	if second.orgPickerOpen {
		t.Error("the picker reopened while a write was in flight")
	}
	if secondCmd != nil {
		t.Error("a second activation issued a command while a write was in flight")
	}

	for _, msg := range drainCmd(cmd) {
		updated, _ = m.Update(msg)
		m = updated.(RepoSettingsModel)
	}
	if m.orgSaving {
		t.Error("orgSaving is still set after the write landed")
	}
	if out := orgView(t, m); strings.Contains(out, "saving") {
		t.Errorf("the in-flight marker outlived the write:\n%s", out)
	}
	if len(org.setCalls) != 1 {
		t.Errorf("SetRepoOrganization calls = %d, want exactly 1", len(org.setCalls))
	}
}

// TestRepoSettingsOrganization_StoredMappingToUnknownOrgIsSurfaced covers AC#6
// on the read path: a mapping naming an organization absent from the caller's
// own list is called out rather than silently rendered as a bare id.
func TestRepoSettingsOrganization_StoredMappingToUnknownOrgIsSurfaced(t *testing.T) {
	org := &fakeOrgClient{
		orgs:    []*pb.Organization{{Id: "org-acme", Name: "Acme"}},
		mapping: &pb.RepoOrganizationMapping{RepoOriginUrl: testRepoOrigin, OrganizationId: "org-stranger"},
	}
	m := newLoadedOrgSettings(t, org)

	out := orgView(t, m)
	if !strings.Contains(out, repoSettingsNotAMemberPrefix) {
		t.Errorf("cross-organization mapping is not surfaced:\n%s", out)
	}
	if !strings.Contains(out, "org-stranger") {
		t.Errorf("refusal does not name the organization it is about:\n%s", out)
	}
}

// TestRepoSettingsOrganization_LoadIsChainedOffTheSettingsLoad covers AC#7/#8:
// Init issues exactly the repo load, and the organization fetch hangs off the
// resulting message rather than opening a second lifecycle of its own. Nothing
// in Update touches the network directly.
func TestRepoSettingsOrganization_LoadIsChainedOffTheSettingsLoad(t *testing.T) {
	org := &fakeOrgClient{orgs: []*pb.Organization{{Id: "org-acme", Name: "Acme"}}}
	stub := &stubRepoClient{repos: []*pb.Repo{{
		Id:          "repo-1",
		DisplayName: "Test Repo",
		OriginUrl:   testRepoOrigin,
	}}}
	m := NewRepoSettingsModel(stub, context.Background(), "repo-1")
	m.SetRepoOrganizationClient(org)

	// Init alone must not fetch organizations: the origin URL is not known yet.
	initMsg := m.Init()()
	if org.listCalls != 0 || org.getCalls != 0 {
		t.Fatalf("Init fetched organizations before the repo loaded (list=%d get=%d)", org.listCalls, org.getCalls)
	}

	updated, cmd := m.Update(initMsg)
	m = updated.(RepoSettingsModel)
	// Handling the message must not itself have blocked on the network.
	if org.listCalls != 0 {
		t.Fatal("Update issued the organization RPC inline; it must return a command instead")
	}
	for _, msg := range drainCmd(cmd) {
		updated, _ = m.Update(msg)
		m = updated.(RepoSettingsModel)
	}
	if org.listCalls != 1 || org.getCalls != 1 {
		t.Fatalf("organization load ran %d list / %d get, want 1 each", org.listCalls, org.getCalls)
	}
}

// TestRepoSettingsOrganization_ListFailureLeavesTheViewUsable pins the failure
// shape: a ListOrganizations error is reported on the field, and the rest of the
// settings view keeps working. A failed list skips the mapping read entirely, so
// the current value is unknown for the same reason a failed mapping read is —
// naming the Personal default here would assert an unmapped repo nobody looked
// at.
func TestRepoSettingsOrganization_ListFailureLeavesTheViewUsable(t *testing.T) {
	org := &fakeOrgClient{listErr: errors.New("boom")}
	m := newLoadedOrgSettings(t, org)

	out := orgView(t, m)
	if !strings.Contains(out, "Could not load organizations") {
		t.Errorf("load failure is not surfaced:\n%s", out)
	}
	if strings.Contains(out, "Organization: "+repoSettingsNoOrgLabel) {
		t.Errorf("an unread mapping is rendered as the Personal default:\n%s", out)
	}
	if !strings.Contains(out, "Organization: Unknown") {
		t.Errorf("field lost its value after a load failure:\n%s", out)
	}
	if !strings.Contains(out, "Merge strategy:") {
		t.Errorf("view stopped rendering its other rows after a load failure:\n%s", out)
	}
}

// TestRepoSettingsOrganization_PickerKeepsTheReadFailureNotice pins the notice
// on the screen the user opened in order to act on the field.
//
// A failed read leaves the picker holding nothing but the Personal row, which is
// indistinguishable from belonging to no organizations. The field row explains
// the difference; before the two shared organizationNotices() the overlay drew
// only orgRefusal, so opening the picker replaced the diagnostic with a
// single-choice list and no reason for it — the field row's stated rule that one
// notice must not shadow another, broken by the overlay one keystroke later.
func TestRepoSettingsOrganization_PickerKeepsTheReadFailureNotice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		org    *fakeOrgClient
		notice string
	}{
		{
			name:   "organization list failed",
			org:    &fakeOrgClient{listErr: errors.New("boom")},
			notice: "Could not load organizations",
		},
		{
			name:   "mapping read failed",
			org:    &fakeOrgClient{orgs: []*pb.Organization{{Id: "org-acme", Name: "Acme"}}, getErr: errors.New("boom")},
			notice: "Could not read this repo's organization",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newLoadedOrgSettings(t, tc.org)

			if field := orgView(t, m); !strings.Contains(field, tc.notice) {
				t.Fatalf("the field row does not carry %q, so the picker comparison below proves nothing:\n%s", tc.notice, field)
			}

			cursorToRow(t, &m, repoSettingsRowOrganization)
			updated, _ := m.activateRow()
			m = updated.(RepoSettingsModel)
			if !m.orgPickerOpen {
				t.Fatal("activating the organization row did not open the picker")
			}

			out := orgView(t, m)
			if !strings.Contains(out, "[enter] select") {
				t.Fatalf("the picker overlay is not what was rendered:\n%s", out)
			}
			if !strings.Contains(out, tc.notice) {
				t.Errorf("opening the picker dropped %q, leaving a one-row picker with no reason:\n%s", tc.notice, out)
			}
		})
	}
}

// appOrgCloudAccess satisfies both CloudAccessClient and RepoOrganizationClient
// so the routing gate has something real to assert against.
type appOrgCloudAccess struct {
	fakeOrgClient
}

func (c *appOrgCloudAccess) GetCloudAccessStatus(context.Context) (*pb.CloudAccessStatus, error) {
	return &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE}, nil
}

func (c *appOrgCloudAccess) CreateCheckoutSession(context.Context, string, string) (string, error) {
	return "", nil
}

func (c *appOrgCloudAccess) CreateBillingPortalSession(context.Context, string) (string, error) {
	return "", nil
}

func (c *appOrgCloudAccess) RefreshCloudEntitlements(context.Context) (*pb.CloudAccessStatus, error) {
	return &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE}, nil
}

// TestEnterRepoSettingsGatesOrganizationOnSignIn covers AC#5 at the seam that
// actually decides it. The cloud access client exists for anyone with an auth
// manager configured — signed in or not — so the type assertion alone would grow
// an organization row for a signed-out user that could only ever report "not
// logged in". The routing gate is the cached sign-in state, not the assertion.
func TestEnterRepoSettingsGatesOrganizationOnSignIn(t *testing.T) {
	msg := switchViewMsg{view: ViewRepoSettings, sessionID: "repo-1"}

	signedOut := NewApp(nil, nil)
	signedOut.cloudAccess = &appOrgCloudAccess{}
	signedOut.home.loggedIn = false
	signedOut.enterRepoSettings(msg)
	if signedOut.repoSettings.orgClient != nil {
		t.Error("a signed-out user was given an organization client")
	}

	signedIn := NewApp(nil, nil)
	signedIn.cloudAccess = &appOrgCloudAccess{}
	signedIn.home.loggedIn = true
	signedIn.enterRepoSettings(msg)
	if signedIn.repoSettings.orgClient == nil {
		t.Error("a signed-in user was not given an organization client")
	}
}

// TestRepoSettingsOrganization_RefusalWrapsToTerminalWidth pins that the
// membership refusal wraps instead of running off the terminal edge. The
// sentence names the organization and what the mapping costs the user, so at a
// realistic width it is longer than one line — and a status line the terminal
// cuts mid-word is one the user cannot act on (the BOS-507 defect, on Home).
func TestRepoSettingsOrganization_RefusalWrapsToTerminalWidth(t *testing.T) {
	const cols = 100

	org := &fakeOrgClient{
		orgs:    []*pb.Organization{{Id: "org-acme", Name: "Acme Corp"}},
		mapping: &pb.RepoOrganizationMapping{RepoOriginUrl: testRepoOrigin, OrganizationId: "org-ghost"},
	}
	m := newLoadedOrgSettings(t, org)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: 40})
	m = updated.(RepoSettingsModel)

	out := orgView(t, m)
	if !strings.Contains(out, repoSettingsNotAMemberPrefix) {
		t.Fatalf("refusal missing from view:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if lipgloss.Width(line) > cols {
			t.Errorf("line is %d columns wide, wider than the %d-column terminal: %q",
				lipgloss.Width(line), cols, line)
		}
	}

	// The wrap has to be a wrap, not a truncation: the sentence's tail must
	// still be on screen somewhere.
	if !strings.Contains(out, "org-ghost") {
		t.Errorf("refusal does not name the organization it is about:\n%s", out)
	}
}
