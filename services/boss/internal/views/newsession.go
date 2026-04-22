package views

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// issueSearchDebounce is the wait between the last filter keystroke and the
// debounced server-side search. ~250ms is the sweet spot — slow enough that
// fast typists only fire one request per word, fast enough that pausing feels
// instant.
const issueSearchDebounce = 250 * time.Millisecond

// sessionType identifies the kind of session to create.
type sessionType int

const (
	sessionTypeQuickChat    sessionType = iota // Quick chat in base folder
	sessionTypeNewPR                           // Create a new PR
	sessionTypeExistingPR                      // Work on an existing PR
	sessionTypeExecutePlan                     // Execute a plan (placeholder)
	sessionTypeLinearTicket                    // Work on a Linear issue
)

// newSessionPhase tracks the current phase of the wizard.
type newSessionPhase int

const (
	newSessionPhaseLoading     newSessionPhase = iota // Fetching repos
	newSessionPhaseRepoSelect                         // Table-based repo picker
	newSessionPhaseTypeSelect                         // Table-based session type picker
	newSessionPhasePRSelect                           // Table-based PR picker
	newSessionPhaseIssueSelect                        // Table-based issue picker (Linear)
	newSessionPhaseForm                               // Main huh form active
	newSessionPhaseCreating                           // Waiting for CreateSession RPC
	newSessionPhaseDone                               // Terminal
)

// reposMsg carries the result of a ListRepos RPC call.
type reposMsg struct {
	repos []*pb.Repo
	err   error
}

// prsMsg carries the result of a ListRepoPRs RPC call.
type prsMsg struct {
	prs []*pb.PRSummary
	err error
}

// issuesMsg carries the result of a ListTrackerIssues RPC call. seq is the
// monotonic sequence number in effect when the fetch was issued; the handler
// drops the response when it no longer matches m.issueSearchSeq (meaning the
// user typed further or navigated away). query is still used to distinguish
// the initial unfiltered load from an empty search result.
type issuesMsg struct {
	issues []*pb.TrackerIssue
	err    error
	seq    uint64
	query  string
}

// searchIssuesTickMsg fires after the debounce window elapses. The seq field
// is a monotonic counter incremented on every keystroke that changes the
// query — when the tick fires we ignore it unless seq is still the latest, so
// a burst of keystrokes only triggers one search at the end.
type searchIssuesTickMsg struct {
	seq   uint64
	query string
}

// createSessionStreamMsg carries the opened stream or error.
type createSessionStreamMsg struct {
	stream client.CreateSessionStream
	err    error
}

// setupScriptLineMsg carries a single line of setup script output.
type setupScriptLineMsg struct {
	text string
}

// streamSessionCreatedMsg carries the final session from the stream.
type streamSessionCreatedMsg struct {
	session *pb.Session
}

// streamErrorMsg carries an error from the stream.
type streamErrorMsg struct {
	err error
}

// sessionTypeOption defines a row in the session-type selection table.
type sessionTypeOption struct {
	label string
	desc  string
	typ   sessionType
}

// buildSessionTypeOptions returns available session types based on repo configuration.
func (m *NewSessionModel) buildSessionTypeOptions() []sessionTypeOption {
	opts := []sessionTypeOption{
		{"Create a new PR", "Start a fresh branch and pull request", sessionTypeNewPR},
		{"Work on an existing PR", "Attach to an open pull request", sessionTypeExistingPR},
		{"Quick chat", "Work directly in the repo's base folder", sessionTypeQuickChat},
	}

	// Add Linear issue option if repo has Linear API key configured
	repo := m.selectedRepo()
	if repo != nil && repo.LinearApiKey != "" {
		// Insert before Quick chat
		opts = append(opts[:2], append([]sessionTypeOption{
			{"Work on a Linear issue", "Pick an issue from your Linear board", sessionTypeLinearTicket},
		}, opts[2:]...)...)
	}

	return opts
}

// formData holds huh form-bound values on the heap so that Value() pointers
// remain valid across bubbletea value-receiver copies of NewSessionModel.
type formData struct {
	title string
}

