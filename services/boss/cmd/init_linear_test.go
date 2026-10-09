package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The fake Linear server speaks just enough of Linear's GraphQL shapes for the
// admin client. No test here reaches the network or reads LINEAR_API_KEY.

const (
	fakeLinearKey  = "lin_api_FAKE-KEY-MARKER"
	fakeBodyMarker = "FAKE-RESPONSE-BODY-MARKER"
	fakeTeamID     = "team-1"
)

type fakeLinearLabel struct {
	id, name, teamID string
	isGroup          bool
}

type fakeLinear struct {
	mu sync.Mutex

	viewerName, viewerEmail, org string
	viewerApp                    bool
	teams                        []linearTeam
	teamsHasNext                 bool
	states                       []linearState
	statesHasNext                bool
	labels                       []fakeLinearLabel
	createErrCode                string

	// failOp makes one operation ("teams", "viewer", "states", "labels",
	// "create") answer failStatus with a body carrying fakeBodyMarker.
	failOp     string
	failStatus int

	createCalls  int
	labelFilters []json.RawMessage
	createInputs []json.RawMessage
	auth         []string
	queries      []string
}

func newFakeLinear(states ...string) *fakeLinear {
	f := &fakeLinear{
		viewerName: "Ada", viewerEmail: "ada@acme.test", org: "Acme",
		teams: []linearTeam{{ID: fakeTeamID, Name: "Example", Key: "EX"}},
	}
	types := map[string]string{"Backlog": "backlog", "Todo": "unstarted", "In Progress": "started", "In Review": "started", "Done": "completed"}
	if len(states) == 0 {
		states = []string{"Backlog", "Todo", "In Progress", "In Review", "Done"}
	}
	for i, name := range states {
		f.states = append(f.states, linearState{ID: fmt.Sprintf("s%d", i), Name: name, Type: types[name]})
	}
	for i, name := range []string{"agent-build", "needs-human", "agent-plan", "agent-question", "epic"} {
		f.labels = append(f.labels, fakeLinearLabel{id: fmt.Sprintf("l%d", i), name: name, teamID: fakeTeamID})
	}
	return f
}

func (f *fakeLinear) withoutLabel(name string) *fakeLinear {
	kept := f.labels[:0]
	for _, l := range f.labels {
		if l.name != name {
			kept = append(kept, l)
		}
	}
	f.labels = kept
	return f
}

func (f *fakeLinear) setLabelName(from, to string) *fakeLinear {
	for i := range f.labels {
		if f.labels[i].name == from {
			f.labels[i].name = to
		}
	}
	return f
}

func (f *fakeLinear) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeLinear) admin(t *testing.T) *graphQLLinearAdmin {
	srv := f.server(t)
	return &graphQLLinearAdmin{endpoint: srv.URL, apiKey: fakeLinearKey, client: srv.Client()}
}

