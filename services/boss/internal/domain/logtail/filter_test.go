package logtail

import "testing"

func TestMatchFindsRepoInsideUnwrappedPluginPayload(t *testing.T) {
	rec := Unwrap(ParseLine("bossd", pluginLine))
	if !(Filter{Repo: "acme/core"}).Match(rec) || (Filter{Repo: "other/repo"}).Match(rec) {
		t.Error("inner repo matching failed")
	}
}

func TestMatchExcludesMissingFieldButAlwaysPassesRawRecords(t *testing.T) {
	if (Filter{Repo: "acme/core"}).Match(ParseLine("bossd", `{"level":"info"}`)) {
		t.Error("record missing repo matched")
	}
	if !(Filter{Repo: "acme/core", Level: "error"}).Match(ParseLine("bossd", "panic: boom")) {
		t.Error("raw record did not pass")
	}
}

func TestMatchIsCaseInsensitiveOnLevel(t *testing.T) {
	if !(Filter{Level: "warn"}).Match(ParseLine("bossd", `{"level":"WARN"}`)) {
		t.Error("level matching is case-sensitive")
	}
}

func TestMatchFindsLevelInsideUnwrappedPluginPayload(t *testing.T) {
	rec := Unwrap(ParseLine("bossd", pluginLine))
	if !(Filter{Level: "info"}).Match(rec) {
		t.Error("inner plugin level did not match")
	}
}
