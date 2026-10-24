package logtail

import "testing"

const pluginLine = `{"level":"debug","service":"bossd","component":"plugin-host","plugin":"dependabot","time":"2026-08-05T05:44:46+09:00","message":"{\"level\":\"info\",\"repo\":\"acme/core\",\"pr\":4532,\"message\":\"checks failed, creating fix session\"}"}`

func TestParseLineExtractsTopLevelFields(t *testing.T) {
	rec := ParseLine("bossd", `{"level":"warn","time":"2026-08-05T05:44:46+09:00","message":"hi"}`)
	if !rec.Parsed || rec.Level != "warn" || rec.Message != "hi" || rec.Time.IsZero() {
		t.Errorf("unexpected record: %+v", rec)
	}
}

func TestParseLineKeepsNonJSONVerbatim(t *testing.T) {
	const panicLine = "panic: runtime error: invalid memory address"
	rec := ParseLine("bossd", panicLine)
	if rec.Parsed || rec.Raw != panicLine {
		t.Errorf("unexpected raw record: %+v", rec)
	}
}

func TestUnwrapPromotesPluginPayload(t *testing.T) {
	rec := Unwrap(ParseLine("bossd", pluginLine))
	if rec.Plugin == nil {
		t.Fatal("want decoded plugin payload")
	}
	if got, _ := rec.Plugin["repo"].(string); got != "acme/core" {
		t.Errorf("want inner repo, got %q", got)
	}
	if rec.Message != "checks failed, creating fix session" {
		t.Errorf("got %q", rec.Message)
	}
}

func TestUnwrapKeepsOuterRecordWhenInnerIsMalformed(t *testing.T) {
	rec := Unwrap(ParseLine("bossd", `{"component":"plugin-host","message":"{not json"}`))
	if rec.Plugin != nil || rec.Message != "{not json" {
		t.Errorf("unexpected record: %+v", rec)
	}
}