func (f *fakeLinear) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req struct {
		Query     string                     `json:"query"`
		Variables map[string]json.RawMessage `json:"variables"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &req)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.queries = append(f.queries, req.Query)

	var op string
	switch {
	case strings.Contains(req.Query, "issueLabelCreate"):
		op = "create"
	case strings.Contains(req.Query, "issueLabels("):
		op = "labels"
	case strings.Contains(req.Query, "states("):
		op = "states"
	case strings.Contains(req.Query, "viewer"):
		op = "viewer"
	case strings.Contains(req.Query, "teams("):
		op = "teams"
	}
	if op == "create" {
		f.createCalls++
		f.createInputs = append(f.createInputs, req.Variables["input"])
	}
	if op == f.failOp {
		w.WriteHeader(f.failStatus)
		_, _ = io.WriteString(w, `{"errors":[{"message":"`+fakeBodyMarker+` `+fakeLinearKey+`"}]}`)
		return
	}
	write := func(data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}
	switch op {
	case "teams":
		nodes := []map[string]string{}
		for _, t := range f.teams {
			nodes = append(nodes, map[string]string{"id": t.ID, "name": t.Name, "key": t.Key})
		}
		write(map[string]any{"teams": map[string]any{"nodes": nodes, "pageInfo": map[string]bool{"hasNextPage": f.teamsHasNext}}})
	case "viewer":
		write(map[string]any{"viewer": map[string]any{"id": "u1", "name": f.viewerName, "email": f.viewerEmail, "app": f.viewerApp, "organization": map[string]string{"name": f.org}}})
	case "states":
		nodes := []map[string]string{}
		for _, s := range f.states {
			nodes = append(nodes, map[string]string{"id": s.ID, "name": s.Name, "type": s.Type})
		}
		write(map[string]any{"team": map[string]any{"states": map[string]any{"nodes": nodes, "pageInfo": map[string]bool{"hasNextPage": f.statesHasNext}}}})
	case "labels":
		f.labelFilters = append(f.labelFilters, req.Variables["labels"])
		var filter struct {
			Or []struct {
				Name struct {
					EqIgnoreCase string `json:"eqIgnoreCase"`
				} `json:"name"`
			} `json:"or"`
		}
		_ = json.Unmarshal(req.Variables["labels"], &filter)
		nodes := []map[string]any{}
		for _, l := range f.labels {
			for _, clause := range filter.Or {
				if strings.EqualFold(l.name, clause.Name.EqIgnoreCase) {
					var team any
					if l.teamID != "" {
						team = map[string]string{"id": l.teamID}
					}
					nodes = append(nodes, map[string]any{"id": l.id, "name": l.name, "isGroup": l.isGroup, "team": team})
					break
				}
			}
		}
		hasNext := len(nodes) > linearPageSize
		if hasNext {
			nodes = nodes[:linearPageSize]
		}
		write(map[string]any{"issueLabels": map[string]any{"nodes": nodes, "pageInfo": map[string]bool{"hasNextPage": hasNext}}})
	case "create":
		if f.createErrCode != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": fakeBodyMarker, "extensions": map[string]string{"code": f.createErrCode}}}})
			return
		}
		var input struct {
			Name   string `json:"name"`
			TeamID string `json:"teamId"`
		}
		_ = json.Unmarshal(req.Variables["input"], &input)
		label := fakeLinearLabel{id: fmt.Sprintf("new-%d", f.createCalls), name: input.Name, teamID: input.TeamID}
		f.labels = append(f.labels, label)
		write(map[string]any{"issueLabelCreate": map[string]any{"success": true, "issueLabel": map[string]any{"id": label.id, "name": label.name, "team": map[string]string{"id": label.teamID}}}})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

// assertNoLeak fails when s carries the fake's response-body marker or the key.
func assertNoLeak(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, fakeBodyMarker) {
		t.Errorf("%s echoes the response body:\n%s", what, s)
	}
	if strings.Contains(s, fakeLinearKey) {
		t.Errorf("%s echoes the API key:\n%s", what, s)
	}
}

func linearSection(t *testing.T, tracker map[string]any, key string) map[string]any {
	t.Helper()
	linear, _ := tracker["linear"].(map[string]any)
	section, _ := linear[key].(map[string]any)
	return section
}

func TestInitLinearClientRequestShapes(t *testing.T) {
	f := newFakeLinear()
	admin := f.admin(t)
	ctx := context.Background()

	if _, _, err := admin.ListTeams(ctx); err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if _, err := admin.ListStates(ctx, fakeTeamID); err != nil {
		t.Fatalf("ListStates: %v", err)
	}
	if _, err := admin.FindLabels(ctx, fakeTeamID, []string{"agent-build", "Needs-Human", "needs-human"}); err != nil {
		t.Fatalf("FindLabels: %v", err)
	}
	if _, err := admin.CreateLabel(ctx, fakeTeamID, "agent-plan"); err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	for _, got := range f.auth {
		if got != fakeLinearKey {
			t.Fatalf("Authorization = %q; want the candidate key, raw", got)
		}
	}
	for _, want := range []string{"teams(first: 250)", "states(first: 250)", "issueLabels(first: 250, filter: $labels)"} {
		if !strings.Contains(strings.Join(f.queries, "\n"), want) {
			t.Errorf("no query contains %q:\n%s", want, strings.Join(f.queries, "\n"))
		}
	}
	// The label filter is linear-gate-lib.mjs's shape, byte for byte, with
	// case-insensitive duplicates collapsed.
	const wantFilter = `{"or":[{"name":{"eqIgnoreCase":"agent-build"}},{"name":{"eqIgnoreCase":"Needs-Human"}}]}`
	if len(f.labelFilters) != 1 || string(f.labelFilters[0]) != wantFilter {
		t.Errorf("label filter = %s; want %s", f.labelFilters, wantFilter)
	}
	if len(f.createInputs) != 1 || string(f.createInputs[0]) != `{"name":"agent-plan","teamId":"team-1"}` {
		t.Errorf("create input = %s; want a team-scoped agent-plan", f.createInputs)
	}
}

func TestInitLinearFindLabelsScopesToTeamAndWorkspace(t *testing.T) {
	f := newFakeLinear()
	f.labels = []fakeLinearLabel{
		{id: "g", name: "epic", teamID: fakeTeamID, isGroup: true}, // a group cannot be applied
		{id: "other", name: "agent-build", teamID: "team-2"},       // invisible to this team
		{id: "ws", name: "agent-plan"},                             // workspace label
		{id: "child", name: "needs-human", teamID: fakeTeamID},     // child of a group
	}
	labels, err := f.admin(t).FindLabels(context.Background(), fakeTeamID, linearLabelLookupNames(linearOverrides{}))
	if err != nil {
		t.Fatalf("FindLabels: %v", err)
	}
	var got []string
	for _, l := range labels {
		got = append(got, l.ID)
	}
	if strings.Join(got, ",") != "ws,child" {
		t.Fatalf("labels = %v; want the workspace label and the team's child label only", got)
	}
}

func TestInitLinearErrorsAreClassOnly(t *testing.T) {
	cases := []struct {
		name, op, class string
		status          int
		call            func(linearAdmin) error
	}{
		{"teams 401", "teams", "unauthorized", http.StatusUnauthorized, func(a linearAdmin) error { _, _, err := a.ListTeams(context.Background()); return err }},
		{"viewer 500", "viewer", "HTTP status 500", http.StatusInternalServerError, func(a linearAdmin) error { _, err := a.Viewer(context.Background()); return err }},
		{"states GraphQL error", "states", "GraphQL error", http.StatusOK, func(a linearAdmin) error { _, err := a.ListStates(context.Background(), fakeTeamID); return err }},
		{"labels 403", "labels", "HTTP status 403", http.StatusForbidden, func(a linearAdmin) error {
			_, err := a.FindLabels(context.Background(), fakeTeamID, []string{"epic"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLinear()
			f.failOp, f.failStatus = tc.op, tc.status
			err := tc.call(f.admin(t))
			var apiErr *linearAPIError
			if !errors.As(err, &apiErr) || apiErr.class != tc.class {
				t.Fatalf("err = %v; want class %q", err, tc.class)
			}
			assertNoLeak(t, "error", err.Error())
		})
	}
	t.Run("AUTHENTICATION_ERROR on HTTP 400", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errors":[{"message":"`+fakeBodyMarker+`","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`)
		}))
		defer srv.Close()
		_, err := (&graphQLLinearAdmin{endpoint: srv.URL, apiKey: fakeLinearKey, client: srv.Client()}).Viewer(context.Background())
		if linearErrorClass(err, "") != "unauthorized" {
			t.Fatalf("err = %v; want class unauthorized", err)
		}
		assertNoLeak(t, "error", err.Error())
	})
}

