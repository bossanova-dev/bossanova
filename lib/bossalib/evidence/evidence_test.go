package evidence_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/evidence"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want evidence.Summary
		ok   bool
	}{
		{
			name: "valid line inside a comment",
			body: "<!-- boss-verify-evidence -->\n" +
				`<!-- boss-evidence: {"head":"` + sha + `","video":"https://evidence.example/v.mp4","videoMs":160000,"proven":5,"total":5} -->` +
				"\n## Verification evidence\n",
			want: evidence.Summary{Head: sha, VideoURL: "https://evidence.example/v.mp4", VideoDuration: 160 * time.Second, Proven: 5, Total: 5},
			ok:   true,
		},
		{
			name: "video and length are optional",
			body: `<!-- boss-evidence: {"head":"` + sha + `","proven":0,"total":0} -->`,
			want: evidence.Summary{Head: sha},
			ok:   true,
		},
		{
			name: "missing head is rejected",
			body: `<!-- boss-evidence: {"video":"https://evidence.example/v.mp4","proven":1,"total":1} -->`,
		},
		{
			name: "missing counts are rejected",
			body: `<!-- boss-evidence: {"head":"` + sha + `"} -->`,
		},
		{
			name: "malformed JSON is rejected",
			body: `<!-- boss-evidence: {"head":"` + sha + `","proven":5,"total": -->`,
		},
		{
			name: "proven above total is rejected",
			body: `<!-- boss-evidence: {"head":"` + sha + `","proven":6,"total":5} -->`,
		},
		{
			name: "negative length is rejected",
			body: `<!-- boss-evidence: {"head":"` + sha + `","videoMs":-1,"proven":1,"total":1} -->`,
		},
		{
			name: "non-http video is rejected",
			body: `<!-- boss-evidence: {"head":"` + sha + `","video":"javascript:alert(1)","proven":1,"total":1} -->`,
		},
		{
			name: "unclosed line is rejected",
			body: `<!-- boss-evidence: {"head":"` + sha + `","proven":1,"total":1}`,
		},
		{
			name: "two lines: the later one wins",
			body: `<!-- boss-evidence: {"head":"aaaa","proven":1,"total":2} -->` + "\n" +
				`<!-- boss-evidence: {"head":"` + sha + `","proven":2,"total":2} -->`,
			want: evidence.Summary{Head: sha, Proven: 2, Total: 2},
			ok:   true,
		},
		{
			name: "a malformed later line does not hide a valid earlier one",
			body: `<!-- boss-evidence: {"head":"` + sha + `","proven":2,"total":2} -->` + "\n" +
				`<!-- boss-evidence: {not json} -->`,
			want: evidence.Summary{Head: sha, Proven: 2, Total: 2},
			ok:   true,
		},
		{
			name: "no line",
			body: "<!-- boss-verify-evidence -->\n## Verification evidence\n",
		},
		{
			name: "empty body",
			body: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := evidence.Parse(tt.body)
			if ok != tt.ok {
				t.Fatalf("Parse ok = %v, want %v (got %+v)", ok, tt.ok, got)
			}
			if got != tt.want {
				t.Fatalf("Parse = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMarkerIsTheLinePrefix(t *testing.T) {
	line := evidence.Format(evidence.Summary{Head: sha, Proven: 1, Total: 1})
	if !strings.HasPrefix(line, evidence.Marker) {
		t.Fatalf("Format(...) = %q, want prefix %q", line, evidence.Marker)
	}
}

func TestFormatRoundTrips(t *testing.T) {
	in := evidence.Summary{Head: sha, VideoURL: "https://evidence.example/a<b>-->.mp4", VideoDuration: 2*time.Minute + 40*time.Second, Proven: 3, Total: 4}
	line := evidence.Format(in)
	if strings.Count(line, "-->") != 1 {
		t.Fatalf("Format(...) = %q: the URL must not close the comment early", line)
	}
	got, ok := evidence.Parse(line)
	if !ok || got != in {
		t.Fatalf("Parse(Format(in)) = %+v, %v; want %+v", got, ok, in)
	}
}
