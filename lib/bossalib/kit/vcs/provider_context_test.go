package vcs

import (
	"context"
	"testing"
)

func TestWithoutBackgroundReads(t *testing.T) {
	type markerKey struct{}
	ctx := context.WithValue(context.Background(), markerKey{}, "kept")
	ctx = WithReserveQuotaReads(WithCachedReads(ctx))
	foreground := WithoutBackgroundReads(ctx)
	if CachedReadsAllowed(foreground) || ReserveQuotaReads(foreground) {
		t.Fatal("foreground context retains background read marks")
	}
	if foreground.Value(markerKey{}) != "kept" {
		t.Fatal("foreground context lost parent values")
	}
	if !CachedReadsAllowed(ctx) || !ReserveQuotaReads(ctx) {
		t.Fatal("parent context was changed")
	}
}
