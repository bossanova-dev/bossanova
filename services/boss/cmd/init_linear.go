package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// --- Linear admin client ---------------------------------------------------
//
// boss init's view of Linear: who a key belongs to, the teams it can see, a
// team's workflow states, and the pipeline labels. Labels are the only thing
// it ever writes; workflow states, teams and projects are never created.
//
// Every query uses the key the client was built with (the candidate key), so
// what init sees is what that key will see at gate time. Every failure is a
// class-only linearAPIError: neither a response body nor a transport error's
// text (both can carry request data) is ever part of Error().

// linearGraphQLEndpoint is Linear's public GraphQL API.
const linearGraphQLEndpoint = "https://api.linear.app/graphql"

// linearPageSize bounds every list query. A further page is truncation, which
// fails closed rather than paginating: a mapping built from part of a team's
// states could silently pick the wrong one.
const linearPageSize = 250

// linearRequestTimeout bounds each call; boss init must not hang on the network.
const linearRequestTimeout = 15 * time.Second

// linearResponseLimit caps how much of a response is ever read.
const linearResponseLimit = 1 << 20

const (
	linearTeamsQuery = `{ teams(first: 250) { nodes { id name key } pageInfo { hasNextPage } } }`

	linearViewerQuery = `{ viewer { id name email app organization { name } } }`

	linearStatesQuery = `query TeamStates($teamId: String!) {
  team(id: $teamId) { states(first: 250) { nodes { id name type } pageInfo { hasNextPage } } }
}`

	// linearLabelsQuery is the label lookup skills-toolbox/linear-gate-lib.mjs
	// issues (selectionRefsQuery): workspace-wide issueLabels with an
	// IssueLabelFilter of `or: [{ name: { eqIgnoreCase } }]`. It returns team AND
	// workspace labels, including children of label groups, and the name filter
	// means a workspace with hundreds of labels never pages. Team scoping is
	// applied to the returned nodes rather than invented as a new filter shape.
	linearLabelsQuery = `query FindLabels($labels: IssueLabelFilter) {
  issueLabels(first: 250, filter: $labels) { nodes { id name isGroup team { id } } pageInfo { hasNextPage } }
}`

	linearCreateLabelMutation = `mutation CreateLabel($input: IssueLabelCreateInput!) {
  issueLabelCreate(input: $input) { success issueLabel { id name team { id } } }
}`
)

type linearTeam struct {
	ID, Name, Key string
}

// linearViewer is the user a key belongs to. App is Linear's app/bot flag: an
// "only my tickets" selection resolves to that bot rather than a person.
type linearViewer struct {
	ID, Name, Email, Organization string
	App                           bool
}

// linearState is one workflow state. Type is Linear's state category: triage,
// backlog, unstarted, started, completed, canceled or duplicate.
type linearState struct {
	ID, Name, Type string
}

// linearLabel is an applicable issue label. TeamID is empty for a workspace label.
type linearLabel struct {
	ID, Name, TeamID string
}

// teamLister lists the Linear teams visible to a credential. hasNextPage is
// true when more teams exist than were returned.
type teamLister interface {
	ListTeams(ctx context.Context) (teams []linearTeam, hasNextPage bool, err error)
}

// linearAdmin is everything boss init asks of Linear. It widens teamLister so
// the team-resolution path keeps accepting either.
type linearAdmin interface {
	teamLister
	Viewer(ctx context.Context) (linearViewer, error)
	// ListStates returns every workflow state of a team; a truncated page is a
	// linearAPIError of class "truncated".
	ListStates(ctx context.Context, teamID string) ([]linearState, error)
	// FindLabels returns the applicable (non-group) team or workspace labels
	// whose name equals one of names case-insensitively.
	FindLabels(ctx context.Context, teamID string, names []string) ([]linearLabel, error)
	// CreateLabel creates a team-scoped label.
	CreateLabel(ctx context.Context, teamID, name string) (linearLabel, error)
}

// linearAPIError names only the CLASS of a failure (unauthorized, HTTP status
// N, request failed, GraphQL error, forbidden, unreadable response, truncated).
type linearAPIError struct {
	op, class string
	err       error
}

func (e *linearAPIError) Error() string { return e.op + ": " + e.class }
func (e *linearAPIError) Unwrap() error { return e.err }

// linearErrorClass is the class of err when it is a linearAPIError, else fallback.
func linearErrorClass(err error, fallback string) string {
	var apiErr *linearAPIError
	if errors.As(err, &apiErr) {
		return apiErr.class
	}
	return fallback
}

// graphQLLinearAdmin is the production linearAdmin. It sends the raw API key in
// Authorization with no Bearer prefix, as Linear personal API keys require.
type graphQLLinearAdmin struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

