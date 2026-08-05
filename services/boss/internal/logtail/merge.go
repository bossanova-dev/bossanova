package logtail

import (
	"sort"
	"time"
)

// Merge interleaves source record groups in timestamp order. Untimestamped
// records inherit the preceding record's timestamp from their own source.
func Merge(groups [][]Record) []Record {
	type keyed struct {
		rec          Record
		at           time.Time
		group, index int
	}
	var all []keyed
	for groupIndex, group := range groups {
		var previous time.Time
		for index, rec := range group {
			at := rec.Time
			if at.IsZero() {
				at = previous
			} else {
				previous = at
			}
			all = append(all, keyed{rec, at, groupIndex, index})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.Before(all[j].at)
		}
		if all[i].group != all[j].group {
			return all[i].group < all[j].group
		}
		return all[i].index < all[j].index
	})
	out := make([]Record, 0, len(all))
	for _, record := range all {
		out = append(out, record.rec)
	}
	return out
}
