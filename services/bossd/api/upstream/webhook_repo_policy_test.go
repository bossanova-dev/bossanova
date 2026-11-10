package upstream

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/rs/zerolog"
)

func TestWebhookPayloadNeedsRegisteredTargets(t *testing.T) {
	emitter := &fakeEmitter{}
	dispatcher := NewWebhookDispatcherWithEmitter(&fakePRRefresher{}, emitter, zerolog.Nop()).WithCloudControl(fixedCloudControl("full"))
	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{RepoOriginUrl: "https://github.com/owner/repo", PullRequest: 42, EventType: "pull_request", Payload: []byte(`{"action":"closed","pull_request":{"number":42,"merged":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(emitter.calls) != 0 {
		t.Fatal("payload emitted without locally registered targets")
	}
}

type webhookTargetFixture struct {
	repo    string
	objects []CommandTargetObject
	err     error
}

func (f webhookTargetFixture) WebhookTargets(context.Context, string, int) (string, []CommandTargetObject, error) {
	return f.repo, f.objects, f.err
}

func TestWebhookRepoPayloadAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, origin, repo string
		mode               config.CloudControlMode
		objects            []CommandTargetObject
		wantPayload        bool
	}{
		{name: "granted repo", origin: "https://github.com/owner/repo", repo: "a", mode: config.CloudControlFull, objects: []CommandTargetObject{{RepoID: "a"}}, wantPayload: true},
		{name: "read only repo", origin: "https://github.com/owner/repo", repo: "a", mode: config.CloudControlReadOnly},
		{name: "private repo", origin: "https://github.com/owner/repo", repo: "a", mode: config.CloudControlPrivate},
		{name: "unregistered", origin: "https://github.com/owner/repo", mode: config.CloudControlFull},
		{name: "unparseable", origin: "not an origin", repo: "a", mode: config.CloudControlFull},
		{name: "mixed sessions", origin: "https://github.com/owner/repo", repo: "a", mode: config.CloudControlFull, objects: []CommandTargetObject{{RepoID: "a"}, {RepoID: "b"}}},
		{name: "unresolved chat", origin: "https://github.com/owner/repo", repo: "a", mode: config.CloudControlFull, objects: []CommandTargetObject{{RepoID: ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitter := &fakeEmitter{}
			refresh := &fakePRRefresher{}
			d := NewWebhookDispatcherWithEmitter(refresh, emitter, zerolog.Nop()).WithCloudControl(targetPolicy{machine: config.CloudControlReadOnly, repos: map[string]config.CloudControlMode{"a": tc.mode}}).WithWebhookTargets(webhookTargetFixture{repo: tc.repo, objects: tc.objects})
			err := d.Dispatch(context.Background(), &pb.WebhookEvent{RepoOriginUrl: tc.origin, PullRequest: 42, EventType: "pull_request", Payload: []byte(`{"action":"closed","pull_request":{"number":42,"merged":true}}`)})
			if err != nil {
				t.Fatal(err)
			}
			if (len(emitter.calls) > 0) != tc.wantPayload {
				t.Fatalf("payload emitted=%v want=%v", len(emitter.calls) > 0, tc.wantPayload)
			}
			if len(refresh.calls) != 1 || refresh.calls[0].prNumber != 42 {
				t.Fatal("hint did not re-read envelope PR")
			}
		})
	}
}
