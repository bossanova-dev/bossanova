package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/apiversion"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestSupportedMaxFromError(t *testing.T) {
	cases := []struct {
		name    string
		errText string
		want    apiversion.Version
		wantOK  bool
	}{
		{
			name:    "canonical interceptor error",
			errText: `invalid_argument: unsupported API version "not-a-date": must be YYYY-MM-DD; supported range: 2026-06-29 to 2026-07-04`,
			want:    "2026-07-04",
			wantOK:  true,
		},
		{
			name:    "trailing text after max is trimmed",
			errText: "supported range: 2026-06-29 to 2026-07-04 (deploy required)",
			want:    "2026-07-04",
			wantOK:  true,
		},
		{
			name:    "single-version range",
			errText: "supported range: 2026-06-29 to 2026-06-29",
			want:    "2026-06-29",
			wantOK:  true,
		},
		{
			name:    "no range marker",
			errText: "unauthenticated: missing token",
			wantOK:  false,
		},
		{
			name:    "malformed max not a date",
			errText: "supported range: 2026-06-29 to notadate",
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := supportedMaxFromError(tc.errText)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (err=%q)", ok, tc.wantOK, tc.errText)
			}
			if ok && got != tc.want {
				t.Errorf("max = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeReporter returns a canned error (or nil) for ReportBug so checkCurrentSupported
// is exercised without a live server.
type fakeReporter struct {
	err error
}

func (f fakeReporter) ReportBug(context.Context, *connect.Request[pb.ReportBugRequest]) (*connect.Response[pb.ReportBugResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&pb.ReportBugResponse{}), nil
}

// rangeErr builds the exact InvalidArgument error the version interceptor returns
// for the malformed probe, so the test drives the real error surface.
func rangeErr(min, max apiversion.Version) error {
	return connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("unsupported API version %q: must be YYYY-MM-DD; supported range: %s to %s", probeVersion, min, max))
}

func TestCheckCurrentSupported(t *testing.T) {
	current := apiversion.Version("2026-07-04")

	t.Run("server up to date passes", func(t *testing.T) {
		err := checkCurrentSupported(context.Background(),
			fakeReporter{err: rangeErr("2026-06-29", "2026-07-04")}, current)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("server ahead of current passes", func(t *testing.T) {
		err := checkCurrentSupported(context.Background(),
			fakeReporter{err: rangeErr("2026-06-29", "2026-08-01")}, current)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("server trailing current fails and names the skew", func(t *testing.T) {
		// The BOS-241 outage: production stuck at Baseline while clients request 2026-07-04.
		err := checkCurrentSupported(context.Background(),
			fakeReporter{err: rangeErr("2026-06-29", "2026-06-29")}, current)
		if err == nil {
			t.Fatal("expected failure when server supported-max trails current")
		}
		if !strings.Contains(err.Error(), "trails the built client version") {
			t.Errorf("error should explain the skew, got: %v", err)
		}
	})

	t.Run("unparseable server response fails closed", func(t *testing.T) {
		err := checkCurrentSupported(context.Background(),
			fakeReporter{err: connect.NewError(connect.CodeUnavailable, fmt.Errorf("connection refused"))}, current)
		if err == nil {
			t.Fatal("expected failure when the supported range cannot be parsed")
		}
	})

	t.Run("accepted probe version fails closed", func(t *testing.T) {
		// No error means no version interceptor rejected the malformed version.
		err := checkCurrentSupported(context.Background(), fakeReporter{err: nil}, current)
		if err == nil {
			t.Fatal("expected failure when the malformed probe is accepted")
		}
	})
}
