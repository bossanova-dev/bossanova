package fixtures

import (
	"math"
	"strings"
	"testing"
)

// These tests cover the int→int32 overflow guards added for gosec G115 on the
// three PR-number conversions in overlay.go (OverlaySession.Build,
// OverlayPR.Build, OverlayTrackerIssue.Build). Each guard rejects a value
// outside the int32 range instead of silently wrapping it.

func TestOverlaySessionBuildPRNumberInt32Bounds(t *testing.T) {
	ptr := func(v int) *int { return &v }
	tests := []struct {
		name    string
		pr      *int
		wantErr bool
	}{
		{"nil pr number", nil, false},
		{"in range", ptr(42), false},
		{"max int32", ptr(math.MaxInt32), false},
		{"over int32", ptr(math.MaxInt32 + 1), true},
		{"negative", ptr(-1), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := OverlaySession{ID: "s1", Title: "t", PRNumber: tc.pr}
			got, err := s.Build()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error for pr=%v, got nil", tc.pr)
				}
				if !strings.Contains(err.Error(), "out of int32 range") {
					t.Fatalf("error = %q, want it to mention int32 range", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.pr == nil {
				if got.PrNumber != nil {
					t.Fatalf("PrNumber = %v, want nil", *got.PrNumber)
				}
				return
			}
			if got.PrNumber == nil || *got.PrNumber != int32(*tc.pr) {
				t.Fatalf("PrNumber = %v, want %d", got.PrNumber, *tc.pr)
			}
		})
	}
}

func TestOverlayPRBuildNumberInt32Bounds(t *testing.T) {
	tests := []struct {
		name    string
		number  int
		wantErr string // substring; "" means success
	}{
		{"required", 0, "number is required"},
		{"in range", 7, ""},
		{"max int32", math.MaxInt32, ""},
		{"over int32", math.MaxInt32 + 1, "out of int32 range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := OverlayPR{Number: tc.number, Title: "t"}
			got, err := p.Build()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Number != int32(tc.number) {
				t.Fatalf("Number = %d, want %d", got.Number, tc.number)
			}
		})
	}
}

func TestOverlayTrackerIssueBuildPRNumberInt32Bounds(t *testing.T) {
	tests := []struct {
		name    string
		pr      int
		wantErr bool
	}{
		{"zero", 0, false},
		{"in range", 99, false},
		{"max int32", math.MaxInt32, false},
		{"over int32", math.MaxInt32 + 1, true},
		{"negative", -5, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			i := OverlayTrackerIssue{ExternalID: "BOS-1", Title: "t", PRNumber: tc.pr}
			got, err := i.Build()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "out of int32 range") {
					t.Fatalf("want int32-range error for pr=%d, got %v", tc.pr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.PrNumber != int32(tc.pr) {
				t.Fatalf("PrNumber = %d, want %d", got.PrNumber, tc.pr)
			}
		})
	}
}