// NewSessionModel is the multi-step wizard for creating a new coding session.
type NewSessionModel struct {
	client client.BossClient
	ctx    context.Context

	phase  newSessionPhase
	err    error
	done   bool
	cancel bool

	// Data
	repos []*pb.Repo
	prs   []*pb.PRSummary

	// Linear issues
	trackerIssues   []*pb.TrackerIssue
	issueTable      table.Model
	issueTableReady bool
	selectedIssue   *pb.TrackerIssue

	// Live server-side search state. issueSearchSeq is incremented on every
	// keystroke that changes the filter query; debounce ticks and in-flight
	// fetches carry the seq they were issued with so stale responses are
	// dropped. issueSearchQuery is the query that the currently displayed
	// trackerIssues was fetched with — used both for stale-response rejection
	// and to know whether a backend refetch is needed on Esc.
	issueSearchSeq   uint64
	issueSearchQuery string
	issuesFetching   bool

	// Filters — indices into m.prs / m.trackerIssues that match the current query.
	prFilter       listFilter
	issueFilter    listFilter
	prsFiltered    []int
	issuesFiltered []int

	// Form-bound values (heap-allocated for stable pointers)
	selectedRepoID string
	selectedType   sessionType
	fd             *formData

	// Async / conflict state
	createdSess         *pb.Session
	forceBranch         bool
	confirmingOverwrite bool

	// Streaming create session
	createStream client.CreateSessionStream
	setupLines   []string

	// Tables
	repoTable table.Model
	typeTable table.Model
	prTable   table.Model

	// Form
	form *huh.Form

	// Layout
	width  int
	height int
}

// NewNewSessionModel creates a NewSessionModel wired to the daemon client.
func NewNewSessionModel(c client.BossClient, ctx context.Context) NewSessionModel {
	return NewSessionModel{
		client:      c,
		ctx:         ctx,
		phase:       newSessionPhaseLoading,
		prFilter:    newListFilter(),
		issueFilter: newListFilter(),
	}
}

func (m NewSessionModel) Init() tea.Cmd {
	return fetchRepos(m.client, m.ctx)
}

func fetchRepos(c client.BossClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		repos, err := c.ListRepos(ctx)
		return reposMsg{repos: repos, err: err}
	}
}

func fetchPRs(c client.BossClient, ctx context.Context, repoID string) tea.Cmd {
	return func() tea.Msg {
		prs, err := c.ListRepoPRs(ctx, repoID)
		return prsMsg{prs: prs, err: err}
	}
}

func fetchIssues(c client.BossClient, ctx context.Context, repoID, query string, seq uint64) tea.Cmd {
	return func() tea.Msg {
		issues, err := c.ListTrackerIssues(ctx, repoID, query)
		return issuesMsg{issues: issues, err: err, seq: seq, query: query}
	}
}

// scheduleIssueSearch schedules a debounced server-side search. The returned
// command emits a searchIssuesTickMsg after issueSearchDebounce; the handler
// for that message ignores it if a newer keystroke has incremented seq.
func scheduleIssueSearch(seq uint64, query string) tea.Cmd {
	return tea.Tick(issueSearchDebounce, func(time.Time) tea.Msg {
		return searchIssuesTickMsg{seq: seq, query: query}
	})
}

func openCreateStream(c client.BossClient, ctx context.Context, req *pb.CreateSessionRequest) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.CreateSession(ctx, req)
		return createSessionStreamMsg{stream: stream, err: err}
	}
}

func readNextStreamMsg(stream client.CreateSessionStream) tea.Cmd {
	return func() tea.Msg {
		// Close the stream on any terminal path (error, EOF, SessionCreated,
		// unknown event). SetupOutput is the only non-terminal case, where the
		// caller will schedule another readNextStreamMsg and the stream must stay
		// open.
		if !stream.Receive() {
			_ = stream.Close()
			if err := stream.Err(); err != nil {
				return streamErrorMsg{err: err}
			}
			return streamErrorMsg{err: fmt.Errorf("stream ended unexpectedly")}
		}
		msg := stream.Msg()
		switch e := msg.Event.(type) {
		case *pb.CreateSessionResponse_SetupOutput:
			return setupScriptLineMsg{text: e.SetupOutput.GetText()}
		case *pb.CreateSessionResponse_SessionCreated:
			_ = stream.Close()
			return streamSessionCreatedMsg{session: e.SessionCreated.GetSession()}
		default:
			_ = stream.Close()
			return streamErrorMsg{err: fmt.Errorf("unexpected stream event")}
		}
	}
}

func (m *NewSessionModel) buildRepoTable() {
	names := make([]string, len(m.repos))
	paths := make([]string, len(m.repos))
	for i, r := range m.repos {
		names[i] = r.DisplayName
		paths[i] = r.LocalPath
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "NAME", Width: maxColWidth("NAME", names, 30) + tableColumnSep},
		{Title: "PATH", Width: maxColWidth("PATH", paths, 60) + tableColumnSep},
	}

	rows := make([]table.Row, len(m.repos))
	for i := range m.repos {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, names[i], paths[i]}
	}

	m.repoTable = newBossTable(cols, rows, m.repoTableHeight())
	m.repoTable.SetWidth(columnsWidth(cols))
}

