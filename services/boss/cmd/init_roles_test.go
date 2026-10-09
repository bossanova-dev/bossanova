package main

import (
	"errors"
	"maps"
	"strings"
	"testing"
)

func rolesStates(pairs ...string) []linearState {
	var states []linearState
	for i := 0; i+1 < len(pairs); i += 2 {
		states = append(states, linearState{ID: "id-" + pairs[i], Name: pairs[i], Type: pairs[i+1]})
	}
	return states
}

func defaultRoleStates() []linearState {
	return rolesStates("Backlog", "backlog", "Todo", "unstarted", "In Progress", "started", "In Review", "started", "Done", "completed")
}

func defaultRoleLabels() []linearLabel {
	var labels []linearLabel
	for _, role := range linearLabelRoles {
		labels = append(labels, linearLabel{ID: role, Name: linearLabelDefaults[role], TeamID: "t"})
	}
	return labels
}

func mustOverrides(t *testing.T, states, labels []string) linearOverrides {
	t.Helper()
	ov, err := parseLinearOverrides(states, labels, false)
	if err != nil {
		t.Fatal(err)
	}
	return ov
}

func TestInitRolesDefaultsProduceNoOverrides(t *testing.T) {
	m, err := mapLinearRoles(defaultRoleStates(), defaultRoleLabels(), linearOverrides{})
	if err != nil {
		t.Fatalf("mapLinearRoles: %v", err)
	}
	if len(m.Overrides.States)+len(m.Overrides.Labels)+len(m.Missing)+len(m.Warnings) != 0 {
		t.Fatalf("mapping = %+v; want nothing to write and nothing to warn", m)
	}
}

func TestInitRolesExplicitFlagsCoverEveryStateRole(t *testing.T) {
	states := rolesStates("Triage", "triage", "Ready", "unstarted", "Doing", "started", "Review", "started", "Shipped", "completed")
	explicit := mustOverrides(t, []string{"unplanned=Triage", "planned=ready", "inProgress=Doing", "inReview=Review", "done=Shipped"}, nil)
	m, err := mapLinearRoles(states, defaultRoleLabels(), explicit)
	if err != nil {
		t.Fatalf("mapLinearRoles: %v", err)
	}
	want := map[string]string{"unplanned": "Triage", "planned": "Ready", "inProgress": "Doing", "inReview": "Review", "done": "Shipped"}
	if !maps.Equal(m.Overrides.States, want) {
		t.Fatalf("states = %v; want %v (a flag resolves to the team's own spelling)", m.Overrides.States, want)
	}
	if len(m.Warnings) != 0 {
		t.Errorf("warnings = %v; want none", m.Warnings)
	}
}

func TestInitRolesPlannedSharingUnplannedIsAnError(t *testing.T) {
	states := rolesStates("Ready", "unstarted", "Doing", "started", "Review", "started", "Shipped", "completed")
	explicit := mustOverrides(t, []string{"unplanned=Ready", "planned=Ready", "inProgress=Doing", "inReview=Review", "done=Shipped"}, nil)
	_, err := mapLinearRoles(states, defaultRoleLabels(), explicit)
	if !errors.Is(err, errPlannedIsUnplanned) {
		t.Fatalf("err = %v; want errPlannedIsUnplanned", err)
	}
}

