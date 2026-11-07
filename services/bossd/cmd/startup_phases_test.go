package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
)

func startupEvents(t *testing.T, buf *bytes.Buffer) []map[string]json.RawMessage {
	t.Helper()
	var events []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestStartupPhaseTimerSummary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		timer := startupPhaseTimer{logger: zerolog.New(&buf), started: time.Now(), slowAfter: startupPhaseSlowAfter}
		for i, name := range []string{"stores", "serving"} {
			if err := timer.phase(name, func() error { time.Sleep(time.Duration(i+1) * time.Millisecond); return nil }); err != nil {
				t.Fatal(err)
			}
		}
		timer.ready()
		events := startupEvents(t, &buf)
		if len(events) != 3 {
			t.Fatalf("got %d events, want 3: %s", len(events), &buf)
		}
		for i, name := range []string{"stores", "serving"} {
			if string(events[i]["message"]) != `"startup phase done"` || string(events[i]["phase"]) != `"`+name+`"` || string(events[i]["level"]) != `"debug"` || events[i]["elapsed"] == nil {
				t.Fatalf("bad phase event: %s", &buf)
			}
		}
		summary := events[2]
		if string(summary["message"]) != `"daemon ready to serve"` || string(summary["level"]) != `"info"` || summary["total"] == nil || string(summary["slowest_phase"]) != `"serving"` || summary["slowest_elapsed"] == nil {
			t.Fatalf("bad summary: %s", &buf)
		}
		phases := string(summary["phases"])
		if !strings.Contains(phases, `"stores":`) || !strings.Contains(phases, `"serving":`) || strings.Index(phases, `"stores"`) > strings.Index(phases, `"serving"`) {
			t.Fatalf("bad phase order: %s", phases)
		}
	})
}

func TestStartupPhaseTimerError(t *testing.T) {
	var buf bytes.Buffer
	timer := startupPhaseTimer{logger: zerolog.New(&buf), started: time.Now(), slowAfter: startupPhaseSlowAfter}
	want := errors.New("startup failed")
	if err := timer.phase("stores", func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	events := startupEvents(t, &buf)
	if len(events) != 1 || string(events[0]["error"]) != `"startup failed"` {
		t.Fatalf("missing error event: %s", &buf)
	}
}

func TestStartupPhaseTimerWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		timer := startupPhaseTimer{logger: zerolog.New(&buf), started: time.Now(), slowAfter: startupPhaseSlowAfter}
		if err := timer.phase("slow", func() error {
			time.Sleep(2 * startupPhaseSlowAfter)
			synctest.Wait()
			events := startupEvents(t, &buf)
			if len(events) != 1 || string(events[0]["message"]) != `"startup phase still running"` || string(events[0]["phase"]) != `"slow"` || string(events[0]["level"]) != `"warn"` || events[0]["elapsed"] == nil {
				t.Fatalf("missing warning before return: %s", &buf)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := timer.phase("fast", func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * startupPhaseSlowAfter)
		synctest.Wait()
		if events := startupEvents(t, &buf); len(events) != 3 {
			t.Fatalf("want one warning and two completions: %s", &buf)
		}
	})
}