// repoTableHeight returns the height for the repo selection table.
func (m NewSessionModel) repoTableHeight() int {
	return clampedTableHeight(len(m.repos), m.height, bannerOverhead+6) // header + gaps + action bar
}

func (m *NewSessionModel) buildTypeTable() {
	cols := []table.Column{
		cursorColumn,
		{Title: "", Width: 24 + tableColumnSep},
		{Title: "", Width: 46 + tableColumnSep},
	}
	opts := m.buildSessionTypeOptions()
	rows := make([]table.Row, len(opts))
	for i, opt := range opts {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, opt.label, styleSubtle.Render(opt.desc)}
	}
	m.typeTable = newBossTable(cols, rows, len(opts)+1)
	m.typeTable.SetWidth(columnsWidth(cols))
}

// applyPRFilter rebuilds m.prsFiltered based on the current prFilter query.
func (m *NewSessionModel) applyPRFilter() {
	m.prsFiltered = m.prsFiltered[:0]
	for i, pr := range m.prs {
		hay := fmt.Sprintf("#%d %s %s", pr.Number, pr.Title, pr.HeadBranch)
		if m.prFilter.Matches(hay) {
			m.prsFiltered = append(m.prsFiltered, i)
		}
	}
	m.prFilter.SetCounts(len(m.prsFiltered), len(m.prs))
}

func (m *NewSessionModel) buildPRTable() {
	// Always re-apply the filter so m.prsFiltered reflects current m.prs.
	// Without this, stale indices from a previous fetch can point past the end
	// of a shorter new m.prs and panic in the row loop below.
	m.applyPRFilter()
	n := len(m.prsFiltered)
	numbers := make([]string, n)
	titles := make([]string, n)
	branches := make([]string, n)
	for j, i := range m.prsFiltered {
		pr := m.prs[i]
		numbers[j] = fmt.Sprintf("#%d", pr.Number)
		titles[j] = pr.Title
		branches[j] = pr.HeadBranch
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "PR", Width: maxColWidth("PR", numbers, 10) + tableColumnSep},
		{Title: "TITLE", Width: maxColWidth("TITLE", titles, 50) + tableColumnSep},
		{Title: "BRANCH", Width: maxColWidth("BRANCH", branches, 30) + tableColumnSep},
	}

	rows := make([]table.Row, n)
	for j := range m.prsFiltered {
		indicator := ""
		if j == 0 {
			indicator = cursorChevron
		}
		rows[j] = table.Row{indicator, numbers[j], titles[j], styleSubtle.Render(branches[j])}
	}

	m.prTable = newBossTable(cols, rows, m.prTableHeight())
	m.prTable.SetWidth(columnsWidth(cols))
}

// prTableHeight returns the height for the PR selection table.
func (m NewSessionModel) prTableHeight() int {
	return clampedTableHeight(len(m.prsFiltered), m.height, bannerOverhead+6+m.prFilter.Height())
}

// applyIssueFilter rebuilds m.issuesFiltered based on the current issueFilter query.
func (m *NewSessionModel) applyIssueFilter() {
	m.issuesFiltered = m.issuesFiltered[:0]
	for i, issue := range m.trackerIssues {
		hay := issue.ExternalId + " " + issue.Title + " " + issue.State
		if m.issueFilter.Matches(hay) {
			m.issuesFiltered = append(m.issuesFiltered, i)
		}
	}
	m.issueFilter.SetCounts(len(m.issuesFiltered), len(m.trackerIssues))
}

func (m *NewSessionModel) buildIssueTable() {
	// Always re-apply the filter so m.issuesFiltered reflects current
	// m.trackerIssues. Without this, stale indices from a previous fetch can
	// point past the end of a shorter new m.trackerIssues and panic below.
	m.applyIssueFilter()
	n := len(m.issuesFiltered)
	ids := make([]string, n)
	titles := make([]string, n)
	states := make([]string, n)
	for j, i := range m.issuesFiltered {
		issue := m.trackerIssues[i]
		ids[j] = issue.ExternalId
		titles[j] = issue.Title
		states[j] = issue.State
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "ID", Width: maxColWidth("ID", ids, 10) + tableColumnSep},
		{Title: "TITLE", Width: maxColWidth("TITLE", titles, 50) + tableColumnSep},
		{Title: "STATE", Width: maxColWidth("STATE", states, 15) + tableColumnSep},
	}

	rows := make([]table.Row, n)
	for j := range m.issuesFiltered {
		indicator := ""
		if j == 0 {
			indicator = cursorChevron
		}
		rows[j] = table.Row{indicator, ids[j], titles[j], styleSubtle.Render(states[j])}
	}

	m.issueTable = newBossTable(cols, rows, m.issueTableHeight())
	m.issueTable.SetWidth(columnsWidth(cols))
}

