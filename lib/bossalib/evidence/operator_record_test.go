package evidence

import (
	"slices"
	"strings"
	"testing"
)

func TestParseOperatorRecord(t *testing.T) {
	head := strings.Repeat("ab", 20)
	for name, tc := range map[string]struct {
		body string
		want OperatorRecord
		ok   bool
	}{
		"a send-back":                   {body: "**Sent back**\n\n<!-- boss-send-back head=" + head + " -->\n", want: OperatorRecord{Head: head, IsSendBack: true}, ok: true},
		"an approval waiving codes":     {body: "<!-- boss-approve head=" + head + " waives=ledger-open,no-receipt -->", want: OperatorRecord{Head: head, Waives: []string{"ledger-open", "no-receipt"}}, ok: true},
		"an approval waiving nothing":   {body: "<!-- boss-approve head=" + head + " -->", want: OperatorRecord{Head: head}, ok: true},
		"the head is lower-cased":       {body: "<!-- boss-send-back head=" + strings.ToUpper(head) + " -->", want: OperatorRecord{Head: head, IsSendBack: true}, ok: true},
		"a send-back naming codes":      {body: "<!-- boss-send-back head=" + head + " waives=ledger-open -->"},
		"an empty code in the list":     {body: "<!-- boss-approve head=" + head + " waives=ledger-open,,no-receipt -->"},
		"a marker that is not the last": {body: "<!-- boss-send-back head=" + head + " -->\nmore text"},
		"a short head":                  {body: "<!-- boss-send-back head=abc -->"},
		"another boss line":             {body: "<!-- boss-park-note head=" + head + " digest=0123456789abcdef -->"},
	} {
		got, ok := ParseOperatorRecord(tc.body)
		if ok != tc.ok || got.Head != tc.want.Head || got.IsSendBack != tc.want.IsSendBack || !slices.Equal(got.Waives, tc.want.Waives) {
			t.Errorf("%s: ParseOperatorRecord = %+v, %v; want %+v, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}
