package session

import (
	"testing"

	"github.com/recurser/bossalib/models"
)

// The review app the daemon serves is passed to every managed chat, so verify's
// pages link stacked PRs to it; a daemon with none passes nothing.
func TestManagedSessionEnv_ReviewAppURL(t *testing.T) {
	sess := &models.Session{ID: "s1"}
	t.Setenv(ReviewAppURLEnv, "")
	if _, ok := ManagedSessionEnv(sess, "agent-1", "claude", nil, nil)[ReviewAppURLEnv]; ok {
		t.Fatalf("%s set without a review app", ReviewAppURLEnv)
	}
	t.Setenv(ReviewAppURLEnv, "https://review.example")
	if got := ManagedSessionEnv(sess, "agent-1", "claude", nil, nil)[ReviewAppURLEnv]; got != "https://review.example" {
		t.Fatalf("%s = %q, want the daemon's review app", ReviewAppURLEnv, got)
	}
}