// issueTableHeight returns the height for the issue selection table.
func (m NewSessionModel) issueTableHeight() int {
	return clampedTableHeight(len(m.issuesFiltered), m.height, bannerOverhead+6+m.issueFilter.Height())
}

func (m *NewSessionModel) buildForm() {
	if m.fd == nil {
		m.fd = &formData{}
	}

	switch m.selectedType {
	case sessionTypeNewPR:
		m.form = huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Session title").
					Placeholder("What are you working on?").
					Value(&m.fd.title).
					Validate(func(s string) error {
						if strings.TrimSpace(s) == "" {
							return fmt.Errorf("title is required")
						}
						return nil
					}),
			),
		).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70)

	case sessionTypeQuickChat, sessionTypeExistingPR, sessionTypeExecutePlan, sessionTypeLinearTicket:
		// No form needed for these types.
	}
}

func (m NewSessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.phase == newSessionPhaseRepoSelect {
			m.repoTable.SetHeight(m.repoTableHeight())
			m.repoTable.SetWidth(msg.Width)
		}
		if m.phase == newSessionPhasePRSelect {
			m.prTable.SetHeight(m.prTableHeight())
			m.prTable.SetWidth(msg.Width)
		}
		if m.phase == newSessionPhaseIssueSelect {
			m.issueTable.SetHeight(m.issueTableHeight())
			m.issueTable.SetWidth(msg.Width)
		}
		return m, nil

	case reposMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.repos = msg.repos
		slices.SortFunc(m.repos, func(a, b *pb.Repo) int {
			return strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
		})
		if len(m.repos) == 1 {
			m.selectedRepoID = m.repos[0].Id
			m.phase = newSessionPhaseTypeSelect
			m.buildTypeTable()
			return m, nil
		}
		m.phase = newSessionPhaseRepoSelect
		m.buildRepoTable()
		return m, nil

	case prsMsg:
		m.prs = msg.prs
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		if len(m.prs) == 0 {
			m.err = fmt.Errorf("no open PRs found")
			return m, nil
		}
		m.phase = newSessionPhasePRSelect
		m.buildPRTable()
		return m, nil

	case issuesMsg:
		// Drop stale responses: a newer search may have been issued, or the
		// user may have navigated away from the issue flow, since this fetch
		// was started. Keying off seq (rather than query) closes the window
		// where m.issueSearchQuery has not yet caught up with the latest
		// keystroke — the debounce tick only updates it when it fires.
		if msg.seq != m.issueSearchSeq {
			return m, nil
		}
		m.issuesFetching = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.trackerIssues = msg.issues
		// An empty result is fatal only on the very first (unfiltered) load —
		// after that we may legitimately be showing "no matches for <query>",
		// which the table renders fine on its own.
		if len(m.trackerIssues) == 0 && !m.issueTableReady && msg.query == "" {
			m.err = fmt.Errorf("no issues found in Linear")
			return m, nil
		}
		m.phase = newSessionPhaseIssueSelect
		m.buildIssueTable()
		m.issueTable.SetCursor(0)
		updateCursorColumn(&m.issueTable)
		m.issueTableReady = true
		return m, nil

	case searchIssuesTickMsg:
		// Ignore stale ticks — a newer keystroke has superseded this one.
		if msg.seq != m.issueSearchSeq {
			return m, nil
		}
		m.issueSearchQuery = msg.query
		return m, fetchIssues(m.client, m.ctx, m.selectedRepoID, msg.query, msg.seq)

	case createSessionStreamMsg:
		if msg.err != nil {
			var connectErr *connect.Error
			if errors.As(msg.err, &connectErr) && connectErr.Code() == connect.CodeAlreadyExists {
				m.confirmingOverwrite = true
				m.phase = newSessionPhaseForm
				m.err = nil
				return m, nil
			}
			m.err = msg.err
			m.phase = newSessionPhaseForm
			return m, nil
		}
		m.createStream = msg.stream
		return m, readNextStreamMsg(m.createStream)

	case setupScriptLineMsg:
		m.setupLines = append(m.setupLines, msg.text)
		return m, readNextStreamMsg(m.createStream)

	case streamSessionCreatedMsg:
		// readNextStreamMsg closes the stream on terminal events.
		m.createdSess = msg.session
		m.done = true
		return m, nil

	case streamErrorMsg:
		// readNextStreamMsg closes the stream on terminal events.
		var connectErr *connect.Error
		if errors.As(msg.err, &connectErr) && connectErr.Code() == connect.CodeAlreadyExists {
			m.confirmingOverwrite = true
			m.phase = newSessionPhaseForm
			m.err = nil
			return m, nil
		}
		m.err = msg.err
		m.phase = newSessionPhaseForm
		return m, nil

	case tea.KeyMsg:
		if m.confirmingOverwrite {
			return m.updateConfirmOverwrite(msg)
		}

		if m.phase == newSessionPhaseRepoSelect {
			switch msg.String() {
			case "esc":
				m.cancel = true
				return m, nil
			case "enter":
				idx := m.repoTable.Cursor()
				if idx >= 0 && idx < len(m.repos) {
					m.selectedRepoID = m.repos[idx].Id
					m.phase = newSessionPhaseTypeSelect
					m.buildTypeTable()
				}
				return m, nil
			}

			var cmd tea.Cmd
			m.repoTable, cmd = m.repoTable.Update(msg)
			updateCursorColumn(&m.repoTable)
			return m, cmd
		}

		if m.phase == newSessionPhaseTypeSelect {
			switch msg.String() {
			case "esc":
				// Go back to repo select if multiple repos, otherwise cancel.
				if len(m.repos) > 1 {
					m.phase = newSessionPhaseRepoSelect
					return m, nil
				}
				m.cancel = true
				return m, nil
			case "enter":
				idx := m.typeTable.Cursor()
				opts := m.buildSessionTypeOptions()
				m.selectedType = opts[idx].typ
				return m.advanceFromTypeSelect()
			}

			var cmd tea.Cmd
			m.typeTable, cmd = m.typeTable.Update(msg)
			updateCursorColumn(&m.typeTable)
			return m, cmd
		}

		if m.phase == newSessionPhasePRSelect {
			// While the filter input is focused, route keys through it (with
			// special handling for commit/clear/navigation).
			if m.prFilter.Active() {
				switch msg.String() {
				case "enter":
					if !m.prFilter.Commit() {
						m.prFilter.Deactivate()
						m.buildPRTable()
					}
					return m, nil
				case "esc":
					m.prFilter.Deactivate()
					m.buildPRTable()
					m.prTable.SetCursor(0)
					updateCursorColumn(&m.prTable)
					return m, nil
				case "up", "down", "ctrl+p", "ctrl+n", "ctrl+d", "ctrl+u":
					var cmd tea.Cmd
					m.prTable, cmd = m.prTable.Update(msg)
					updateCursorColumn(&m.prTable)
					return m, cmd
				}
				prev := m.prFilter.Query()
				cmd := m.prFilter.Update(msg)
				if m.prFilter.Query() != prev {
					m.buildPRTable()
					m.prTable.SetCursor(0)
					updateCursorColumn(&m.prTable)
				}
				return m, cmd
			}

			switch msg.String() {
			case "/":
				cmd := m.prFilter.Activate()
				// Activate transitions the filter line from hidden (Height=0)
				// to visible (Height=1); rebuild the table so its height is
				// recomputed before the next render.
				m.buildPRTable()
				return m, cmd
			case "esc":
				if m.prFilter.Applied() {
					m.prFilter.Deactivate()
					m.buildPRTable()
					m.prTable.SetCursor(0)
					updateCursorColumn(&m.prTable)
					return m, nil
				}
				m.phase = newSessionPhaseTypeSelect
				m.forceBranch = false
				return m, nil
			case "enter":
				idx := m.prTable.Cursor()
				if idx >= 0 && idx < len(m.prsFiltered) {
					return m, m.startCreating()
				}
				return m, nil
			}

			var cmd tea.Cmd
			m.prTable, cmd = m.prTable.Update(msg)
			updateCursorColumn(&m.prTable)
			return m, cmd
		}

		if m.phase == newSessionPhaseIssueSelect {
			if m.issueFilter.Active() {
				switch msg.String() {
				case "enter":
					// Live debounced search means there is nothing to "commit"
					// — pressing Enter selects the highlighted row, mirroring
					// how Enter behaves once the filter input is blurred.
					idx := m.issueTable.Cursor()
					if idx >= 0 && idx < len(m.issuesFiltered) {
						m.selectedIssue = m.trackerIssues[m.issuesFiltered[idx]]
						return m, m.startCreating()
					}
					return m, nil
				case "esc":
					// Clear the filter and refetch the unfiltered list. The
					// seq bump invalidates any in-flight tick or fetch.
					m.issueFilter.Deactivate()
					m.issueSearchSeq++
					m.issueSearchQuery = ""
					m.issuesFetching = true
					m.buildIssueTable()
					return m, fetchIssues(m.client, m.ctx, m.selectedRepoID, "", m.issueSearchSeq)
				case "up", "down", "ctrl+p", "ctrl+n", "ctrl+d", "ctrl+u":
					var cmd tea.Cmd
					m.issueTable, cmd = m.issueTable.Update(msg)
					updateCursorColumn(&m.issueTable)
					return m, cmd
				}
				prev := m.issueFilter.Query()
				cmd := m.issueFilter.Update(msg)
				if m.issueFilter.Query() != prev {
					// Local rebuild gives instant feedback against the cached
					// rows; the debounced tick will issue the real search.
					m.buildIssueTable()
					m.issueTable.SetCursor(0)
					updateCursorColumn(&m.issueTable)
					m.issueSearchSeq++
					m.issuesFetching = true
					return m, tea.Batch(cmd, scheduleIssueSearch(m.issueSearchSeq, m.issueFilter.Query()))
				}
				return m, cmd
			}

			switch msg.String() {
			case "/":
				cmd := m.issueFilter.Activate()
				// See the matching comment on the PR filter branch above.
				m.buildIssueTable()
				return m, cmd
			case "esc":
				// Bumping seq here invalidates any in-flight fetch whose
				// response would otherwise snap the user back to issue select.
				m.issueSearchSeq++
				m.phase = newSessionPhaseTypeSelect
				m.forceBranch = false
				return m, nil
			case "enter":
				idx := m.issueTable.Cursor()
				if idx >= 0 && idx < len(m.issuesFiltered) {
					m.selectedIssue = m.trackerIssues[m.issuesFiltered[idx]]
					return m, m.startCreating()
				}
				return m, nil
			}

			var cmd tea.Cmd
			m.issueTable, cmd = m.issueTable.Update(msg)
			updateCursorColumn(&m.issueTable)
			return m, cmd
		}

		switch msg.String() {
		case "esc":
			if m.phase == newSessionPhaseForm {
				m.phase = newSessionPhaseTypeSelect
				m.form = nil
				m.err = nil
				m.fd = nil
				m.forceBranch = false
				return m, nil
			}
			m.cancel = true
			return m, nil
		}
	}

	// Delegate to form.
	if m.form != nil && m.phase == newSessionPhaseForm && !m.confirmingOverwrite {
		_, cmd := m.form.Update(msg)

		if m.form.State == huh.StateAborted {
			m.phase = newSessionPhaseTypeSelect
			m.form = nil
			m.err = nil
			m.fd = nil
			m.forceBranch = false
			return m, nil
		}

		if m.form.State == huh.StateCompleted {
			return m.handleFormCompleted()
		}

		return m, cmd
	}

	return m, nil
}

