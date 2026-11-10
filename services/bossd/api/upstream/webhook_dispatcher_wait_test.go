package upstream

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/rs/zerolog"
)

// orderedWaitNotifier records PR wait notifications in the dispatcher's order
// log, beside the invalidations they must follow.
type orderedWaitNotifier struct{ log *orderLog }

func (n orderedWaitNotifier) NotifyPR(repo string, pr int) {
	n.log.add(fmt.Sprintf("notify %s#%d", repo, pr))
}

func (n orderedWaitNotifier) NotifyRepo(repo string) { n.log.add("notify-repo " + repo) }

func isNotify(step string) bool { return strings.HasPrefix(step, "notify") }

// TestDispatch_NotifiesPRWaitsAfterEachInvalidation pins the wake sites: a PR
// delivery notifies its PR right after InvalidatePR, a PR-less delivery and a
// push notify the repo right after InvalidateRepo, and nothing is notified
// when no read was dropped.
func TestDispatch_NotifiesPRWaitsAfterEachInvalidation(t *testing.T) {
	const origin = "https://github.com/owner/repo"
	repoInv := func(l *orderLog) vcs.ReadInvalidator { return orderedRepoInvalidator{orderedInvalidator{l}} }
	prOnlyInv := func(l *orderLog) vcs.ReadInvalidator { return orderedInvalidator{l} }
	noInv := func(*orderLog) vcs.ReadInvalidator { return nil }
	prEvent := &pb.WebhookEvent{EventType: "check_suite", RepoOriginUrl: origin, PullRequest: 42}
	statusEvent := &pb.WebhookEvent{EventType: "status", RepoOriginUrl: origin}
	pushEvent := &pb.WebhookEvent{EventType: "push", RepoOriginUrl: origin, Payload: []byte(`{"ref":"refs/heads/main"}`)}
	for _, tc := range []struct {
		name        string
		ev          *pb.WebhookEvent
		invalidator func(*orderLog) vcs.ReadInvalidator
		wantPrefix  []string
	}{
		{"PR delivery", prEvent, repoInv, []string{"invalidate " + origin + "#42", "notify " + origin + "#42"}},
		{"PR-less status", statusEvent, repoInv, []string{"invalidate-repo " + origin, "notify-repo " + origin}},
		{"push", pushEvent, repoInv, []string{"invalidate-repo " + origin, "notify-repo " + origin, "base-moved main", "invalidate " + origin + "#7"}},
		{"PR delivery without an invalidator", prEvent, noInv, nil},
		{"PR-less status without an invalidator", statusEvent, noInv, nil},
		{"push without an invalidator", pushEvent, noInv, []string{"base-moved main"}},
		{"PR-less status with a PR-only invalidator", statusEvent, prOnlyInv, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &orderLog{}
			dispatcher := NewWebhookDispatcher(orderedRefresher{log}, zerolog.Nop()).WithCloudControl(fixedCloudControl("full")).WithWebhookTargets(webhookTargetFixture{repo: "repo"}).
				WithPRPoller(orderedPRPoller{log}).
				WithBaseMoves(orderedBaseMoves{log, []int{7}}).
				WithPRWaiters(orderedWaitNotifier{log})
			if inv := tc.invalidator(log); inv != nil {
				dispatcher = dispatcher.WithReadInvalidator(inv)
			}
			polled := func() {}
			if tc.ev.GetPullRequest() != 0 {
				hook, wait := captureEvalDone(t)
				dispatcher.evalDoneHook, polled = hook, wait
			}
			if err := dispatcher.Dispatch(context.Background(), tc.ev); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			polled()
			steps := log.snapshot()
			if len(steps) < len(tc.wantPrefix) || !slices.Equal(steps[:len(tc.wantPrefix)], tc.wantPrefix) {
				t.Fatalf("steps = %v, want them to start %v", steps, tc.wantPrefix)
			}
			if slices.ContainsFunc(tc.wantPrefix, isNotify) != slices.ContainsFunc(steps, isNotify) {
				t.Fatalf("steps = %v, want a PR wait notification only where %v has one", steps, tc.wantPrefix)
			}
		})
	}
}
