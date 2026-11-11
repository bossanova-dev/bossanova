package main

import (
	"go/token"
	"slices"
	"testing"
)

func TestScanSource(t *testing.T) {
	tests := []struct {
		name, source string
		want         []bool
	}{
		{"aliased imports and lexical bubble", `package p
import tm "time"
import st "testing/synctest"
func f() { tm.Sleep(1); st.Test(nil, func(t any) { tm.Sleep(1); func(){tm.Sleep(1)}() }); tm.Sleep(1) }`, []bool{false, true, true, false}},
		{"parenthesized calls and callback", `package p
import "time"
import "testing/synctest"
func f() { (time.Sleep)(1); (synctest.Test)(nil, (func(t any){(time.Sleep)(1)})) }`, []bool{false, true}},
		{"local variable shadows import", `package p
import "time"
func f() { time.Sleep(1); { time := other; time.Sleep(1) }; time.Sleep(1) }`, []bool{false, false}},
		{"initializer uses outer import", `package p
import "time"
func f() { time := time.Sleep(1); time.Sleep(1) }`, []bool{false}},
		{"parameters and receivers", `package p
import "time"
func f(time any) { time.Sleep(1) }
func (time T) m() { time.Sleep(1) }
func g() { func(time any) { time.Sleep(1) }(nil); time.Sleep(1) }`, []bool{false}},
		{"range and control scope", `package p
import "time"
func f() { for time := range xs { time.Sleep(1) }; if time := other; true { time.Sleep(1) } else { time.Sleep(1) }; time.Sleep(1) }`, []bool{false}},
		{"var const and type declarations", `package p
import "time"
func f() { { var time = time.Sleep(1); time.Sleep(1) }; { const time = 1; time.Sleep(1) }; { type time int; time.Sleep(1) }; time.Sleep(1) }`, []bool{false, false}},
		{"synctest shadow does not create bubble", `package p
import "time"
import "testing/synctest"
func f() { synctest := other; synctest.Test(nil, func(t any){time.Sleep(1)}) }`, []bool{false}},
		{"package initializer callback shadows time", `package p
import "time"
var f = func(time any) { time.Sleep(1) }`, nil},
		{"callback parameter shadows time", `package p
import "time"
import "testing/synctest"
func f() { synctest.Test(nil, func(time any){time.Sleep(1)}) }`, nil},
		{"only last literal is bubble", `package p
import "time"
import "testing/synctest"
func f() { synctest.Test(func(){time.Sleep(1)}, callback); time.Sleep(1) }`, []bool{false, false}},
		{"comments strings and selectors ignored", `package p
import "time"
// time.Sleep(1)
var s = "time.Sleep(1)"
func f() { object.time.Sleep(1) }`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanSource(token.NewFileSet(), "a_test.go", tt.source)
			if len(got.Errors) > 0 {
				t.Fatal(got.Errors)
			}
			if len(got.Sleeps) != len(tt.want) {
				t.Fatalf("sleeps = %+v; want %v", got.Sleeps, tt.want)
			}
			for i, want := range tt.want {
				if got.Sleeps[i].InSynctest != want {
					t.Fatalf("sleep %d = %+v; want bubble %v", i, got.Sleeps[i], want)
				}
			}
		})
	}
}

func TestFakeDeclarationsAndParseErrors(t *testing.T) {
	got := scanSource(token.NewFileSet(), "pkg/a_test.go", "package p\ntype (fakeClock struct{}; stubStore interface{}; mockCall int; FakeIgnored int)\nfunc f(){type fakeLocal int}")
	if len(got.Fakes) != 4 {
		t.Fatalf("fakes = %+v", got.Fakes)
	}
	if got.Fakes[0].Name != "fakeClock" || got.Fakes[0].Dir != "pkg" || got.Fakes[0].Line != 2 {
		t.Fatalf("location = %+v", got.Fakes[0])
	}
	bad := scanSource(token.NewFileSet(), "broken_test.go", "package p\nfunc")
	if len(bad.Errors) != 1 {
		t.Fatalf("parse errors = %+v", bad.Errors)
	}
}

func TestScanSourceLiteralTimers(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"all constructors", `tm.After(tm.Second); tm.NewTimer(2 * tm.Minute); tm.AfterFunc(tm.Millisecond, func() {}); tm.Tick(tm.Hour); tm.NewTicker(tm.Microsecond)`, 5},
		{"literal expressions", `(tm.After)((-2 + 3) * tm.Second); tm.After(tm.Duration(1.5 * 1000)); tm.After(0); tm.After(1 << 3); tm.After(^1)`, 5},
		{"named and computed budgets", `tm.After(timeout); tm.After(2 * timeout); tm.After(other.Delay); tm.After(next()); tm.After(tm.Duration(timeout))`, 0},
		{"non-duration literals and selectors", `tm.After("short"); tm.After('s'); tm.After(tm.Unknown); tm.After(other.Second)`, 0},
		{"shadowed import", `{ tm := other; tm.After(tm.Second) }; tm.After(tm.Second)`, 1},
		{"shadowed unit import", `tm.After(func() int { tm := other; return tm.Second }())`, 0},
		{"unrelated calls", `tm.Since(1); tm.Until(1); other.After(tm.Second); tm.After()`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanSource(token.NewFileSet(), "timers_test.go", "package p\nimport tm \"time\"\nfunc f(){"+tt.body+"}")
			if len(got.Errors) != 0 || len(got.Sleeps) != tt.want {
				t.Fatalf("scan = %+v; want %d timer calls", got, tt.want)
			}
		})
	}
}

func TestScanSourceTimerNamesAndBubbles(t *testing.T) {
	got := scanSource(token.NewFileSet(), "timers_test.go", `package p
import tm "time"
import st "testing/synctest"
func f() {
tm.Sleep(timeout)
tm.After(tm.Second)
tm.NewTimer(tm.Second)
tm.AfterFunc(tm.Second, func(){})
tm.Tick(tm.Second)
tm.NewTicker(tm.Second)
st.Test(nil, func(t any) { (tm.After)(tm.Second); func(){tm.NewTimer(tm.Second)}() })
}`)
	var calls []string
	var bubbles []bool
	for _, found := range got.Sleeps {
		calls = append(calls, found.Call)
		bubbles = append(bubbles, found.InSynctest)
		if found.File != "timers_test.go" || found.Line < 5 {
			t.Fatalf("location = %+v", found)
		}
	}
	if len(got.Errors) != 0 || !slices.Equal(calls, []string{"Sleep", "After", "NewTimer", "AfterFunc", "Tick", "NewTicker", "After", "NewTimer"}) ||
		!slices.Equal(bubbles, []bool{false, false, false, false, false, false, true, true}) {
		t.Fatalf("calls = %v, bubbles = %v, errors = %v", calls, bubbles, got.Errors)
	}
}
