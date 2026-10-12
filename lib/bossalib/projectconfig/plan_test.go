package projectconfig

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPlanRender(t *testing.T) {
	t.Run("converged", func(t *testing.T) {
		cases := []struct {
			name string
			plan Plan
			want bool
		}{
			{"empty plan", Plan{}, true},
			{"all unchanged", Plan{Changes: []Change{
				{Resource: ResourceCron, Identity: "a", Action: ActionUnchanged},
				{Resource: ResourceRepo, Identity: "r", Action: ActionUnchanged},
			}}, true},
			{"warnings only", Plan{Warnings: []string{"unknown key"}}, true},
		}
		for _, a := range []Action{ActionCreate, ActionUpdate, ActionAdopt, ActionDisable, ActionDelete, ActionConflict, ActionSkip} {
			cases = append(cases, struct {
				name string
				plan Plan
				want bool
			}{"one " + string(a), Plan{Changes: []Change{
				{Resource: ResourceCron, Identity: "a", Action: ActionUnchanged},
				{Resource: ResourceCron, Identity: "b", Action: a},
			}}, false})
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.plan.Converged(); got != tc.want {
					t.Fatalf("Converged() = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("text omits unchanged and sorts by resource, daemon, identity", func(t *testing.T) {
		changes := []Change{
			{Resource: ResourceWebhook, Identity: "hook", Action: ActionCreate},
			{Resource: ResourceCron, Identity: "zeta", Daemon: "laptop", Action: ActionUpdate},
			{Resource: ResourceCron, Identity: "quiet", Daemon: "laptop", Action: ActionUnchanged},
			{Resource: ResourceCron, Identity: "alpha", Daemon: "server", Action: ActionDisable},
			{Resource: ResourceTrigger, Identity: "deploy", Action: ActionConflict, Reason: ReasonManagedByOther},
			{Resource: ResourceCron, Identity: "beta", Daemon: "laptop", Action: ActionAdopt},
			{Resource: ResourceRepo, Identity: "github.com/acme/app", Action: ActionUpdate},
		}
		original := append([]Change(nil), changes...)
		want := []string{"github.com/acme/app", "beta", "zeta", "alpha", "deploy", "hook"}

		// Every input order yields the same rendering.
		var first string
		for i := range changes {
			rotated := append(append([]Change(nil), changes[i:]...), changes[:i]...)
			text := Plan{Changes: rotated}.Text()
			if i == 0 {
				first = text
			} else if text != first {
				t.Fatalf("rendering depends on input order:\n%s\nvs\n%s", first, text)
			}
		}
		if !reflect.DeepEqual(changes, original) {
			t.Fatal("Text sorted the caller's slice")
		}

		lines := strings.Split(strings.TrimRight(first, "\n"), "\n")
		if len(lines) != len(want) {
			t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), first)
		}
		for i, id := range want {
			if !strings.Contains(lines[i], id) {
				t.Errorf("line %d = %q, want identity %q", i, lines[i], id)
			}
		}
		if strings.Contains(first, "quiet") {
			t.Errorf("unchanged change rendered:\n%s", first)
		}
		if !strings.Contains(lines[4], ReasonManagedByOther) {
			t.Errorf("conflict line %q does not carry its reason", lines[4])
		}
	})

	t.Run("text renders warnings", func(t *testing.T) {
		text := Plan{Warnings: []string{"daemons[0]: no live daemon"}}.Text()
		if !strings.Contains(text, "daemons[0]: no live daemon") {
			t.Fatalf("warning missing from %q", text)
		}
	})

	t.Run("secret fields are redacted in text and JSON", func(t *testing.T) {
		const oldValue, newValue = "lin_api_OLDVALUE123", "lin_api_NEWVALUE456"
		plan := mixedPlan(oldValue, newValue)
		text := plan.Text()
		rendered, err := plan.JSON()
		if err != nil {
			t.Fatal(err)
		}
		marshalled, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		outputs := map[string]string{"text": text, "json": string(rendered), "json.Marshal": string(marshalled)}
		for name, out := range outputs {
			for _, v := range []string{oldValue, newValue} {
				if strings.Contains(out, v) {
					t.Errorf("%s output leaks secret value %q:\n%s", name, v, out)
				}
			}
		}
		for _, name := range []string{"text", "json"} {
			if !strings.Contains(outputs[name], SecretPlaceholder) {
				t.Errorf("%s output has no %s placeholder:\n%s", name, SecretPlaceholder, outputs[name])
			}
		}
		// The non-secret field still renders its values.
		if !strings.Contains(text, "*/5 * * * *") {
			t.Errorf("non-secret value missing from text:\n%s", text)
		}
		t.Logf("text:\n%s", text)
		t.Logf("json:\n%s", rendered)
	})

	t.Run("json uses lower-camel keys and round-trips", func(t *testing.T) {
		plan := Plan{
			Changes: []Change{{
				Resource: ResourceCron, Identity: "factory-build", Daemon: "laptop",
				Action: ActionUpdate, Reason: ReasonDaemonTooOld, Detail: "schedule drift",
				Fields:            []FieldDiff{{Field: "schedule", Old: "0 * * * *", New: "*/5 * * * *"}},
				NeedsConfirmation: true,
			}},
			Warnings: []string{"heads up"},
		}
		data, err := plan.JSON()
		if err != nil {
			t.Fatal(err)
		}
		var generic map[string]any
		if err := json.Unmarshal(data, &generic); err != nil {
			t.Fatal(err)
		}
		assertKeys(t, generic, "changes", "warnings")
		change := generic["changes"].([]any)[0].(map[string]any)
		assertKeys(t, change, "resource", "identity", "daemon", "action", "reason", "detail", "fields", "needsConfirmation")

		var back Plan
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, plan) {
			t.Fatalf("round trip:\n got %+v\nwant %+v", back, plan)
		}
	})

	t.Run("json always carries change and warning arrays", func(t *testing.T) {
		data, err := json.Marshal(Plan{})
		if err != nil {
			t.Fatal(err)
		}
		var generic map[string]any
		if err := json.Unmarshal(data, &generic); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"changes", "warnings"} {
			if _, ok := generic[k].([]any); !ok {
				t.Errorf("%s = %v, want an array", k, generic[k])
			}
		}
	})
}

// mixedPlan is a plan with every kind of line, including one secret field.
func mixedPlan(oldSecret, newSecret string) Plan {
	return Plan{
		Changes: []Change{
			{Resource: ResourceCron, Identity: "nightly", Daemon: "laptop", Action: ActionUnchanged},
			{Resource: ResourceCron, Identity: "factory-build", Daemon: "laptop", Action: ActionUpdate,
				Fields: []FieldDiff{{Field: "schedule", Old: "0 * * * *", New: "*/5 * * * *"}}},
			{Resource: ResourceRepo, Identity: "github.com/acme/app", Action: ActionUpdate, NeedsConfirmation: true,
				Fields: []FieldDiff{{Field: "secrets.linearApiKey", Old: oldSecret, New: newSecret, Secret: true}}},
			{Resource: ResourceTrigger, Identity: "deploy", Action: ActionConflict, Reason: ReasonManagedByOther,
				Detail: "owned by boss-project:other"},
			{Resource: ResourceCron, Identity: "factory-plan", Daemon: "server", Action: ActionSkip, Reason: ReasonDaemonOffline},
		},
		Warnings: []string{"daemons[1]: server is offline"},
	}
}

func assertKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in %v", k, m)
		}
	}
	if len(m) != len(keys) {
		t.Errorf("got keys %v, want exactly %v", reflect.ValueOf(m).MapKeys(), keys)
	}
}
