package github

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestGetPRStatusBody(t *testing.T) {
	const body = "## Partial\ndo not merge — partial: 1/2"
	p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, ","), ",body,") {
			t.Fatalf("body not requested: %v", args)
		}
		return `{"state":"OPEN","isDraft":true,"body":"## Partial\ndo not merge — partial: 1/2"}`, nil
	}))
	got, err := p.GetPRStatus(t.Context(), "owner/repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != body {
		t.Fatalf("body = %q, want %q", got.Body, body)
	}
}
