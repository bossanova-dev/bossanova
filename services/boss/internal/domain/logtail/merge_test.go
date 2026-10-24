package logtail

import "testing"

func TestMergeOrdersByTimestampAndAnchorsRawLines(t *testing.T) {
	bossd := []Record{ParseLine("bossd", `{"time":"2026-08-05T05:00:01Z","message":"first"}`), ParseLine("bossd", "panic: boom")}
	bosso := []Record{ParseLine("bosso", `{"time":"2026-08-05T05:00:02Z","message":"later"}`)}
	got := Merge([][]Record{bossd, bosso})
	want := []string{"first", "panic: boom", "later"}
	if len(got) != len(want) {
		t.Fatalf("want %d, got %d", len(want), len(got))
	}
	for i, text := range want {
		gotText := got[i].Message
		if !got[i].Parsed {
			gotText = got[i].Raw
		}
		if gotText != text {
			t.Errorf("position %d: want %q, got %q", i, text, gotText)
		}
	}
}