func TestInitLinearUnauthorizedKeyNeverLeaks(t *testing.T) {
	t.Run("teams listing degrades to guidance", func(t *testing.T) {
		f := newFakeLinear()
		f.failOp, f.failStatus = "teams", http.StatusUnauthorized
		report, tracker, err := runInitWithTracker(t, initOptions{admin: f.admin(t)})
		if err != nil {
			t.Fatalf("a failed team listing must not fail boss init: %v", err)
		}
		if tracker != nil {
			t.Fatalf("a failed listing wrote a tracker block: %v", tracker)
		}
		if !strings.Contains(strings.Join(strings.Fields(report), " "), "could not be listed (unauthorized)") {
			t.Errorf("report lacks the unauthorized class:\n%s", report)
		}
		assertNoLeak(t, "report", report)
	})
	t.Run("viewer failure fails closed", func(t *testing.T) {
		f := newFakeLinear()
		f.failOp, f.failStatus = "viewer", http.StatusUnauthorized
		report, _, err := runInitWithTracker(t, initOptions{admin: f.admin(t)})
		if err == nil || !strings.Contains(err.Error(), "unauthorized") {
			t.Fatalf("err = %v; want an unauthorized failure", err)
		}
		assertNoLeak(t, "error", err.Error())
		assertNoLeak(t, "report", report)
	})
}

func TestInitLinearReportsViewerAndWorkspaceBeforeWriting(t *testing.T) {
	f := newFakeLinear().withoutLabel("needs-human")
	report, _, err := runInitWithTracker(t, initOptions{admin: f.admin(t), createLabels: true})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	viewer := strings.Index(report, `Linear key belongs to Ada <ada@acme.test> in workspace "Acme"`)
	create := strings.Index(report, `Creating Linear labels in workspace "Acme", team Example (EX): needs-human`)
	wrote := strings.Index(report, "Wrote ")
	if viewer < 0 || create < 0 || wrote < 0 || viewer > create || create > wrote {
		t.Fatalf("want viewer, then the label-create target, then the write (got %d, %d, %d):\n%s", viewer, create, wrote, report)
	}
	if strings.Contains(report, "app/bot") {
		t.Errorf("a human viewer drew the bot warning:\n%s", report)
	}
}

