package logtail

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Agent logs are not service logs. A headless run writes NDJSON entries
// (`{"ts":"…","text":"…"}`) covering stdout and stderr together, with
// `[runner]` diagnostics interleaved in the same shape; an interactive chat
// holds a raw tmux `pipe-pane` capture with no timestamps and full ANSI
// escapes. The two are indistinguishable by filename, so the format is
// detected per file from its first non-empty line.
type agentFormat int

const (
	agentFormatUnknown agentFormat = iota
	agentFormatNDJSON
	agentFormatRaw
)

// AgentParser turns one agent log file's lines into records. It is stateful:
// a line that carries no usable timestamp of its own inherits the last one
// seen on this file, so untimestamped raw output is anchored next to the
// output it followed instead of sorting to the zero time (the Unix epoch),
// which Merge would otherwise float ahead of every other source.
//
// A parser belongs to exactly one source and is not safe for concurrent use.
// Give the backlog pass and the follower separate instances.
type AgentParser struct {
	service string
	format  agentFormat
	last    time.Time
}

// NewAgentParser returns a parser for one agent log. seed anchors output that
// appears before any timestamp is known — see AgentSeedTime.
func NewAgentParser(service string, seed time.Time) *AgentParser {
	return &AgentParser{service: service, last: seed}
}

// AgentSeedTime returns the anchor for a log's untimestamped leading output:
// the file's modification time, or the current time when the file does not
// exist yet (a followed agent session may not have written anything so far).
// Either way the seed is non-zero, which is what keeps a raw capture off the
// epoch. It is captured once, when the source is resolved.
func AgentSeedTime(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Now()
}

// Parse converts one physical agent-log line into a record.
//
// Agent records are always left unparsed (Record.Parsed false): they are agent
// output rather than structured service records, so they carry no level, repo
// or plugin. That also keeps them exempt from --level/--repo/--plugin
// filtering, matching the existing rule that non-JSON diagnostics stay visible
// — marking them parsed would let `--level error` hide every line of an agent
// run.
func (p *AgentParser) Parse(line string) Record {
	line = strings.TrimSuffix(line, "\r")
	if p.format == agentFormatUnknown && strings.TrimSpace(line) != "" {
		if _, _, ok := decodeAgentNDJSON(line); ok {
			p.format = agentFormatNDJSON
		} else {
			p.format = agentFormatRaw
		}
	}
	if p.format == agentFormatNDJSON {
		if text, stamp, ok := decodeAgentNDJSON(line); ok {
			if !stamp.IsZero() {
				p.last = stamp
			}
			return p.record(text)
		}
		// A malformed entry in an NDJSON log degrades to raw rather than
		// vanishing, on the same principle as ParseLine.
	}
	return p.record(line)
}

func (p *AgentParser) record(text string) Record {
	return Record{
		Service: p.service,
		Time:    p.last,
		Raw:     strings.TrimSuffix(ansi.Strip(text), "\r"),
	}
}

// decodeAgentNDJSON reports whether line is a headless-runner entry, returning
// its text and timestamp. The runner's own encode-failure fallback writes an
// empty ts, so a missing or unparseable stamp is not a rejection — it just
// leaves the timestamp to be carried forward.
func decodeAgentNDJSON(line string) (string, time.Time, bool) {
	if !strings.HasPrefix(strings.TrimSpace(line), "{") {
		return "", time.Time{}, false
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return "", time.Time{}, false
	}
	text, hasText := fields["text"].(string)
	rawStamp, hasStamp := fields["ts"].(string)
	if !hasText || !hasStamp {
		return "", time.Time{}, false
	}
	stamp, err := time.Parse(time.RFC3339Nano, rawStamp)
	if err != nil {
		stamp = time.Time{}
	}
	return text, stamp, true
}