func (m *NewSessionModel) advanceFromTypeSelect() (tea.Model, tea.Cmd) {
	switch m.selectedType {
	case sessionTypeQuickChat:
		// No form needed — create session directly.
		return *m, m.startCreating()
	case sessionTypeExistingPR:
		// Fetch PRs, then show PR selector table.
		m.phase = newSessionPhaseLoading
		return *m, fetchPRs(m.client, m.ctx, m.selectedRepoID)
	case sessionTypeLinearTicket:
		// Fetch Linear issues, then show issue selector table.
		m.phase = newSessionPhaseLoading
		m.issueSearchQuery = ""
		return *m, fetchIssues(m.client, m.ctx, m.selectedRepoID, "", m.issueSearchSeq)
	case sessionTypeNewPR:
		m.phase = newSessionPhaseForm
		m.buildForm()
		return *m, m.form.Init()
	default:
		m.cancel = true
		return *m, nil
	}
}

func (m *NewSessionModel) handleFormCompleted() (tea.Model, tea.Cmd) {
	// Title input or plan input completed — proceed to create.
	return *m, m.startCreating()
}

func (m NewSessionModel) updateConfirmOverwrite(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.confirmingOverwrite = false
		m.forceBranch = true
		return m, m.startCreating()
	case "n", "N", "esc":
		m.confirmingOverwrite = false
		m.forceBranch = false
		m.err = nil
		switch m.selectedType {
		case sessionTypeLinearTicket:
			m.phase = newSessionPhaseIssueSelect
			return m, nil
		case sessionTypeExistingPR:
			m.phase = newSessionPhasePRSelect
			return m, nil
		default:
			m.phase = newSessionPhaseForm
			m.buildForm()
			return m, m.form.Init()
		}
	}
	return m, nil
}