// newLinearAdmin builds the production client for one candidate key; a blank
// key yields nil, meaning "no Linear access", never an error.
func newLinearAdmin(key string) linearAdmin {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return &graphQLLinearAdmin{
		endpoint: linearGraphQLEndpoint,
		apiKey:   key,
		client:   &http.Client{Timeout: linearRequestTimeout},
	}
}

// linearAdminFromEnv builds the client from ambient LINEAR_API_KEY, exactly as
// boss init's team listing always has.
func linearAdminFromEnv(getenv func(string) string) linearAdmin {
	return initNewLinearAdmin(getenv("LINEAR_API_KEY"))
}

// initNewLinearAdmin builds boss init's Linear client for a candidate key; a
// package var so tests point both the flag and interview paths at a fake.
var initNewLinearAdmin = newLinearAdmin

// do runs one GraphQL request and decodes its data into out. Every failure is
// a class-only linearAPIError tagged with op.
func (a *graphQLLinearAdmin) do(ctx context.Context, op, query string, variables map[string]any, out any) error {
	fail := func(class string, err error) error { return &linearAPIError{op: op, class: class, err: err} }
	ctx, cancel := context.WithTimeout(ctx, linearRequestTimeout)
	defer cancel()
	request := map[string]any{"query": query}
	if variables != nil {
		request["variables"] = variables
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fail("request could not be encoded", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(payload))
	if err != nil {
		return fail("request could not be built", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", a.apiKey)
	client := a.client
	if client == nil {
		client = &http.Client{Timeout: linearRequestTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail("request failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, linearResponseLimit)).Decode(&body)
	// Linear reports a rejected key as a GraphQL AUTHENTICATION_ERROR, often on
	// a non-200 status, so the code is consulted before the status.
	switch {
	case decodeErr == nil && graphQLErrorClass(body.Errors) == "unauthorized",
		resp.StatusCode == http.StatusUnauthorized:
		return fail("unauthorized", nil)
	case resp.StatusCode != http.StatusOK:
		return fail(fmt.Sprintf("HTTP status %d", resp.StatusCode), nil)
	case decodeErr != nil:
		return fail("unreadable response", decodeErr)
	case len(body.Errors) > 0:
		return fail(graphQLErrorClass(body.Errors), nil)
	case len(body.Data) == 0 || string(body.Data) == "null":
		return fail("unreadable response", nil)
	}
	if err := json.Unmarshal(body.Data, out); err != nil {
		return fail("unreadable response", err)
	}
	return nil
}

// graphQLError is the only part of a GraphQL error ever decoded: its
// machine-readable code. The message can quote the request and is never read.
type graphQLError struct {
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// graphQLErrorClass maps Linear's error extension codes onto a class.
func graphQLErrorClass(errs []graphQLError) string {
	if len(errs) == 0 {
		return ""
	}
	for _, e := range errs {
		switch strings.ToUpper(e.Extensions.Code) {
		case "AUTHENTICATION_ERROR", "UNAUTHENTICATED":
			return "unauthorized"
		case "FORBIDDEN":
			return "forbidden"
		}
	}
	return "GraphQL error"
}

type pageInfo struct {
	HasNextPage bool `json:"hasNextPage"`
}

func (a *graphQLLinearAdmin) ListTeams(ctx context.Context) ([]linearTeam, bool, error) {
	const op = "list Linear teams"
	var data struct {
		Teams *struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Key  string `json:"key"`
			} `json:"nodes"`
			PageInfo pageInfo `json:"pageInfo"`
		} `json:"teams"`
	}
	if err := a.do(ctx, op, linearTeamsQuery, nil, &data); err != nil {
		return nil, false, err
	}
	if data.Teams == nil {
		return nil, false, &linearAPIError{op: op, class: "unreadable response"}
	}
	teams := make([]linearTeam, 0, len(data.Teams.Nodes))
	for _, n := range data.Teams.Nodes {
		if strings.TrimSpace(n.Name) == "" {
			return nil, false, &linearAPIError{op: op, class: "unreadable response"}
		}
		teams = append(teams, linearTeam{ID: n.ID, Name: n.Name, Key: n.Key})
	}
	return teams, data.Teams.PageInfo.HasNextPage, nil
}

func (a *graphQLLinearAdmin) Viewer(ctx context.Context) (linearViewer, error) {
	const op = "read Linear viewer"
	var data struct {
		Viewer *struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Email        string `json:"email"`
			App          bool   `json:"app"`
			Organization *struct {
				Name string `json:"name"`
			} `json:"organization"`
		} `json:"viewer"`
	}
	if err := a.do(ctx, op, linearViewerQuery, nil, &data); err != nil {
		return linearViewer{}, err
	}
	if data.Viewer == nil || data.Viewer.Organization == nil {
		return linearViewer{}, &linearAPIError{op: op, class: "unreadable response"}
	}
	v := data.Viewer
	return linearViewer{ID: v.ID, Name: v.Name, Email: v.Email, App: v.App, Organization: v.Organization.Name}, nil
}

func (a *graphQLLinearAdmin) ListStates(ctx context.Context, teamID string) ([]linearState, error) {
	const op = "list Linear workflow states"
	var data struct {
		Team *struct {
			States *struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"nodes"`
				PageInfo pageInfo `json:"pageInfo"`
			} `json:"states"`
		} `json:"team"`
	}
	if err := a.do(ctx, op, linearStatesQuery, map[string]any{"teamId": teamID}, &data); err != nil {
		return nil, err
	}
	if data.Team == nil || data.Team.States == nil {
		return nil, &linearAPIError{op: op, class: "unreadable response"}
	}
	if data.Team.States.PageInfo.HasNextPage {
		return nil, &linearAPIError{op: op, class: "truncated"}
	}
	states := make([]linearState, 0, len(data.Team.States.Nodes))
	for _, n := range data.Team.States.Nodes {
		if strings.TrimSpace(n.Name) == "" {
			return nil, &linearAPIError{op: op, class: "unreadable response"}
		}
		states = append(states, linearState{ID: n.ID, Name: n.Name, Type: n.Type})
	}
	return states, nil
}

// linearLabelFilter is the IssueLabelFilter linear-gate-lib.mjs sends:
// { or: [{ name: { eqIgnoreCase: <name> } }, ...] }.
func linearLabelFilter(names []string) map[string]any {
	or := make([]map[string]any, 0, len(names))
	for _, name := range names {
		or = append(or, map[string]any{"name": map[string]any{"eqIgnoreCase": name}})
	}
	return map[string]any{"or": or}
}

func (a *graphQLLinearAdmin) FindLabels(ctx context.Context, teamID string, names []string) ([]linearLabel, error) {
	const op = "find Linear labels"
	unique := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if key := strings.ToLower(name); name != "" && !seen[key] {
			seen[key] = true
			unique = append(unique, name)
		}
	}
	if len(unique) == 0 {
		return nil, nil
	}
	var data struct {
		IssueLabels *struct {
			Nodes []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				IsGroup bool   `json:"isGroup"`
				Team    *struct {
					ID string `json:"id"`
				} `json:"team"`
			} `json:"nodes"`
			PageInfo pageInfo `json:"pageInfo"`
		} `json:"issueLabels"`
	}
	if err := a.do(ctx, op, linearLabelsQuery, map[string]any{"labels": linearLabelFilter(unique)}, &data); err != nil {
		return nil, err
	}
	if data.IssueLabels == nil {
		return nil, &linearAPIError{op: op, class: "unreadable response"}
	}
	if data.IssueLabels.PageInfo.HasNextPage {
		return nil, &linearAPIError{op: op, class: "truncated"}
	}
	var labels []linearLabel
	for _, n := range data.IssueLabels.Nodes {
		// A group label cannot be applied to an issue, and another team's label
		// is invisible to this team's issues: neither can satisfy a role.
		if n.IsGroup || strings.TrimSpace(n.Name) == "" {
			continue
		}
		label := linearLabel{ID: n.ID, Name: n.Name}
		if n.Team != nil {
			if n.Team.ID != teamID {
				continue
			}
			label.TeamID = n.Team.ID
		}
		labels = append(labels, label)
	}
	return labels, nil
}

func (a *graphQLLinearAdmin) CreateLabel(ctx context.Context, teamID, name string) (linearLabel, error) {
	const op = "create Linear label"
	var data struct {
		IssueLabelCreate *struct {
			Success    bool `json:"success"`
			IssueLabel *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Team *struct {
					ID string `json:"id"`
				} `json:"team"`
			} `json:"issueLabel"`
		} `json:"issueLabelCreate"`
	}
	input := map[string]any{"name": name, "teamId": teamID}
	if err := a.do(ctx, op, linearCreateLabelMutation, map[string]any{"input": input}, &data); err != nil {
		return linearLabel{}, err
	}
	created := data.IssueLabelCreate
	if created == nil || !created.Success || created.IssueLabel == nil {
		return linearLabel{}, &linearAPIError{op: op, class: "not created"}
	}
	label := linearLabel{ID: created.IssueLabel.ID, Name: created.IssueLabel.Name}
	if created.IssueLabel.Team != nil {
		label.TeamID = created.IssueLabel.Team.ID
	}
	return label, nil
}

// --- boss init mapping flow -------------------------------------------------

// createMissingLabels creates each missing label at team scope, naming the
// target workspace and team before the first write. A failed create is reported
// by class and the flow continues; that role keeps the name the mapping chose
// (its default unless --label named another). It returns the labels created.
func createMissingLabels(ctx context.Context, out io.Writer, admin linearAdmin, viewer linearViewer, team linearTeam, missing []missingLabel) (map[string]linearLabel, []string) {
	created := map[string]linearLabel{}
	if len(missing) == 0 {
		return created, nil
	}
	names := make([]string, 0, len(missing))
	for _, m := range missing {
		names = append(names, m.Name)
	}
	_, _ = fmt.Fprintf(out, "Creating Linear labels in workspace %q, team %s: %s\n", viewer.Organization, teamDisplay(team), strings.Join(names, ", "))
	var warnings []string
	for _, m := range missing {
		label, err := admin.CreateLabel(ctx, team.ID, m.Name)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not create label %q for role %s (%s); the role keeps %q",
				m.Name, m.Role, linearErrorClass(err, "request failed"), m.Name))
			continue
		}
		_, _ = fmt.Fprintf(out, "  created %s\n", label.Name)
		created[m.Role] = label
	}
	return created, warnings
}

// runLinearMapping resolves the role mapping for one team through admin and
// returns explicit widened with every non-default state and label name. Every
// error is class-only or names Linear state names; the caller writes nothing
// when one is returned.
func runLinearMapping(ctx context.Context, out io.Writer, admin linearAdmin, team linearTeam, explicit linearOverrides, createLabels bool, steps *initAppliedSteps) (linearOverrides, error) {
	viewer, err := admin.Viewer(ctx)
	if err != nil {
		return explicit, err
	}
	_, _ = fmt.Fprintf(out, "Linear key belongs to %s in workspace %q\n", viewerDisplay(viewer), viewer.Organization)
	if viewer.App {
		_, _ = fmt.Fprintf(out, "Warning: the Linear key belongs to an app/bot user; an \"only my tickets\" (assignee: me) filter resolves to that bot, not to you\n")
	}
	states, err := admin.ListStates(ctx, team.ID)
	if err != nil {
		return explicit, err
	}
	labels, err := admin.FindLabels(ctx, team.ID, linearLabelLookupNames(explicit))
	if err != nil {
		return explicit, err
	}
	mapping, err := mapLinearRoles(states, labels, explicit)
	if err != nil {
		return explicit, err
	}
	warnings := mapping.Warnings
	switch {
	case len(mapping.Missing) > 0 && createLabels:
		created, createWarnings := createMissingLabels(ctx, out, admin, viewer, team, mapping.Missing)
		warnings = append(warnings, createWarnings...)
		for _, m := range mapping.Missing {
			label, ok := created[m.Role]
			if !ok {
				continue
			}
			if steps != nil {
				steps.LabelsCreated = append(steps.LabelsCreated, label.Name)
			}
			if label.Name != linearLabelDefaults[m.Role] {
				mapping.Overrides.Labels[m.Role] = label.Name
			}
		}
	case len(mapping.Missing) > 0:
		names := make([]string, 0, len(mapping.Missing))
		for _, m := range mapping.Missing {
			names = append(names, m.Name+" ("+m.Role+")")
		}
		warnings = append(warnings, fmt.Sprintf("Linear labels missing from team %s: %s; re-run with --create-labels to create them", teamDisplay(team), strings.Join(names, ", ")))
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(out, "Warning: %s\n", w)
	}
	writeRoleSummary(out, mapping.Overrides)
	return mapping.Overrides, nil
}

// writeRoleSummary names every role written as an override, or says that every
// role matched its default byte-for-byte.
func writeRoleSummary(out io.Writer, ov linearOverrides) {
	if len(ov.States)+len(ov.Labels) == 0 {
		_, _ = fmt.Fprintln(out, "Linear roles: every state and label matches its default name")
		return
	}
	_, _ = fmt.Fprintln(out, "Linear roles mapped to this team's names:")
	for _, role := range sortedOverrideKeys(ov.States) {
		_, _ = fmt.Fprintf(out, "  states.%s: %q\n", role, ov.States[role])
	}
	for _, role := range sortedOverrideKeys(ov.Labels) {
		_, _ = fmt.Fprintf(out, "  labels.%s: %q\n", role, ov.Labels[role])
	}
}

func viewerDisplay(v linearViewer) string {
	name := strings.TrimSpace(v.Name)
	if name == "" {
		name = "an unnamed user"
	}
	if v.Email != "" {
		name += " <" + v.Email + ">"
	}
	return name
}

func teamDisplay(t linearTeam) string {
	if t.Key != "" {
		return t.Name + " (" + t.Key + ")"
	}
	return t.Name
}
