package callback

import (
	"context"
	"errors"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// fakeInterestStore serves canned callbacks, honouring the State filter so
// DeriveInterests's per-state queries behave like the real store.
type fakeInterestStore struct {
	callbacks []*models.GithubCallback
	err       error
}

func (f *fakeInterestStore) List(_ context.Context, filter db.ListGithubCallbacksFilter) ([]*models.GithubCallback, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*models.GithubCallback
	for _, cb := range f.callbacks {
		if filter.State != nil && cb.State != *filter.State {
			continue
		}
		out = append(out, cb)
	}
	return out, nil
}

func cb(owner, name string, pr int, state models.GithubCallbackState) *models.GithubCallback {
	return &models.GithubCallback{RepoOwner: owner, RepoName: name, PRNumber: pr, State: state}
}

func TestDeriveInterests_Empty(t *testing.T) {
	store := &fakeInterestStore{}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}

func TestDeriveInterests_DedupesAndGroups(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("acme", "widget", 7, models.GithubCallbackStateActive),
		cb("acme", "widget", 7, models.GithubCallbackStateLeased),    // dup of above
		cb("acme", "widget", 7, models.GithubCallbackStateTriggered), // dup of above
		cb("acme", "widget", 9, models.GithubCallbackStateActive),    // same repo, diff PR
		cb("acme", "gadget", 7, models.GithubCallbackStateActive),    // diff repo, same PR
	}}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	want := []*pb.CallbackInterest{
		{RepoOriginUrl: "https://github.com/acme/gadget", PrNumber: 7},
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 7},
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 9},
	}
	assertInterests(t, got, want)
}

func TestDeriveInterests_ExcludesTerminalStates(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("acme", "widget", 1, models.GithubCallbackStateDelivered),
		cb("acme", "widget", 2, models.GithubCallbackStateCanceled),
		cb("acme", "widget", 3, models.GithubCallbackStateExpired),
		cb("acme", "widget", 4, models.GithubCallbackStateActive),
		cb("acme", "widget", 5, models.GithubCallbackStateLeased),
		cb("acme", "widget", 6, models.GithubCallbackStateTriggered),
	}}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	want := []*pb.CallbackInterest{
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 4},
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 5},
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 6},
	}
	assertInterests(t, got, want)
}

func TestDeriveInterests_SortedDeterministically(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("zeta", "repo", 3, models.GithubCallbackStateActive),
		cb("alpha", "repo", 5, models.GithubCallbackStateActive),
		cb("alpha", "repo", 2, models.GithubCallbackStateActive),
		cb("mid", "repo", 1, models.GithubCallbackStateActive),
	}}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	want := []*pb.CallbackInterest{
		{RepoOriginUrl: "https://github.com/alpha/repo", PrNumber: 2},
		{RepoOriginUrl: "https://github.com/alpha/repo", PrNumber: 5},
		{RepoOriginUrl: "https://github.com/mid/repo", PrNumber: 1},
		{RepoOriginUrl: "https://github.com/zeta/repo", PrNumber: 3},
	}
	assertInterests(t, got, want)
}

func TestDeriveInterests_URLRoundTripsWithGitHubNWO(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("owner", "repo", 42, models.GithubCallbackStateActive),
	}}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 interest, got %d", len(got))
	}
	// The URL bossd advertises MUST map back to the same owner/name the
	// webhook dispatcher derives via vcs.GitHubNWO — that is the routing key.
	if nwo := vcs.GitHubNWO(got[0].GetRepoOriginUrl()); nwo != "owner/repo" {
		t.Errorf("GitHubNWO(%q) = %q, want owner/repo", got[0].GetRepoOriginUrl(), nwo)
	}
}

func TestDeriveInterests_StoreError(t *testing.T) {
	store := &fakeInterestStore{err: errors.New("boom")}
	if _, err := DeriveInterests(context.Background(), store); err == nil {
		t.Fatal("want error, got nil")
	}
}

// --- advertiser ---

type capturePublisher struct {
	sets [][]*pb.CallbackInterest
}

func (c *capturePublisher) PublishInterests(interests []*pb.CallbackInterest) {
	c.sets = append(c.sets, interests)
}

func TestInterestAdvertiser_PublishesOnChangeAndSuppressesIdentical(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("acme", "widget", 7, models.GithubCallbackStateActive),
	}}
	pub := &capturePublisher{}
	adv := NewInterestAdvertiser(AdvertiserConfig{Store: store, Publisher: pub, Logger: zerolog.Nop()})

	// First derive publishes.
	published, err := adv.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !published || len(pub.sets) != 1 {
		t.Fatalf("first Publish should publish once, published=%v sets=%d", published, len(pub.sets))
	}

	// Identical set is suppressed.
	published, err = adv.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published || len(pub.sets) != 1 {
		t.Fatalf("identical Publish should suppress, published=%v sets=%d", published, len(pub.sets))
	}
}

func TestInterestAdvertiser_PublishesEmptyOnDrain(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("acme", "widget", 7, models.GithubCallbackStateActive),
	}}
	pub := &capturePublisher{}
	adv := NewInterestAdvertiser(AdvertiserConfig{Store: store, Publisher: pub, Logger: zerolog.Nop()})

	if _, err := adv.Publish(context.Background()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Drain all callbacks (e.g. delivered) → interest withdrawn.
	store.callbacks = nil
	published, err := adv.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !published {
		t.Fatal("drain should publish the empty withdrawal set")
	}
	last := pub.sets[len(pub.sets)-1]
	if len(last) != 0 {
		t.Fatalf("withdrawal set should be empty, got %v", last)
	}
}

// assertInterests compares two interest slices element-wise (order-sensitive).
func assertInterests(t *testing.T, got, want []*pb.CallbackInterest) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].GetRepoOriginUrl() != want[i].GetRepoOriginUrl() || got[i].GetPrNumber() != want[i].GetPrNumber() {
			t.Errorf("interest[%d] = {%q, %d}, want {%q, %d}", i,
				got[i].GetRepoOriginUrl(), got[i].GetPrNumber(),
				want[i].GetRepoOriginUrl(), want[i].GetPrNumber())
		}
	}
}
