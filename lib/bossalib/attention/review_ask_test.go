package attention

import (
	"reflect"
	"testing"
)

func TestReviewAsk(t *testing.T) {
	for _, tt := range []struct {
		name           string
		codes          []string
		label          string
		evidence       bool
		head, question string
		items          []string
		ok             bool
	}{
		{"label", []string{"always-human-path"}, "the verify judge (cannot grade itself)", false, "abc123456", "Approve these on head abc1234:", []string{"the verify judge (cannot grade itself)"}, true},
		{"categories", []string{"human-review:ui", "human-review:database", "human-review:ui"}, "", true, "abc123456", "Watch the evidence video, then approve these on head abc1234:", []string{"database migration", "UI change"}, true},
		{"merge", []string{"human-merge"}, "", true, "abc123456", "Verify passed head abc1234. Merge it when you are ready.", nil, true},
		{"empty head", []string{"human-review:ui"}, "", false, "", "Approve these on this head:", []string{"UI change"}, true},
		{"failure wins", []string{"human-review:ui", "repair-exhausted"}, "", false, "abc1234", "", nil, false},
		{"unknown", []string{"unknown"}, "", false, "", "", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q, items, ok := ReviewAsk(tt.codes, tt.label, tt.evidence, tt.head)
			if q != tt.question || !reflect.DeepEqual(items, tt.items) || ok != tt.ok {
				t.Fatalf("got %q %v %v; want %q %v %v", q, items, ok, tt.question, tt.items, tt.ok)
			}
		})
	}
}
