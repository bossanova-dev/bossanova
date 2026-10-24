package logtail

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFormatJSONWrapsRawLinesSoTheStreamStaysParseable(t *testing.T) {
	var sb strings.Builder
	if err := FormatJSON(&sb, ParseLine("bossd", "panic: boom")); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(sb.String())), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["_boss_raw"] != true || obj["line"] != "panic: boom" {
		t.Errorf("unexpected object: %v", obj)
	}
}

func TestFormatJSONKeepsTopLevelKeysQueryable(t *testing.T) {
	var sb strings.Builder
	if err := FormatJSON(&sb, ParseLine("bossd", `{"level":"warn","message":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(sb.String())), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["level"] != "warn" || obj["_boss_service"] != "bossd" {
		t.Errorf("unexpected object: %v", obj)
	}
}

func TestFormatJSONPromotesUnwrappedPluginSeverity(t *testing.T) {
	var sb strings.Builder
	if err := FormatJSON(&sb, Unwrap(ParseLine("bossd", pluginLine))); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(sb.String())), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["level"] != "info" || obj["message"] != "checks failed, creating fix session" {
		t.Errorf("want promoted plugin severity and message, got %v", obj)
	}
	if _, ok := obj["_boss_plugin"]; !ok {
		t.Errorf("want preserved plugin payload, got %v", obj)
	}
}

func TestFormatPrettyPrintsRawLineVerbatim(t *testing.T) {
	var sb strings.Builder
	if err := FormatPretty(&sb, ParseLine("bossd", "panic: boom")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "panic: boom") {
		t.Errorf("missing raw text: %q", sb.String())
	}
}

func TestFormatPrettyPrefersUnwrappedPluginLevel(t *testing.T) {
	var sb strings.Builder
	if err := FormatPretty(&sb, Unwrap(ParseLine("bossd", pluginLine))); err != nil {
		t.Fatal(err)
	}
	if got := sb.String(); !strings.Contains(got, "bossd info") || strings.Contains(got, "bossd debug") {
		t.Errorf("want unwrapped plugin level in %q", got)
	}
}

func TestFormatsCarryTheTimestampOfATimestampedRawRecord(t *testing.T) {
	at := time.Date(2026, 8, 5, 5, 0, 1, 0, time.UTC)
	rec := Record{Service: "agent-1", Time: at, Raw: "agent output"}

	var pretty strings.Builder
	if err := FormatPretty(&pretty, rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pretty.String(), at.Format(time.TimeOnly)) {
		t.Errorf("pretty dropped the timestamp: %q", pretty.String())
	}

	var raw strings.Builder
	if err := FormatJSON(&raw, rec); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw.String())), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["time"] != at.Format(time.RFC3339Nano) || obj["_boss_raw"] != true {
		t.Errorf("unexpected object: %v", obj)
	}
}

func TestUntimestampedRawRecordsRenderExactlyAsBefore(t *testing.T) {
	var pretty strings.Builder
	if err := FormatPretty(&pretty, ParseLine("bossd", "panic: boom")); err != nil {
		t.Fatal(err)
	}
	if got, want := pretty.String(), "bossd | panic: boom\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	var raw strings.Builder
	if err := FormatJSON(&raw, ParseLine("bossd", "panic: boom")); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw.String())), &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["time"]; ok {
		t.Errorf("added a time key to an untimestamped record: %v", obj)
	}
}