func (m *NewSessionModel) selectedRepo() *pb.Repo {
	for _, r := range m.repos {
		if r.Id == m.selectedRepoID {
			return r
		}
	}
	return nil
}

// startCreating builds a CreateSessionRequest and fires the RPC.
func (m *NewSessionModel) startCreating() tea.Cmd {
	m.phase = newSessionPhaseCreating
	m.setupLines = nil // Clear setup output from any previous attempt
	repo := m.selectedRepo()
	if repo == nil {
		m.err = fmt.Errorf("no repository selected")
		return nil
	}

	req := &pb.CreateSessionRequest{
		RepoId:      repo.Id,
		BaseBranch:  repo.DefaultBaseBranch,
		ForceBranch: m.forceBranch,
	}

	switch m.selectedType {
	case sessionTypeQuickChat:
		req.Title = "Quick chat"
		req.QuickChat = true
	case sessionTypeNewPR:
		req.Title = m.fd.title
	case sessionTypeExistingPR:
		idx := m.prTable.Cursor()
		if idx >= 0 && idx < len(m.prsFiltered) {
			pr := m.prs[m.prsFiltered[idx]]
			req.Title = pr.Title
			req.PrNumber = &pr.Number
		}
	case sessionTypeLinearTicket:
		if m.selectedIssue != nil {
			issue := m.selectedIssue
			req.Title = fmt.Sprintf("[%s] %s", issue.ExternalId, issue.Title)
			req.Plan = formatLinearPrompt(issue)
			req.TrackerId = &issue.ExternalId
			if issue.Url != "" {
				req.TrackerUrl = &issue.Url
			}
			if issue.PrNumber > 0 {
				// Existing PR - attach to it
				prNum := issue.PrNumber
				req.PrNumber = &prNum
			} else if issue.BranchName != "" {
				// New branch using Linear's suggested name
				req.BranchName = &issue.BranchName
			}
		}
	default:
		req.Title = "New session"
	}

	return openCreateStream(m.client, m.ctx, req)
}

