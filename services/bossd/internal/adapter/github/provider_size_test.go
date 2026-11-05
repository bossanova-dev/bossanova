package github

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestGetPRStatusSize_Sessions_AC213501(t *testing.T) {
	for _, sizes := range []string{`,"additions":12,"deletions":3,"changedFiles":2`, ""} {
		var requested string
		p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
			requested = strings.Join(args, " ")
			return `{"state":"OPEN","mergeable":"CONFLICTING"` + sizes + `}`, nil
		}))
		got, err := p.GetPRStatus(t.Context(), "owner/repo", 42)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"additions", "deletions", "changedFiles"} {
			if !strings.Contains(requested, field) {
				t.Fatalf("missing %s: %s", field, requested)
			}
		}
		want := [3]int{}
		if sizes != "" {
			want = [3]int{12, 3, 2}
		}
		if counts := [3]int{got.AddedLineCount, got.DeletedLineCount, got.ChangedFileCount}; counts != want {
			t.Fatalf("size=%v want %v", counts, want)
		}
	}
}
