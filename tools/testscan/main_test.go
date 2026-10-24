package main

import (
	"go/token"
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
