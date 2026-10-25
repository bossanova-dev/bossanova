package tuidriver

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCastWriterRecordsTerminalOutput(t *testing.T) {
	var output bytes.Buffer
	now := time.Unix(100, 0)
	writer, err := NewCastWriter(&output, 120, 30, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(1500 * time.Millisecond)
	if n, err := writer.Write([]byte("\x1b[2Jhome")); n != 8 || err != nil {
		t.Fatalf("write = %d, %v", n, err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var header struct{ Version, Width, Height int }
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header.Version != 2 || header.Width != 120 || header.Height != 30 {
		t.Fatalf("header = %+v", header)
	}
	var event []any
	if err := json.Unmarshal([]byte(lines[1]), &event); err != nil {
		t.Fatal(err)
	}
	if event[0] != 1.5 || event[1] != "o" || event[2] != "\x1b[2Jhome" {
		t.Fatalf("event = %#v", event)
	}
}
