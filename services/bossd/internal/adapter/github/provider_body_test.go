package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
)

func TestUpdatePRBodyPreservesBodyAndInvalidatesCachedStatus(t *testing.T) {
	body := "Tracker issue: https://tracker.example/ABC-42\n\nOriginal body\r\n\n## Appended section\n`code` $literal\n"
	remoteBody := "Original body"
	views := 0
	var logs bytes.Buffer
	p := New(zerolog.New(&logs), WithRunGH(func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pr" && args[1] == "edit" {
			want := []string{"pr", "edit", "42", "--repo", "owner/repo", "--body", body}
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("gh args = %q, want %q", args, want)
			}
			remoteBody = args[6]
			return "", nil
		}
		views++
		out, err := json.Marshal(map[string]any{"state": "OPEN", "isDraft": true, "body": remoteBody})
		return string(out), err
	}))
	ctx := vcs.WithCachedReads(context.Background())
	for range 2 {
		if _, err := p.GetPRStatus(ctx, "owner/repo", 42); err != nil {
			t.Fatal(err)
		}
	}
	if views != 1 {
		t.Fatalf("initial reads = %d, want 1 cached read", views)
	}
	if err := p.UpdatePRBody(ctx, "https://github.com/owner/repo.git", 42, body); err != nil {
		t.Fatal(err)
	}
	status, err := p.GetPRStatus(ctx, "owner/repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if views != 2 || status.Body != body {
		t.Fatalf("reads = %d, body = %q; want 2 and updated body", views, status.Body)
	}
	if strings.Contains(logs.String(), "Appended section") || strings.Contains(logs.String(), "tracker.example") {
		t.Fatalf("body leaked to logs: %s", logs.String())
	}
}

func TestUpdatePRBodyWrapsEditError(t *testing.T) {
	cause := errors.New("edit denied")
	p := New(zerolog.Nop(), WithRunGH(func(context.Context, ...string) (string, error) {
		return "", cause
	}))
	err := p.UpdatePRBody(context.Background(), "owner/repo", 42, "body")
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "update PR body") {
		t.Fatalf("error = %v, want wrapped edit failure", err)
	}
}
