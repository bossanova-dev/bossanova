package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTUI is a programmable tui for protocol tests.
type fakeTUI struct {
	mu sync.Mutex

	// screens, when non-empty, are returned in sequence by Screen(); the last
	// value repeats once exhausted. When empty, screen is returned.
	screens []string
	idx     int
	screen  string

	waitErr error // returned by WaitForText
}

func (f *fakeTUI) Screen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.screens) > 0 {
		if f.idx >= len(f.screens) {
			return f.screens[len(f.screens)-1]
		}
		s := f.screens[f.idx]
		f.idx++
		return s
	}
	return f.screen
}

func (f *fakeTUI) SendString(string) error  { return nil }
func (f *fakeTUI) PasteString(string) error { return nil }
func (f *fakeTUI) WaitForText(time.Duration, string) error {
	return f.waitErr
}

// fastSettle settles quickly for tests.
func fastSettle() settleConfig {
	return settleConfig{
		poll:      time.Millisecond,
		stableFor: 5 * time.Millisecond,
		hardCap:   500 * time.Millisecond,
		cols:      80,
		rows:      24,
	}
}

func decodeResponses(t *testing.T, out string) []map[string]any {
	t.Helper()
	var resps []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("response line not JSON: %q: %v", line, err)
		}
		resps = append(resps, m)
	}
	return resps
}

// TestServeMalformedThenValid asserts a malformed line yields an error response
// and the loop keeps processing a following valid observe.
func TestServeMalformedThenValid(t *testing.T) {
	f := &fakeTUI{screen: "ready"}
	in := strings.NewReader("{not json\n" + `{"id":2,"op":"observe"}` + "\n")
	var out strings.Builder
	if err := serve(f, in, &out, fastSettle()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resps := decodeResponses(t, out.String())
	if len(resps) != 2 {
		t.Fatalf("want 2 responses, got %d: %s", len(resps), out.String())
	}
	if ok, _ := resps[0]["ok"].(bool); ok {
		t.Fatalf("first response should be error, got %v", resps[0])
	}
	if resps[0]["error"] == nil {
		t.Fatalf("first response should carry an error message, got %v", resps[0])
	}
	if resps[1]["screen"] != "ready" {
		t.Fatalf("observe screen = %v, want ready", resps[1]["screen"])
	}
	if got := resps[1]["id"]; got != float64(2) {
		t.Fatalf("observe id = %v, want 2", got)
	}
}

// TestServeWaitTimeout asserts wait against an anchor the fake never shows
// returns ok:false / error:timeout within the timeout.
func TestServeWaitTimeout(t *testing.T) {
	f := &fakeTUI{screen: "loading", waitErr: errors.New("timeout after 30ms")}
	in := strings.NewReader(`{"id":7,"op":"wait","text":"never","timeoutMs":30}` + "\n")
	var out strings.Builder
	start := time.Now()
	if err := serve(f, in, &out, fastSettle()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait took too long: %v", elapsed)
	}
	resps := decodeResponses(t, out.String())
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if ok, _ := resps[0]["ok"].(bool); ok {
		t.Fatalf("wait should fail, got %v", resps[0])
	}
	if resps[0]["error"] != "timeout" {
		t.Fatalf("wait error = %v, want timeout", resps[0]["error"])
	}
}

// TestSettleReturnsStableScreen asserts settle returns only after Screen()
// stops changing.
func TestSettleReturnsStableScreen(t *testing.T) {
	f := &fakeTUI{screens: []string{"loading", "loading", "ready"}}
	got := settle(f, fastSettle())
	if got != "ready" {
		t.Fatalf("settle = %q, want ready", got)
	}
}

// TestServeQuit asserts quit returns the quit sentinel and acks.
func TestServeQuit(t *testing.T) {
	f := &fakeTUI{screen: "ready"}
	in := strings.NewReader(`{"id":9,"op":"quit"}` + "\n")
	var out strings.Builder
	err := serve(f, in, &out, fastSettle())
	if err != errQuit {
		t.Fatalf("serve returned %v, want errQuit", err)
	}
	resps := decodeResponses(t, out.String())
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if ok, _ := resps[0]["ok"].(bool); !ok {
		t.Fatalf("quit ack should be ok:true, got %v", resps[0])
	}
	if got := resps[0]["id"]; got != float64(9) {
		t.Fatalf("quit id = %v, want 9", got)
	}
}

// TestServeUnknownOp asserts an unknown op yields an error response and keeps
// serving.
func TestServeUnknownOp(t *testing.T) {
	f := &fakeTUI{screen: "ready"}
	in := strings.NewReader(`{"id":3,"op":"frobnicate"}` + "\n")
	var out strings.Builder
	if err := serve(f, in, &out, fastSettle()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resps := decodeResponses(t, out.String())
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if ok, _ := resps[0]["ok"].(bool); ok {
		t.Fatalf("unknown op should fail, got %v", resps[0])
	}
	if msg, _ := resps[0]["error"].(string); !strings.Contains(msg, "unknown op") {
		t.Fatalf("unknown op error = %v", resps[0]["error"])
	}
}
