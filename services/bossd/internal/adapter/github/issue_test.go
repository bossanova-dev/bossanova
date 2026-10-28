package github

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/rs/zerolog"
)

func TestOpenIssueURL(t *testing.T) {
	ghErr := errors.New("gh: HTTP 403: forbidden")
	for _, tc := range []struct {
		name    string
		body    string
		err     error
		wantURL string
		wantErr bool
	}{
		{name: "open", body: `{"html_url":"https://github.com/o/r/issues/2","state":"open"}`, wantURL: "https://github.com/o/r/issues/2"},
		{name: "closed", body: `{"html_url":"https://github.com/o/r/issues/2","state":"closed"}`},
		{name: "pull request", body: `{"html_url":"https://github.com/o/r/pull/2","state":"open","pull_request":{}}`},
		{name: "missing URL", body: `{"state":"open"}`},
		{name: "not found", err: errors.New("gh: Not Found (HTTP 404)")},
		{name: "gh error", err: ghErr, wantErr: true},
		{name: "invalid JSON", body: "not json", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				gotArgs = args
				return tc.body, tc.err
			}))
			url, found, err := p.OpenIssueURL(context.Background(), "github.com", "o", "r", 2)
			if (err != nil) != tc.wantErr {
				t.Fatalf("OpenIssueURL error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.err == ghErr && !errors.Is(err, ghErr) {
				t.Fatalf("error = %v, want wrapped gh error", err)
			}
			if url != tc.wantURL || found != (tc.wantURL != "") {
				t.Errorf("OpenIssueURL = (%q, %v), want (%q, %v)", url, found, tc.wantURL, tc.wantURL != "")
			}
			wantArgs := []string{"api", "repos/o/r/issues/2", "--hostname", "github.com"}
			if !reflect.DeepEqual(gotArgs, wantArgs) {
				t.Errorf("gh args = %v, want %v", gotArgs, wantArgs)
			}
		})
	}
}