// formatLinearPrompt renders a Linear tracker issue as a labeled block used
// for the session plan. The returned string flows into the draft PR body
// and the first prompt shown to Claude, so it needs to stand alone as
// both a human-readable PR description and a usable starting prompt.
//
// Fields missing on the issue are individually omitted so we never render
// blank lines for absent data.
func formatLinearPrompt(issue *pb.TrackerIssue) string {
	if issue == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Linear issue:\n\n")
	header := issue.Title
	if issue.ExternalId != "" {
		if header != "" {
			header = fmt.Sprintf("[%s] %s", issue.ExternalId, issue.Title)
		} else {
			header = fmt.Sprintf("[%s]", issue.ExternalId)
		}
	}
	if header != "" {
		b.WriteString(header)
		b.WriteString("\n")
	}
	if desc := strings.TrimSpace(issue.Description); desc != "" {
		b.WriteString("\n")
		b.WriteString(desc)
		b.WriteString("\n")
	}
	if issue.Url != "" {
		b.WriteString("\n")
		b.WriteString(issue.Url)
		b.WriteString("\n")
	}
	return b.String()
}

// Cancelled returns true if the user cancelled the wizard.
func (m NewSessionModel) Cancelled() bool { return m.cancel }

// Done returns true if session creation succeeded.
func (m NewSessionModel) Done() bool { return m.done }

// CreatedSession returns the session created by the wizard, or nil.
func (m NewSessionModel) CreatedSession() *pb.Session { return m.createdSess }