func TestInitRolesMismatchesOnlyWarn(t *testing.T) {
	states := append(defaultRoleStates(), rolesStates("Shipped", "completed")...)
	explicit := mustOverrides(t, []string{"planned=Shipped", "inReview=In Progress"}, nil)
	m, err := mapLinearRoles(states, defaultRoleLabels(), explicit)
	if err != nil {
		t.Fatalf("a type mismatch must not fail the mapping: %v", err)
	}
	if m.Overrides.States["planned"] != "Shipped" || m.Overrides.States["inReview"] != "In Progress" {
		t.Fatalf("states = %v; want the mismatched choices accepted", m.Overrides.States)
	}
	joined := strings.Join(m.Warnings, "\n")
	for _, want := range []string{
		`state role planned maps to "Shipped", a completed state; expected unstarted`,
		`state roles inProgress and inReview both map to "In Progress"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
}

func TestInitRolesWriteOnlyNonByteIdenticalNames(t *testing.T) {
	states := rolesStates("backlog", "backlog", "Todo", "unstarted", "In progress", "started", "In Review", "started", "Done", "completed")
	labels := defaultRoleLabels()
	labels[0].Name = "Agent-Build"
	m, err := mapLinearRoles(states, labels, linearOverrides{})
	if err != nil {
		t.Fatalf("mapLinearRoles: %v", err)
	}
	if want := map[string]string{"unplanned": "backlog", "inProgress": "In progress"}; !maps.Equal(m.Overrides.States, want) {
		t.Errorf("states = %v; want %v", m.Overrides.States, want)
	}
	if want := map[string]string{"agentBuild": "Agent-Build"}; !maps.Equal(m.Overrides.Labels, want) {
		t.Errorf("labels = %v; want %v", m.Overrides.Labels, want)
	}
	if len(m.Missing) != 0 {
		t.Errorf("missing = %v; a case-different label is found, not missing", m.Missing)
	}
}

func TestInitRolesUnresolvedStateNamesRoleAndAvailable(t *testing.T) {
	states := rolesStates("Backlog", "backlog", "Ready", "unstarted", "In Progress", "started", "In Review", "started", "Done", "completed")
	_, err := mapLinearRoles(states, defaultRoleLabels(), linearOverrides{})
	var unresolved *unresolvedRoleError
	if !errors.As(err, &unresolved) || unresolved.role != "planned" {
		t.Fatalf("err = %v; want an unresolved planned role", err)
	}
	if !strings.Contains(err.Error(), "available: Backlog, Ready, In Progress, In Review, Done") {
		t.Errorf("error does not list the available states: %v", err)
	}
}

func TestInitRolesExplicitStateTypoIsAnError(t *testing.T) {
	_, err := mapLinearRoles(defaultRoleStates(), defaultRoleLabels(), mustOverrides(t, []string{"done=Shipped"}, nil))
	if err == nil || !strings.Contains(err.Error(), "--state done=Shipped") || !strings.Contains(err.Error(), "available: Backlog, Todo") {
		t.Fatalf("err = %v; want the typo named with the available states", err)
	}
}

func TestInitRolesMissingLabels(t *testing.T) {
	labels := defaultRoleLabels()[2:] // agent-build and needs-human absent
	explicit := mustOverrides(t, nil, []string{"needsHuman=Needs Human"})
	m, err := mapLinearRoles(defaultRoleStates(), labels, explicit)
	if err != nil {
		t.Fatalf("mapLinearRoles: %v", err)
	}
	if len(m.Missing) != 2 || m.Missing[0] != (missingLabel{"agentBuild", "agent-build"}) || m.Missing[1] != (missingLabel{"needsHuman", "Needs Human"}) {
		t.Fatalf("missing = %v", m.Missing)
	}
	// An explicit choice survives even before it exists; a default stays unwritten.
	if want := map[string]string{"needsHuman": "Needs Human"}; !maps.Equal(m.Overrides.Labels, want) {
		t.Errorf("labels = %v; want %v", m.Overrides.Labels, want)
	}
}

func TestInitRolesLabelPreference(t *testing.T) {
	labels := []linearLabel{{ID: "ws", Name: "EPIC"}, {ID: "team", Name: "Epic", TeamID: "t"}}
	if got, _ := pickLabel(labels, "epic"); got.ID != "team" {
		t.Errorf("picked %s; want the team label over the workspace one", got.ID)
	}
	labels = append(labels, linearLabel{ID: "exact", Name: "epic"})
	if got, _ := pickLabel(labels, "epic"); got.ID != "exact" {
		t.Errorf("picked %s; want the byte-identical name", got.ID)
	}
}

func TestInitRolesLookupNamesIncludeExplicitLabels(t *testing.T) {
	names := linearLabelLookupNames(mustOverrides(t, nil, []string{"epic=Initiative", "agentBuild=agent-build"}))
	if strings.Join(names, ",") != "agent-build,needs-human,agent-plan,agent-question,epic,Initiative" {
		t.Fatalf("names = %v", names)
	}
}