func TestInitLinearAppViewerWarns(t *testing.T) {
	f := newFakeLinear()
	f.viewerApp = true
	report, _, err := runInitWithTracker(t, initOptions{admin: f.admin(t)})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if !strings.Contains(report, "Warning: the Linear key belongs to an app/bot user") {
		t.Fatalf("no bot warning:\n%s", report)
	}
}

func TestInitLinearTruncatedListingsFailClosed(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeLinear)
		team  string
	}{
		{"states page", func(f *fakeLinear) { f.statesHasNext = true }, ""},
		{"teams page", func(f *fakeLinear) { f.teamsHasNext = true }, ""},
		{"teams page, --team beyond it", func(f *fakeLinear) { f.teamsHasNext = true }, "Zeta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLinear()
			tc.setup(f)
			// runInitWithTracker fails the test if a config exists after an error.
			report, _, err := runInitWithTracker(t, initOptions{admin: f.admin(t), team: tc.team})
			var apiErr *linearAPIError
			if !errors.As(err, &apiErr) || apiErr.class != "truncated" || !strings.Contains(err.Error(), "nothing written") {
				t.Fatalf("err = %v; want a class-only truncated failure", err)
			}
			assertNoLeak(t, "error", err.Error())
			assertNoLeak(t, "report", report)
		})
	}
}

func TestInitLinearResolvesRolesAmongFourHundredLabels(t *testing.T) {
	f := newFakeLinear().setLabelName("agent-build", "Agent-Build")
	for i := range 400 {
		f.labels = append(f.labels, fakeLinearLabel{id: fmt.Sprintf("bulk-%d", i), name: fmt.Sprintf("area-%03d", i), teamID: fakeTeamID})
	}
	report, tracker, err := runInitWithTracker(t, initOptions{admin: f.admin(t), createLabels: true})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if f.createCalls != 0 {
		t.Fatalf("created %d labels; every role exists", f.createCalls)
	}
	labels := linearSection(t, tracker, "labels")
	if len(labels) != 1 || labels["agentBuild"] != "Agent-Build" {
		t.Fatalf("labels = %v; want exactly agentBuild: Agent-Build", labels)
	}
	if strings.Contains(report, "truncated") {
		t.Errorf("the name filter still truncated:\n%s", report)
	}
}

func TestInitLinearCaseDifferencesAreWritten(t *testing.T) {
	f := newFakeLinear("Backlog", "Todo", "In progress", "In Review", "Done").setLabelName("agent-build", "Agent-Build")
	f.states[2].Type = "started"
	report, tracker, err := runInitWithTracker(t, initOptions{admin: f.admin(t)})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	states, labels := linearSection(t, tracker, "states"), linearSection(t, tracker, "labels")
	if len(states) != 1 || states["inProgress"] != "In progress" {
		t.Errorf("states = %v; want exactly inProgress: In progress", states)
	}
	if len(labels) != 1 || labels["agentBuild"] != "Agent-Build" {
		t.Errorf("labels = %v; want exactly agentBuild: Agent-Build", labels)
	}
	if f.createCalls != 0 {
		t.Errorf("a case-different label was created (%d calls)", f.createCalls)
	}
	if !strings.Contains(report, `states.inProgress: "In progress"`) {
		t.Errorf("report does not name the mapped role:\n%s", report)
	}
}

func TestInitLinearDefaultNamesWriteNoRoleOverrides(t *testing.T) {
	report, tracker, err := runInitWithTracker(t, initOptions{admin: newFakeLinear().admin(t)})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	linear, _ := tracker["linear"].(map[string]any)
	if linear["team"] != "Example" || linear["states"] != nil || linear["labels"] != nil {
		t.Fatalf("trackerConfig.linear = %v; want the team and no role overrides", linear)
	}
	if !strings.Contains(report, "every state and label matches its default name") {
		t.Errorf("report lacks the all-defaults line:\n%s", report)
	}
}

