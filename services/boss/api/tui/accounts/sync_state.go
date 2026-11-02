package accounts

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SyncedRegistryRead is one read of the hosted synced-account registry,
// bound to a usable login.
type SyncedRegistryRead = func(context.Context) (*pb.ListSyncedAgentAccountsResponse, error)

// SyncedRegistryClient reads the hosted synced-account registry with the
// user's own login. The daemon cannot answer sync membership: an account's
// synced id names its registry row, but only the registry knows how many
// daemons are in that row and which rows look like the same account.
//
// DialSyncedRegistry makes the login usable, refreshing an expired one, and
// returns the read. It fails before any request is sent when the user is
// signed out.
type SyncedRegistryClient interface {
	DialSyncedRegistry(context.Context) (SyncedRegistryRead, error)
}

// syncedRegistryTimeout bounds the registry request, so a hung request leaves
// the SYNC cells reading unknown rather than pending forever. Refreshing an
// expired login is not bounded by it.
const syncedRegistryTimeout = 5 * time.Second

// syncedRegistryLoadedMsg carries the result of a registry read.
type syncedRegistryLoadedMsg struct {
	resp *pb.ListSyncedAgentAccountsResponse
	err  error
}

// syncRegistry is the registry snapshot shared by the accounts list and the
// account detail screen. Like the list's sessions snapshot it is best effort:
// known=false (no client, signed out or a failed read) shows an account with a
// synced id as unknown, never as synced.
type syncRegistry struct {
	client SyncedRegistryClient
	// webURL is the web app's Settings → Accounts page, where link
	// suggestions are resolved; empty when no web base is configured.
	webURL string

	loading bool
	known   bool
	// signIn records that the last read failed for want of a login, so the
	// detail screen can say sign in rather than registry unavailable.
	signIn bool
	resp   *pb.ListSyncedAgentAccountsResponse
}

func newSyncRegistry(c SyncedRegistryClient, webURL string) syncRegistry {
	return syncRegistry{client: c, webURL: webURL, loading: c != nil}
}

// fetchCmd reads the registry, or returns nil when there is no client.
func (r syncRegistry) fetchCmd(ctx context.Context) tea.Cmd {
	c := r.client
	if c == nil {
		return nil
	}
	return func() tea.Msg {
		// The dial runs outside the deadline: it may refresh an expired login, and
		// cancelling that after the provider rotates the single-use refresh token
		// would leave a consumed token in the keychain and force `boss login`.
		read, err := c.DialSyncedRegistry(ctx)
		if err != nil {
			return syncedRegistryLoadedMsg{err: err}
		}
		readCtx, cancel := context.WithTimeout(ctx, syncedRegistryTimeout)
		defer cancel()
		resp, err := read(readCtx)
		return syncedRegistryLoadedMsg{resp: resp, err: err}
	}
}

func (r *syncRegistry) apply(msg syncedRegistryLoadedMsg) {
	r.loading = false
	if msg.err != nil {
		r.known, r.resp, r.signIn = false, nil, registryNeedsSignIn(msg.err)
		return
	}
	r.known, r.resp, r.signIn = true, msg.resp, false
}

// registryNeedsSignIn reports whether a read failed for want of a login: the
// server refused the caller, or the dial gave up before sending a request (no
// stored token, or one flagged for a new sign-in), which leaves the error
// without an RPC classification. A dial that failed on the network, such as a
// login refresh that timed out, reads as unavailable: signing in again would
// not fix it.
func registryNeedsSignIn(err error) bool {
	var classified interface{ RPCKind() rpcerr.Kind }
	if errors.As(err, &classified) {
		return rpcerr.KindOf(err) == rpcerr.Unauthenticated
	}
	var netErr net.Error
	return !errors.As(err, &netErr) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)
}

func (r syncRegistry) resolve(a *pb.AgentAccount) display.AgentAccountSync {
	return display.ResolveAgentAccountSync(a, r.resp, r.known)
}

// pending reports whether the account's state waits on the in-flight read. An
// account with no synced id is local only whatever the registry says.
func (r syncRegistry) pending(a *pb.AgentAccount) bool {
	return r.loading && a.GetSyncedAgentAccountId() != ""
}

// cell is the list's SYNC cell for a.
func (r syncRegistry) cell(a *pb.AgentAccount) string {
	return accountSyncLabel(r.resolve(a), r.pending(a))
}