func (m NewSessionModel) View() tea.View {
	if m.err != nil && !m.confirmingOverwrite {
		var b strings.Builder
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		if len(m.setupLines) > 0 {
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Foreground(colorWarning).Render("Setup script output:"))
			b.WriteString("\n")
			for _, line := range m.setupLines {
				b.WriteString(lipgloss.NewStyle().PaddingLeft(4).Foreground(lipgloss.Color("8")).Render(line))
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhaseLoading {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Render("Loading..."),
		)
	}

	if m.phase == newSessionPhaseRepoSelect {
		var b strings.Builder
		b.WriteString(styleTitle.Render("Select a repository"))
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.repoTable.View()))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[enter] select"}, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhaseTypeSelect {
		var b strings.Builder
		b.WriteString(m.headerView())
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.typeTable.View()))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[enter] select"}, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhasePRSelect {
		var b strings.Builder
		b.WriteString(m.headerView())
		if m.prFilter.Engaged() && len(m.prsFiltered) == 0 {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render("no matches"))
			b.WriteString("\n")
		} else {
			b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.prTable.View()))
			b.WriteString("\n")
		}
		if m.prFilter.Engaged() {
			b.WriteString(m.prFilter.View())
			b.WriteString("\n")
		}
		b.WriteString(prSelectActionBar(m.prFilter, len(m.prs) > 0))
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhaseIssueSelect {
		var b strings.Builder
		b.WriteString(m.headerView())
		if !m.issueTableReady {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Loading Linear issues..."))
		} else {
			if m.issueFilter.Engaged() && len(m.issuesFiltered) == 0 {
				placeholder := "no matches"
				if m.issuesFetching {
					placeholder = "searching…"
				}
				b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render(placeholder))
				b.WriteString("\n")
			} else {
				b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.issueTable.View()))
				b.WriteString("\n")
			}
			if m.issueFilter.Engaged() {
				b.WriteString(m.issueFilter.View())
				if m.issuesFetching && len(m.issuesFiltered) > 0 {
					b.WriteString(lipgloss.NewStyle().Foreground(colorMuted).Render("  searching…"))
				}
				b.WriteString("\n")
			}
			b.WriteString(prSelectActionBar(m.issueFilter, len(m.trackerIssues) > 0))
		}
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhaseCreating {
		var b strings.Builder
		if len(m.setupLines) > 0 {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Running setup script..."))
			b.WriteString("\n")
			// Show last 10 lines of setup output.
			start := 0
			if len(m.setupLines) > 10 {
				start = len(m.setupLines) - 10
			}
			for _, line := range m.setupLines[start:] {
				b.WriteString(lipgloss.NewStyle().PaddingLeft(4).Foreground(lipgloss.Color("8")).Render(line))
				b.WriteString("\n")
			}
		} else {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Creating a new session..."))
		}
		return tea.NewView(b.String())
	}

	if m.done && m.createdSess != nil {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Foreground(colorSuccess).Render("Session created!") + "\n" +
				lipgloss.NewStyle().Padding(0, 2).Render(
					fmt.Sprintf("  ID:     %s\n  Title:  %s\n  Branch: %s",
						m.createdSess.Id, m.createdSess.Title, m.createdSess.BranchName)),
		)
	}

	if m.confirmingOverwrite {
		var b strings.Builder
		b.WriteString(m.headerView())
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
			"A branch with this name already exists."))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			"Remove the old branch and create a new session?"))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
		return tea.NewView(b.String())
	}

	if m.form != nil {
		var b strings.Builder
		b.WriteString(m.headerView())
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(m.form.View()))
		return tea.NewView(b.String())
	}

	return tea.NewView("")
}

// prSelectActionBar renders the action bar for the filterable select phases
// (PR select and issue select). The bar adapts to the filter state:
//   - filtering (input focused): replaces the bar with the filter help.
//   - applied (query committed): offers to edit or clear the filter.
//   - idle: normal "[enter] select" plus discoverability hint for "/".
func prSelectActionBar(f listFilter, hasItems bool) string {
	if f.Active() {
		return actionBar(f.ActionBar())
	}
	if f.Applied() {
		return actionBar(
			[]string{"[enter] select"},
			[]string{"[/] edit filter", "[esc] clear"},
		)
	}
	primary := []string{"[enter] select"}
	if hasItems {
		primary = append(primary, "[/] filter")
	}
	return actionBar(primary, []string{"[esc] back"})
}

func (m NewSessionModel) headerView() string {
	repo := m.selectedRepo()
	if repo == nil {
		return ""
	}
	bold := lipgloss.NewStyle().Bold(true)
	return lipgloss.NewStyle().Padding(0, 2).Render(
		"Starting a "+bold.Render("new session")+" for "+bold.Render(repo.DisplayName)) + "\n"
}
