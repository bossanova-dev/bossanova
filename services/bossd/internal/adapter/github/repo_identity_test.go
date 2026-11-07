package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestGetRepositoryIdentity(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		failure    error
		wantErr    bool
	}{
		{"success", `{"id":42,"visibility":"private"}`, nil, false},
		{"missing id", `{"visibility":"private"}`, nil, true},
		{"malformed", `{`, nil, true},
		{"not found", "", errors.New("404"), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				if !strings.Contains(strings.Join(args, " "), "api repos/acme/repo -i") {
					t.Fatalf("args: %v", args)
				}
				return tt.body, tt.failure
			}))
			id, visibility, err := p.GetRepositoryIdentity(context.Background(), "git@github.com:Acme/Repo.git")
			if (err != nil) != tt.wantErr {
				t.Fatalf("error: %v", err)
			}
			if !tt.wantErr && (id != 42 || visibility != "private") {
				t.Fatalf("identity: %d %s", id, visibility)
			}
		})
	}
}
