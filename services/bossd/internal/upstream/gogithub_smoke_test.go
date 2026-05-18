package upstream

import (
	"testing"

	"github.com/google/go-github/v76/github"
)

func TestGoGithubImportable(t *testing.T) {
	if _, err := github.ParseWebHook("ping", []byte(`{"zen":"ok"}`)); err != nil {
		t.Fatalf("ParseWebHook(ping) returned error: %v", err)
	}
}
