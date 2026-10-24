package logtail

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// FormatJSON writes exactly one parseable JSON object per record.
func FormatJSON(w io.Writer, rec Record) error {
	var fields map[string]any
	if rec.Parsed {
		fields = make(map[string]any, len(rec.Fields)+2)
		for key, value := range rec.Fields {
			fields[key] = value
		}
		if rec.Plugin != nil {
			fields["_boss_plugin"] = rec.Plugin
			if level, ok := rec.Plugin["level"].(string); ok && level != "" {
				fields["level"] = level
			}
			if message, ok := rec.Plugin["message"].(string); ok {
				fields["message"] = message
			}
		}
	} else {
		fields = map[string]any{"_boss_raw": true, "line": rec.Raw}
		// Agent output is raw but timestamped. Service diagnostics are raw with
		// no timestamp at all, and stay shaped exactly as before.
		if !rec.Time.IsZero() {
			fields["time"] = rec.Time.Format(time.RFC3339Nano)
		}
	}
	fields["_boss_service"] = rec.Service
	encoded, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", encoded)
	return err
}

// FormatPretty writes a readable record with source and time context.
func FormatPretty(w io.Writer, rec Record) error {
	if !rec.Parsed {
		if !rec.Time.IsZero() {
			_, err := fmt.Fprintf(w, "%s %-5s | %s\n", rec.Time.Format(time.TimeOnly), rec.Service, rec.Raw)
			return err
		}
		_, err := fmt.Fprintf(w, "%-5s | %s\n", rec.Service, rec.Raw)
		return err
	}
	stamp := "-"
	if !rec.Time.IsZero() {
		stamp = rec.Time.Format(time.TimeOnly)
	}
	level := rec.Level
	if pluginLevel, ok := pluginOrField(rec, "level"); ok {
		level = pluginLevel
	}
	line := fmt.Sprintf("%s %-5s %-5s %s", stamp, rec.Service, level, rec.Message)
	if plugin, ok := rec.Fields["plugin"].(string); ok && plugin != "" {
		line += fmt.Sprintf("  plugin=%s", plugin)
	}
	if repo, ok := pluginOrField(rec, "repo"); ok {
		line += fmt.Sprintf("  repo=%s", repo)
	}
	_, err := fmt.Fprintln(w, line)
	return err
}

func pluginOrField(rec Record, key string) (string, bool) {
	for _, fields := range []map[string]any{rec.Plugin, rec.Fields} {
		if value, ok := fields[key].(string); ok && value != "" {
			return value, true
		}
	}
	return "", false
}
