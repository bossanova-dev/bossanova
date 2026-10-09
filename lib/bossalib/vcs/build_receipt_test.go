package vcs

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type legacyCheckProvider struct {
	Provider
	checks []CheckResult
	err    error
	calls  int
}

func (p *legacyCheckProvider) GetCheckResults(context.Context, string, int) ([]CheckResult, error) {
	p.calls++
	return p.checks, p.err
}

type receiptCheckProvider struct {
	*legacyCheckProvider
	set CheckSet
}

func (p *receiptCheckProvider) GetCheckSet(context.Context, string, int) (CheckSet, error) {
	return p.set, p.err
}

func TestReadCheckSet_BuildReceipt(t *testing.T) {
	checks := []CheckResult{{Name: "build"}}
	legacy := &legacyCheckProvider{checks: checks}
	got, err := ReadCheckSet(context.Background(), legacy, "owner/repo", 42)
	if err != nil || got.HasBuildReceipt || !reflect.DeepEqual(got.Checks, checks) || legacy.calls != 1 {
		t.Fatalf("fallback: set=%+v err=%v calls=%d", got, err, legacy.calls)
	}
	capable := &receiptCheckProvider{legacyCheckProvider: legacy, set: CheckSet{Checks: checks, HasBuildReceipt: true}}
	got, err = ReadCheckSet(context.Background(), capable, "owner/repo", 42)
	if err != nil || !got.HasBuildReceipt || !reflect.DeepEqual(got.Checks, checks) || legacy.calls != 1 {
		t.Fatalf("capability: set=%+v err=%v calls=%d", got, err, legacy.calls)
	}
	sentinel := errors.New("remote failed")
	legacy.err = sentinel
	for _, p := range []Provider{legacy, capable} {
		_, err = ReadCheckSet(context.Background(), p, "owner/repo", 42)
		if !errors.Is(err, sentinel) {
			t.Fatalf("error=%v want %v", err, sentinel)
		}
	}
}
