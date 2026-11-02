package upstream

import (
	"context"
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

type orderedRepoInvalidator struct{ orderedInvalidator }

func (i orderedRepoInvalidator) InvalidateRepo(repoPath string) {
	i.log.add("invalidate-repo " + repoPath)
}

type orderedBaseMoves struct {
	log *orderLog
	prs []int
}

func (b orderedBaseMoves) NoteBaseMoved(_ context.Context, _, branch string) []int {
	b.log.add("base-moved " + branch)
	return b.prs
}

// TestDispatch_PushInvalidatesRepoBeforeBaseMoves pins the push path's
// composition: the repo's cached reads go stale before the base-move notifier
// marks any PR due, whether or not it maps the push to a PR.
func TestDispatch_PushInvalidatesRepoBeforeBaseMoves(t *testing.T) {
	const origin = "https://github.com/owner/repo"
	for _, tc := range []struct {
		name string
		prs  []int
		want []string
	}{
		{"no parked PR on the branch", nil, []string{"invalidate-repo " + origin, "base-moved main"}},
		{"one parked PR", []int{7}, []string{"invalidate-repo " + origin, "base-moved main", "invalidate " + origin + "#7"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &orderLog{}
			dispatcher := NewWebhookDispatcher(orderedRefresher{log}, zerolog.Nop()).
				WithReadInvalidator(orderedRepoInvalidator{orderedInvalidator{log}}).
				WithPRPoller(orderedPRPoller{log}).
				WithBaseMoves(orderedBaseMoves{log, tc.prs})
			if err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
				EventType:     "push",
				RepoOriginUrl: origin,
				Payload:       []byte(`{"ref":"refs/heads/main"}`),
			}); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if steps := log.snapshot(); !slices.Equal(steps, tc.want) {
				t.Fatalf("steps = %v, want %v", steps, tc.want)
			}
		})
	}
}