func TestInitLinearCreateLabelsOnceThenNone(t *testing.T) {
	f := newFakeLinear().withoutLabel("needs-human")
	admin := f.admin(t)

	report, _, err := runInitWithTracker(t, initOptions{admin: admin})
	if err != nil {
		t.Fatalf("runInit without --create-labels: %v", err)
	}
	if f.createCalls != 0 || !strings.Contains(report, "re-run with --create-labels") {
		t.Fatalf("without consent: %d create calls; report:\n%s", f.createCalls, report)
	}

	if _, _, err := runInitWithTracker(t, initOptions{admin: admin, createLabels: true}); err != nil {
		t.Fatalf("first --create-labels run: %v", err)
	}
	if f.createCalls != 1 || string(f.createInputs[0]) != `{"name":"needs-human","teamId":"team-1"}` {
		t.Fatalf("first run: %d create calls (%s); want one team-scoped needs-human", f.createCalls, f.createInputs)
	}

	if _, _, err := runInitWithTracker(t, initOptions{admin: admin, createLabels: true}); err != nil {
		t.Fatalf("second --create-labels run: %v", err)
	}
	if f.createCalls != 1 {
		t.Fatalf("re-run made %d more create calls; want zero", f.createCalls-1)
	}
}

func TestInitLinearCreateLabelPermissionErrorContinues(t *testing.T) {
	f := newFakeLinear().withoutLabel("needs-human")
	f.createErrCode = "FORBIDDEN"
	report, tracker, err := runInitWithTracker(t, initOptions{admin: f.admin(t), createLabels: true})
	if err != nil {
		t.Fatalf("a failed label create must not fail boss init: %v", err)
	}
	if f.createCalls != 1 {
		t.Fatalf("create calls = %d; want 1", f.createCalls)
	}
	if !strings.Contains(report, `could not create label "needs-human" for role needsHuman (forbidden)`) {
		t.Errorf("report lacks the class-only create failure:\n%s", report)
	}
	if labels := linearSection(t, tracker, "labels"); labels["needsHuman"] != nil {
		t.Errorf("needsHuman = %v; want it left at its default", labels["needsHuman"])
	}
	assertNoLeak(t, "report", report)
}

func TestInitLinearExplicitStateFlagsAreChecked(t *testing.T) {
	f := newFakeLinear("Triage", "Ready", "Doing", "Review", "Shipped")
	for i, typ := range []string{"triage", "unstarted", "started", "started", "completed"} {
		f.states[i].Type = typ
	}
	explicit, err := parseLinearOverrides([]string{"unplanned=Triage", "planned=Ready", "inProgress=Doing", "inReview=Review", "done=Shipped"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	_, tracker, err := runInitWithTracker(t, initOptions{admin: f.admin(t), overrides: explicit})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if states := linearSection(t, tracker, "states"); len(states) != 5 || states["done"] != "Shipped" {
		t.Fatalf("states = %v; want all five roles", states)
	}

	typo, _ := parseLinearOverrides([]string{"planned=Redy"}, nil, false)
	_, _, err = runInitWithTracker(t, initOptions{admin: newFakeLinear().admin(t), overrides: typo})
	if err == nil || !strings.Contains(err.Error(), "available: Backlog, Todo, In Progress, In Review, Done") {
		t.Fatalf("err = %v; want the typo named with the available states", err)
	}
}

func TestInitLinearMergeSeedsRolesFromExistingConfig(t *testing.T) {
	f := newFakeLinear("Backlog", "Ready for Dev", "In Progress", "In Review", "Done").setLabelName("agent-build", "build-me")
	f.states[1].Type = "unstarted"
	dir := t.TempDir()
	target := filepath.Join(dir, ".boss-skills.json")
	source := `{"trackerConfig":{"linear":{"states":{"planned":"Ready for Dev"},"labels":{"agentBuild":"build-me"}}}}` + "\n"
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n", ".boss-skills.json": source})
	var out bytes.Buffer
	if err := runInit(&out, dir, false, initOptions{admin: f.admin(t), mergeExisting: true}); err != nil {
		t.Fatalf("runInit --merge over an existing role mapping: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	tracker, _ := onDisk["trackerConfig"].(map[string]any)
	if states := linearSection(t, tracker, "states"); states["planned"] != "Ready for Dev" {
		t.Errorf("states = %v; want the existing planned: Ready for Dev kept", states)
	}
	if labels := linearSection(t, tracker, "labels"); labels["agentBuild"] != "build-me" {
		t.Errorf("labels = %v; want the existing agentBuild: build-me kept", labels)
	}
	if strings.Contains(out.String(), "agent-build") {
		t.Errorf("the default label was looked up as missing despite the existing mapping:\n%s", out.String())
	}
}
