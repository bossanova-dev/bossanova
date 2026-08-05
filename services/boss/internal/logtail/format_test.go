package logtail

import (
	"encoding/json"
	"strings"
	"testing"
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
