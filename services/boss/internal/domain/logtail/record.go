// Package logtail parses, filters, merges and renders service log records for
// the boss tail command.
package logtail

import (
	"encoding/json"
	"time"
)

// Record represents one physical source-log line. Invalid JSON remains a raw
// record so panic output and stderr bleed are never hidden.
type Record struct {
	Service string
	Time    time.Time
	Level   string
	Message string
	Fields  map[string]any
	Plugin  map[string]any
	Raw     string
	Parsed  bool
}

// ParseLine decodes a source-log line without ever dropping invalid JSON.
func ParseLine(service, line string) Record {
	rec := Record{Service: service, Raw: line}
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return rec
	}
	rec.Parsed, rec.Fields = true, fields
	rec.Level, _ = fields["level"].(string)
	rec.Message, _ = fields["message"].(string)
	if stamp, ok := fields["time"].(string); ok {
		rec.Time, _ = time.Parse(time.RFC3339Nano, stamp)
	}
	return rec
}

// Unwrap exposes a plugin-host record's escaped inner NDJSON fields.
func Unwrap(rec Record) Record {
	if !rec.Parsed || rec.Message == "" {
		return rec
	}
	if component, _ := rec.Fields["component"].(string); component != "plugin-host" {
		return rec
	}
	var plugin map[string]any
	if err := json.Unmarshal([]byte(rec.Message), &plugin); err != nil {
		return rec
	}
	rec.Plugin = plugin
	if message, ok := plugin["message"].(string); ok {
		rec.Message = message
	}
	return rec
}
