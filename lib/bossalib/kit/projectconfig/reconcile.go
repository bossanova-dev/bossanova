package projectconfig

// DesiredRow describes one row the file wants. Key is its identity (a cron or
// trigger name, a webhook URL); Fallback, when set, is a second key consulted
// only when Key matches no observed row (a factory stage's prompt token).
type DesiredRow struct {
	Key      string
	Fallback string
}

// ObservedRow describes one live row: its identity key, fallback key,
// ownership marker (empty when unmarked) and whether it is enabled.
type ObservedRow struct {
	Key      string
	Fallback string
	Marker   string
	Enabled  bool
}

// FieldDiffFunc returns the field differences between desired row d and
// observed row o, by index into ReconcileInput's slices. It is the only
// resource-specific part of classification.
type FieldDiffFunc func(d, o int) []FieldDiff

// ReconcileInput is everything Reconcile classifies.
type ReconcileInput struct {
	Resource Resource
	Desired  []DesiredRow
	Observed []ObservedRow
	// Marker is this project's ownership marker (see Marker).
	Marker string
	// Prune deletes own rows absent from the file instead of disabling them.
	Prune bool
	// Diff compares a matched pair; nil means "no differences".
	Diff FieldDiffFunc
}

// Reconcile classifies desired rows against observed rows. For each desired
// row, in order: no candidate is create; several, or one live row that two
// desired rows reach through a shared fallback key, are conflict/ambiguous for
// each of them, so no live row is claimed twice; one with this project's
// marker is update (with field diffs) or unchanged; one unmarked is adopt
// (carrying field diffs); one with another marker is
// conflict/managed-by-other. Then each observed row with this project's
// marker that no desired row matched is disable (unchanged when already
// disabled), or delete under Prune. Unmarked and foreign rows absent from the
// file never appear.
//
// Changes carry Resource, Identity, Action, Reason and Fields; callers fill
// Daemon, Detail and NeedsConfirmation. Reconcile does no I/O.
func Reconcile(in ReconcileInput) []Change {
	// A row some desired row matches by identity key is never another
	// desired row's fallback candidate.
	keyMatched := make([]bool, len(in.Observed))
	for _, d := range in.Desired {
		for oi, o := range in.Observed {
			if o.Key == d.Key {
				keyMatched[oi] = true
			}
		}
	}

	// Resolve every desired row's candidates first, counting how many desired
	// rows reach each observed row through the fallback key: a live row two
	// desired rows could both adopt is ambiguous for each of them.
	candidatesOf := make([][]int, len(in.Desired))
	fallbackClaims := make([]int, len(in.Observed))
	for di, d := range in.Desired {
		candidates := in.candidates(func(_ int, o ObservedRow) bool { return o.Key == d.Key })
		if len(candidates) == 0 && d.Fallback != "" {
			candidates = in.candidates(func(oi int, o ObservedRow) bool {
				return !keyMatched[oi] && o.Fallback == d.Fallback
			})
			for _, oi := range candidates {
				fallbackClaims[oi]++
			}
		}
		candidatesOf[di] = candidates
	}

	matched := make([]bool, len(in.Observed))
	var changes []Change
	for di, d := range in.Desired {
		candidates := candidatesOf[di]
		shared := false
		for _, oi := range candidates {
			matched[oi] = true
			shared = shared || fallbackClaims[oi] > 1
		}
		change := Change{Resource: in.Resource, Identity: d.Key}
		switch {
		case len(candidates) == 0:
			change.Action = ActionCreate
		case len(candidates) == 1 && !shared:
			oi := candidates[0]
			switch marker := in.Observed[oi].Marker; marker {
			case in.Marker:
				change.Fields = in.diff(di, oi)
				change.Action = ActionUnchanged
				if change.Fields != nil {
					change.Action = ActionUpdate
				}
			case "":
				change.Action = ActionAdopt
				change.Fields = in.diff(di, oi)
			default:
				change.Action = ActionConflict
				change.Reason = ReasonManagedByOther
			}
		default:
			change.Action = ActionConflict
			change.Reason = ReasonAmbiguous
		}
		changes = append(changes, change)
	}

	for oi, o := range in.Observed {
		if matched[oi] || o.Marker != in.Marker {
			continue
		}
		change := Change{Resource: in.Resource, Identity: o.Key, Action: ActionDisable}
		switch {
		case in.Prune:
			change.Action = ActionDelete
		case !o.Enabled:
			change.Action = ActionUnchanged
		}
		changes = append(changes, change)
	}
	return changes
}

// candidates returns the indexes of observed rows satisfying ok.
func (in ReconcileInput) candidates(ok func(oi int, o ObservedRow) bool) []int {
	var out []int
	for oi, o := range in.Observed {
		if ok(oi, o) {
			out = append(out, oi)
		}
	}
	return out
}

// diff runs the field-diff callback, normalising "no differences" to nil.
func (in ReconcileInput) diff(d, o int) []FieldDiff {
	if in.Diff == nil {
		return nil
	}
	fields := in.Diff(d, o)
	if len(fields) == 0 {
		return nil
	}
	return fields
}