// accountSyncLabel renders a resolved sync state for the SYNC column, with a
// link marker when a pending suggestion involves the account.
func accountSyncLabel(s display.AgentAccountSync, pending bool) string {
	if pending {
		return "…"
	}
	label := "local only"
	switch s.State {
	case display.AgentAccountSyncSynced:
		label = fmt.Sprintf("synced (%d)", s.MemberCount)
	case display.AgentAccountSyncUnknown:
		label = "unknown"
	}
	if len(s.SuggestedPeers) > 0 {
		label += " · link?"
	}
	return label
}

// detailRows are the detail screen's Sync row and, when a suggestion applies,
// its Link suggestion row.
func (r syncRegistry) detailRows(a *pb.AgentAccount) []ui.KeyValue {
	if r.pending(a) {
		return []ui.KeyValue{{Key: "Sync", Value: "…"}}
	}
	s := r.resolve(a)
	rows := []ui.KeyValue{{Key: "Sync", Value: r.syncDetail(s)}}
	if len(s.SuggestedPeers) > 0 {
		rows = append(rows, ui.KeyValue{Key: "Link suggestion", Value: linkSuggestionDetail(s.SuggestedPeers, r.webURL)})
	}
	return rows
}

func (r syncRegistry) syncDetail(s display.AgentAccountSync) string {
	switch s.State {
	case display.AgentAccountSyncSynced:
		return fmt.Sprintf("Shared with %d daemons", s.MemberCount)
	case display.AgentAccountSyncUnknown:
		if r.client == nil || r.signIn {
			return "Unknown — sign in to Bossanova Cloud to see sync membership"
		}
		return "Unknown — registry unavailable"
	}
	return "This daemon only"
}

func linkSuggestionDetail(peers []display.SyncPeer, webURL string) string {
	names := make([]string, len(peers))
	for i, p := range peers {
		label := p.Label
		if label == "" {
			label = p.ID
		}
		names[i] = fmt.Sprintf("%q on %s", label, daemonCount(p.MemberCount))
	}
	return "looks like " + strings.Join(names, "; ") + " — resolve in " + accountsWebLocation(webURL)
}

func daemonCount(n int) string {
	if n == 1 {
		return "1 daemon"
	}
	return fmt.Sprintf("%d daemons", n)
}

// accountsWebLocation names where suggestions are resolved: the web page's
// URL when known, otherwise the page by name.
func accountsWebLocation(webURL string) string {
	if webURL == "" {
		return "Settings → Accounts in the web app"
	}
	return "Settings → Accounts at " + webURL
}

// suggestionCount counts the distinct suggestion pairs involving any listed
// account.
func (r syncRegistry) suggestionCount(accounts []*pb.AgentAccount) int {
	pairs := map[[2]string]bool{}
	for _, a := range accounts {
		own := a.GetSyncedAgentAccountId()
		for _, p := range r.resolve(a).SuggestedPeers {
			pairs[[2]string{min(own, p.ID), max(own, p.ID)}] = true
		}
	}
	return len(pairs)
}

// footerText is the list's link-suggestion hint, or "" when there is none.
func (r syncRegistry) footerText(accounts []*pb.AgentAccount) string {
	n := r.suggestionCount(accounts)
	if n == 0 {
		return ""
	}
	noun := "link suggestion"
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s — resolve in %s", n, noun, accountsWebLocation(r.webURL))
}

// renderSyncFooter renders the footer hint wrapped to width, or "" when there
// is no suggestion.
func (r syncRegistry) renderSyncFooter(accounts []*pb.AgentAccount, width int) string {
	text := r.footerText(accounts)
	if text == "" {
		return ""
	}
	style := lipgloss.NewStyle().Padding(0, 2)
	if width > style.GetHorizontalPadding() {
		style = style.Width(width)
	}
	return style.Render(ui.StyleStatusInfo.Render(text))
}

// SetSyncedRegistry gives the list the hosted registry client and the web
// Settings → Accounts URL. Without it, accounts with a synced id read unknown.
func (m *AgentAccountsListModel) SetSyncedRegistry(c SyncedRegistryClient, webAccountsURL string) {
	m.sync = newSyncRegistry(c, webAccountsURL)
}

// SetSyncedRegistry gives the detail screen the hosted registry client and the
// web Settings → Accounts URL. Without it, an account with a synced id reads
// unknown.
func (m *AgentAccountEditModel) SetSyncedRegistry(c SyncedRegistryClient, webAccountsURL string) {
	m.sync = newSyncRegistry(c, webAccountsURL)
}
