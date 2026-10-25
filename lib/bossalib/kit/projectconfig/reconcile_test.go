package projectconfig

import (
	"reflect"
	"testing"
)

func TestReconcile(t *testing.T) {
	const own = "boss-project:app"
	const other = "boss-project:other"

	// row is a cron-like test row: identity key, fallback key (a factory
	// prompt token), marker, enabled and the one field the diff compares.
	type row struct {
		key, fallback, marker, schedule string
		disabled                        bool
	}
	desired := func(key, fallback, schedule string) row { return row{key: key, fallback: fallback, schedule: schedule} }
	observed := func(key, fallback, marker, schedule string) row {
		return row{key: key, fallback: fallback, marker: marker, schedule: schedule}
	}
	disabledRow := func(r row) row { r.disabled = true; return r }

	// run builds the classifier input from typed rows, the way a resource
	// planner does, with a schedule field diff.
	run := func(want, live []row, prune bool) []Change {
		in := ReconcileInput{Resource: ResourceCron, Marker: own, Prune: prune}
		for _, d := range want {
			in.Desired = append(in.Desired, DesiredRow{Key: d.key, Fallback: d.fallback})
		}
		for _, o := range live {
			in.Observed = append(in.Observed, ObservedRow{Key: o.key, Fallback: o.fallback, Marker: o.marker, Enabled: !o.disabled})
		}
		in.Diff = func(di, oi int) []FieldDiff {
			if want[di].schedule == live[oi].schedule {
				return nil
			}
			return []FieldDiff{{Field: "schedule", Old: live[oi].schedule, New: want[di].schedule}}
		}
		return Reconcile(in)
	}

	schedDiff := []FieldDiff{{Field: "schedule", Old: "0 * * * *", New: "*/5 * * * *"}}

	cases := []struct {
		name     string
		desired  []row
		observed []row
		prune    bool
		want     []Change
	}{
		{
			name:    "no observed match creates",
			desired: []row{desired("nightly", "", "0 * * * *")},
			observed: []row{
				observed("other-job", "", own, "0 * * * *"),
			},
			want: []Change{
				{Resource: ResourceCron, Identity: "nightly", Action: ActionCreate},
				{Resource: ResourceCron, Identity: "other-job", Action: ActionDisable},
			},
		},
		{
			name:     "own marker without differences is unchanged",
			desired:  []row{desired("nightly", "", "0 * * * *")},
			observed: []row{observed("nightly", "", own, "0 * * * *")},
			want:     []Change{{Resource: ResourceCron, Identity: "nightly", Action: ActionUnchanged}},
		},
		{
			name:     "own marker with a schedule change updates",
			desired:  []row{desired("nightly", "", "*/5 * * * *")},
			observed: []row{observed("nightly", "", own, "0 * * * *")},
			want:     []Change{{Resource: ResourceCron, Identity: "nightly", Action: ActionUpdate, Fields: schedDiff}},
		},
		{
			name:     "unmarked row with the same name is adopted",
			desired:  []row{desired("nightly", "", "*/5 * * * *")},
			observed: []row{observed("nightly", "", "", "0 * * * *")},
			want:     []Change{{Resource: ResourceCron, Identity: "nightly", Action: ActionAdopt, Fields: schedDiff}},
		},
		{
			name:     "unmarked row with no differences is still adopted",
			desired:  []row{desired("nightly", "", "0 * * * *")},
			observed: []row{observed("nightly", "", "", "0 * * * *")},
			want:     []Change{{Resource: ResourceCron, Identity: "nightly", Action: ActionAdopt}},
		},
		{
			name:     "unmarked factory job adopted through the fallback key",
			desired:  []row{desired("factory-build", "/boss-build", "*/5 * * * *")},
			observed: []row{observed("Build tickets", "/boss-build", "", "0 * * * *")},
			want:     []Change{{Resource: ResourceCron, Identity: "factory-build", Action: ActionAdopt, Fields: schedDiff}},
		},
		{
			name:    "fallback is not consulted when the name matched",
			desired: []row{desired("factory-build", "/boss-build", "0 * * * *")},
			observed: []row{
				observed("factory-build", "", own, "0 * * * *"),
				observed("Build tickets", "/boss-build", "", "0 * * * *"),
			},
			want: []Change{{Resource: ResourceCron, Identity: "factory-build", Action: ActionUnchanged}},
		},
		{
			name:    "a row another desired row matched by name is not a fallback candidate",
			desired: []row{desired("factory-build", "/boss-build", "0 * * * *"), desired("build-copy", "/boss-build", "0 * * * *")},
			observed: []row{
				observed("factory-build", "/boss-build", own, "0 * * * *"),
			},
			want: []Change{
				{Resource: ResourceCron, Identity: "factory-build", Action: ActionUnchanged},
				{Resource: ResourceCron, Identity: "build-copy", Action: ActionCreate},
			},
		},
		{
			name:     "another project's marker conflicts",
			desired:  []row{desired("nightly", "", "0 * * * *")},
			observed: []row{observed("nightly", "", other, "0 * * * *")},
			want: []Change{{Resource: ResourceCron, Identity: "nightly", Action: ActionConflict,
				Reason: ReasonManagedByOther}},
		},
		{
			name:    "two observed rows matching one desired row conflict as ambiguous",
			desired: []row{desired("nightly", "", "0 * * * *")},
			observed: []row{
				observed("nightly", "", own, "0 * * * *"),
				observed("nightly", "", "", "0 * * * *"),
			},
			want: []Change{{Resource: ResourceCron, Identity: "nightly", Action: ActionConflict, Reason: ReasonAmbiguous}},
		},
		{
			name:    "two fallback candidates conflict as ambiguous",
			desired: []row{desired("factory-build", "/boss-build", "0 * * * *")},
			observed: []row{
				observed("Build A", "/boss-build", "", "0 * * * *"),
				observed("Build B", "/boss-build", "", "0 * * * *"),
			},
			want: []Change{{Resource: ResourceCron, Identity: "factory-build", Action: ActionConflict, Reason: ReasonAmbiguous}},
		},
		{
			name: "two desired rows sharing a fallback key never both claim one live row",
			desired: []row{
				desired("factory-build", "/boss-build", "0 * * * *"),
				desired("factory-build-2", "/boss-build", "0 * * * *"),
			},
			observed: []row{observed("Build", "/boss-build", "", "0 * * * *")},
			want: []Change{
				{Resource: ResourceCron, Identity: "factory-build", Action: ActionConflict, Reason: ReasonAmbiguous},
				{Resource: ResourceCron, Identity: "factory-build-2", Action: ActionConflict, Reason: ReasonAmbiguous},
			},
		},
		{
			name:     "own row dropped from the file is disabled",
			observed: []row{observed("old", "", own, "0 * * * *")},
			desired:  []row{},
			want:     []Change{{Resource: ResourceCron, Identity: "old", Action: ActionDisable}},
		},
		{
			name:     "own row dropped and already disabled is unchanged",
			observed: []row{disabledRow(observed("old", "", own, "0 * * * *"))},
			want:     []Change{{Resource: ResourceCron, Identity: "old", Action: ActionUnchanged}},
		},
		{
			name: "own rows dropped are deleted only under prune",
			observed: []row{
				observed("old", "", own, "0 * * * *"),
				disabledRow(observed("older", "", own, "0 * * * *")),
			},
			prune: true,
			want: []Change{
				{Resource: ResourceCron, Identity: "old", Action: ActionDelete},
				{Resource: ResourceCron, Identity: "older", Action: ActionDelete},
			},
		},
		{
			name: "unmarked and foreign rows absent from the file never appear",
			observed: []row{
				observed("manual", "", "", "0 * * * *"),
				observed("theirs", "", other, "0 * * * *"),
			},
			prune: true,
			want:  nil,
		},
		{
			name: "empty desired set disables every own row",
			observed: []row{
				observed("a", "", own, "0 * * * *"),
				observed("manual", "", "", "0 * * * *"),
				observed("b", "/boss-plan", own, "0 * * * *"),
			},
			desired: []row{},
			want: []Change{
				{Resource: ResourceCron, Identity: "a", Action: ActionDisable},
				{Resource: ResourceCron, Identity: "b", Action: ActionDisable},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := run(tc.desired, tc.observed, tc.prune)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Reconcile:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}

	t.Run("a nil diff callback reports no field differences", func(t *testing.T) {
		got := Reconcile(ReconcileInput{
			Resource: ResourceWebhook, Marker: own,
			Desired:  []DesiredRow{{Key: "hook"}},
			Observed: []ObservedRow{{Key: "hook", Marker: own, Enabled: true}},
		})
		want := []Change{{Resource: ResourceWebhook, Identity: "hook", Action: ActionUnchanged}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}
