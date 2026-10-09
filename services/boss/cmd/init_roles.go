package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// --- Pure Linear role mapping ---------------------------------------------
//
// The skills compare state and label names with case-sensitive equality, so a
// team whose states or labels differ from the defaults (even only in case)
// needs those names written into trackerConfig.linear. These functions decide
// what to write from what the admin client found; they do no I/O.

// linearStateRoles and linearLabelRoles fix the order roles are reported in.
var (
	linearStateRoles = []string{"unplanned", "planned", "inProgress", "inReview", "done"}
	linearLabelRoles = []string{"agentBuild", "needsHuman", "agentPlan", "agentQuestion", "epic"}
)

// linearStateRoleTypes is the Linear state type each state role expects. A
// mismatch is a warning, not an error: the mapping is still accepted.
var linearStateRoleTypes = map[string][]string{
	"unplanned":  {"backlog", "unstarted", "triage"},
	"planned":    {"unstarted"},
	"inProgress": {"started"},
	"inReview":   {"started"},
	"done":       {"completed"},
}

// errPlannedIsUnplanned is the one hard mapping error: boss-build would pick
// up work boss-plan never planned.
var errPlannedIsUnplanned = errors.New("the unplanned and planned state roles map to the same Linear state")

// unresolvedRoleError is a role with no case-insensitive match and no explicit
// flag. An interactive caller can catch it and prompt instead.
type unresolvedRoleError struct {
	kind, role, want string
	available        []string
}

func (e *unresolvedRoleError) Error() string {
	available := "none"
	if len(e.available) > 0 {
		available = strings.Join(e.available, ", ")
	}
	return fmt.Sprintf("Linear %s role %s: no %s named %q (case-insensitive) and no --%s %s=<name>; available: %s",
		e.kind, e.role, e.kind, e.want, e.kind, e.role, available)
}

// missingLabel is a label role whose name exists nowhere the team can see.
type missingLabel struct {
	Role, Name string
}

// linearRoleMapping is the mapping outcome. Overrides carries explicit's team,
// key and assignee choice, plus every state and label role whose chosen name is
// not byte-identical to its default.
type linearRoleMapping struct {
	Overrides linearOverrides
	Missing   []missingLabel
	Warnings  []string
}

// linearLabelLookupNames is every label name FindLabels has to resolve: each
// role's default plus any explicit --label choice.
func linearLabelLookupNames(explicit linearOverrides) []string {
	names := make([]string, 0, len(linearLabelRoles))
	for _, role := range linearLabelRoles {
		names = append(names, linearLabelDefaults[role])
		if name, ok := explicit.Labels[role]; ok && name != linearLabelDefaults[role] {
			names = append(names, name)
		}
	}
	return names
}

// mapLinearRoles maps every state and label role onto the team's real names.
// Case-insensitive matching only picks the candidate; the candidate's own name
// is written whenever it is not byte-identical to the default. Explicit flag
// choices win and are validated against the team's states.
func mapLinearRoles(states []linearState, labels []linearLabel, explicit linearOverrides) (linearRoleMapping, error) {
	ov := explicit
	ov.States, ov.Labels = map[string]string{}, map[string]string{}
	m := linearRoleMapping{}

	chosen := map[string]linearState{}
	for _, role := range linearStateRoles {
		want, flagged := explicit.States[role]
		if !flagged {
			want = linearStateDefaults[role]
		}
		state, ok := pickState(states, want)
		if !ok {
			if flagged {
				return m, fmt.Errorf("--state %s=%s names no workflow state of this team; available: %s", role, want, stateNames(states))
			}
			return m, &unresolvedRoleError{kind: "state", role: role, want: want, available: stateNamesList(states)}
		}
		chosen[role] = state
		if state.Name != linearStateDefaults[role] {
			ov.States[role] = state.Name
		}
		if !slices.Contains(linearStateRoleTypes[role], state.Type) {
			m.Warnings = append(m.Warnings, fmt.Sprintf("state role %s maps to %q, a %s state; expected %s",
				role, state.Name, typeOrUnknown(state.Type), strings.Join(linearStateRoleTypes[role], " or ")))
		}
	}
	if sameState(chosen["unplanned"], chosen["planned"]) {
		return m, fmt.Errorf("%w (%q); boss-build would pick up unplanned work", errPlannedIsUnplanned, chosen["planned"].Name)
	}
	for i, a := range linearStateRoles {
		for _, b := range linearStateRoles[i+1:] {
			if (a == "unplanned" && b == "planned") || !sameState(chosen[a], chosen[b]) {
				continue
			}
			m.Warnings = append(m.Warnings, fmt.Sprintf("state roles %s and %s both map to %q", a, b, chosen[a].Name))
		}
	}

	for _, role := range linearLabelRoles {
		want, flagged := explicit.Labels[role]
		if !flagged {
			want = linearLabelDefaults[role]
		}
		label, ok := pickLabel(labels, want)
		if !ok {
			m.Missing = append(m.Missing, missingLabel{Role: role, Name: want})
			if want != linearLabelDefaults[role] {
				ov.Labels[role] = want
			}
			continue
		}
		if label.Name != linearLabelDefaults[role] {
			ov.Labels[role] = label.Name
		}
	}
	m.Overrides = ov
	return m, nil
}

// pickState returns the state named want case-insensitively, preferring a
// byte-identical name.
func pickState(states []linearState, want string) (linearState, bool) {
	var found linearState
	ok := false
	for _, s := range states {
		if s.Name == want {
			return s, true
		}
		if !ok && strings.EqualFold(s.Name, want) {
			found, ok = s, true
		}
	}
	return found, ok
}

// pickLabel returns the label named want case-insensitively, preferring a
// byte-identical name, then a team label over a workspace one.
func pickLabel(labels []linearLabel, want string) (linearLabel, bool) {
	var found linearLabel
	ok := false
	for _, l := range labels {
		if l.Name == want {
			return l, true
		}
		if strings.EqualFold(l.Name, want) && (!ok || (found.TeamID == "" && l.TeamID != "")) {
			found, ok = l, true
		}
	}
	return found, ok
}

func sameState(a, b linearState) bool {
	if a.ID != "" || b.ID != "" {
		return a.ID == b.ID
	}
	return a.Name == b.Name
}

func stateNamesList(states []linearState) []string {
	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, s.Name)
	}
	return names
}

func stateNames(states []linearState) string {
	if len(states) == 0 {
		return "none"
	}
	return strings.Join(stateNamesList(states), ", ")
}

func typeOrUnknown(t string) string {
	if t == "" {
		return "untyped"
	}
	return t
}
