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

func TestPlainOpenItem(t *testing.T) {
	for in, want := range map[string]string{
		// What park notes record, backticks already dropped.
		"## Acceptance criteria is missing.":                                        "The Acceptance criteria section is missing.",
		"## Open questions appears more than once, so verify cannot read it.":       "The Open questions section appears more than once, so it cannot be read.",
		"## Human follow-up is not a checklist of - [ ] / - [x] items (or - none).": "The Human follow-up section is not a checklist.",
		"## Acceptance criteria is not a checklist of - [ ] / - [x] items.":         "The Acceptance criteria section is not a checklist.",
		"`## Human follow-up` is missing.":                                          "The Human follow-up section is missing.",
		"## Open questions section is missing.":                                     "The Open questions section is missing.",
		"## Rollout Section appears more than once.":                                "The Rollout section appears more than once, so it cannot be read.",
		"## is missing.":           "A section with no name is missing.",
		"`##` is not a checklist.": "A section with no name is not a checklist.",
		// Any other leading heading loses only its marker.
		"### Rollout notes":           "Rollout notes",
		"`## Rollout` needs `a` look": "Rollout needs `a` look",
		// Current wording and quoted items pass through.
		"The Acceptance criteria section is missing.": "The Acceptance criteria section is missing.",
		"Open question: is ## a heading?":             "Open question: is ## a heading?",
		"Unmet acceptance criterion: ## is missing.":  "Unmet acceptance criterion: ## is missing.",
		"#hashtag is missing.":                        "#hashtag is missing.",
	} {
		if got := PlainOpenItem(in); got != want {
			t.Errorf("PlainOpenItem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseOpenItemsSaysSectionHeadingsInWords(t *testing.T) {
	got, ok := ParseOpenItems(`<!-- boss-open-items: {"head":"h","items":["## Acceptance criteria is missing.","Open question: which region?"]} -->`)
	want := []string{"The Acceptance criteria section is missing.", "Open question: which region?"}
	if !ok || !slices.Equal(got.Items, want) {
		t.Fatalf("ParseOpenItems = %q (ok %v), want %q", got.Items, ok, want)
	}
}
