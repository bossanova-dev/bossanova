package evidence

import (
	"slices"
	"strings"
	"testing"
)

func TestParseOpenItems(t *testing.T) {
	head := strings.Repeat("ab", 20)
	for name, tc := range map[string]struct {
		body  string
		want  []string
		valid bool
	}{
		"one item": {
			body:  "note\n\n<!-- boss-open-items: {\"head\":\"" + head + "\",\"items\":[\"Open human follow-up: watch  the\\nrollout\"]} -->\n<!-- boss-park-note -->",
			want:  []string{"Open human follow-up: watch the rollout"},
			valid: true,
		},
		"the last well-formed line wins": {
			body:  "<!-- boss-open-items: {\"head\":\"" + head + "\",\"items\":[\"old\"]} -->\n<!-- boss-open-items: {\"head\":\"" + head + "\",\"items\":[\"new\"]} -->\n<!-- boss-open-items: not json -->",
			want:  []string{"new"},
			valid: true,
		},
		"escaped angle brackets decode": {
			body:  "<!-- boss-open-items: {\"head\":\"" + head + "\",\"items\":[\"a \\u003c-- b \\u003e c\"]} -->",
			want:  []string{"a <-- b > c"},
			valid: true,
		},
		"no head":        {body: "<!-- boss-open-items: {\"items\":[\"x\"]} -->"},
		"only blanks":    {body: "<!-- boss-open-items: {\"head\":\"" + head + "\",\"items\":[\" \",\"\"]} -->"},
		"unterminated":   {body: "<!-- boss-open-items: {\"head\":\"" + head + "\",\"items\":[\"x\"]}"},
		"no line at all": {body: "<!-- boss-evidence: {} -->"},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := ParseOpenItems(tc.body)
			if ok != tc.valid || !slices.Equal(got.Items, tc.want) || (ok && got.Head != head) {
				t.Fatalf("ParseOpenItems = %+v, %v; want %q, %v", got, ok, tc.want, tc.valid)
			}
		})
	}
}

func TestParseOpenItemsCapsTheList(t *testing.T) {
	items := make([]string, MaxOpenItems+5)
	for i := range items {
		items[i] = `"item"`
	}
	got, ok := ParseOpenItems(`<!-- boss-open-items: {"head":"h","items":[` + strings.Join(items, ",") + `]} -->`)
	if !ok || len(got.Items) != MaxOpenItems {
		t.Fatalf("kept %d items (ok %v), want %d", len(got.Items), ok, MaxOpenItems)
	}
}
