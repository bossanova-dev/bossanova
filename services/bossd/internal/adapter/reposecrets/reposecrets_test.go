package reposecrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossd/internal/adapter/reposecrets"
	"github.com/recurser/bossd/internal/adapter/reposecrets/reposecretstest"
)

// markerSecrets builds marker values at runtime so no literal secret-shaped
// string is committed.
func markerSecrets(tag string) domain.RepoSecrets {
	return domain.RepoSecrets{
		LinearAPIKey: "lin-marker-" + tag,
		SentryAPIKey: "sen-marker-" + tag,
	}
}

func TestStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	ring := reposecretstest.NewRing()
	store := reposecretstest.NewStore(ring)

	want := markerSecrets("rt")
	if err := store.Set(ctx, "repo-1", want); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !ring.Has(reposecrets.ItemKey("repo-1")) {
		t.Fatalf("keyring has no item %q after Set", reposecrets.ItemKey("repo-1"))
	}

	// A fresh store over the same ring reads the keyring, not a cache.
	fresh := reposecretstest.NewStore(ring)
	got, err := fresh.Get(ctx, "repo-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != want {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}

	// A repo with no item reads back empty, not as an error.
	empty, err := fresh.Get(ctx, "repo-unknown")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if !empty.IsEmpty() {
		t.Fatalf("Get missing = %+v, want empty", empty)
	}
}

func TestStore_CachesReadsAndInvalidatesOnWrite(t *testing.T) {
	ctx := context.Background()
	ring := reposecretstest.NewRing()
	seed := reposecretstest.NewStore(ring)
	if err := seed.Set(ctx, "repo-1", markerSecrets("v1")); err != nil {
		t.Fatalf("seed Set: %v", err)
	}

	store := reposecretstest.NewStore(ring)
	for i := 0; i < 3; i++ {
		if _, err := store.Get(ctx, "repo-1"); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
	if n := ring.GetCount(); n != 1 {
		t.Fatalf("keyring reads = %d after 3 Gets, want 1 (cached)", n)
	}

	// Set replaces the cached value.
	v2 := markerSecrets("v2")
	if err := store.Set(ctx, "repo-1", v2); err != nil {
		t.Fatalf("Set v2: %v", err)
	}
	if got, _ := store.Get(ctx, "repo-1"); got != v2 {
		t.Fatalf("Get after Set = %+v, want %+v", got, v2)
	}

	// A failed write must not leave the old value cached as if current.
	ring.SetFailSet(true)
	if err := store.Set(ctx, "repo-1", markerSecrets("v3")); err == nil {
		t.Fatal("Set with failing keyring returned nil error")
	}
	ring.SetFailSet(false)
	before := ring.GetCount()
	if got, _ := store.Get(ctx, "repo-1"); got != v2 {
		t.Fatalf("Get after failed Set = %+v, want keyring value %+v", got, v2)
	}
	if ring.GetCount() != before+1 {
		t.Fatal("Get after failed Set served the cache instead of re-reading the keyring")
	}

	// Forget drops the cache entry.
	store.Forget("repo-1")
	before = ring.GetCount()
	if _, err := store.Get(ctx, "repo-1"); err != nil {
		t.Fatalf("Get after Forget: %v", err)
	}
	if ring.GetCount() != before+1 {
		t.Fatal("Get after Forget did not re-read the keyring")
	}
}

func TestStore_Delete(t *testing.T) {
	ctx := context.Background()
	ring := reposecretstest.NewRing()
	store := reposecretstest.NewStore(ring)
	if err := store.Set(ctx, "repo-1", markerSecrets("d")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Delete(ctx, "repo-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ring.Has(reposecrets.ItemKey("repo-1")) {
		t.Fatal("keyring item survived Delete")
	}
	if got, _ := store.Get(ctx, "repo-1"); !got.IsEmpty() {
		t.Fatalf("Get after Delete = %+v, want empty", got)
	}
	// Deleting again (no item) is not an error.
	if err := store.Delete(ctx, "repo-1"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	// Setting empty secrets deletes the item too.
	if err := store.Set(ctx, "repo-2", markerSecrets("e")); err != nil {
		t.Fatalf("Set repo-2: %v", err)
	}
	if err := store.Set(ctx, "repo-2", domain.RepoSecrets{}); err != nil {
		t.Fatalf("Set empty: %v", err)
	}
	if ring.Has(reposecrets.ItemKey("repo-2")) {
		t.Fatal("Set(empty) left a keyring item behind")
	}
}

func TestStore_UnavailableKeyring(t *testing.T) {
	ctx := context.Background()
	store := reposecrets.NewUnavailable()
	if err := store.Set(ctx, "repo-1", markerSecrets("u")); !errors.Is(err, domain.ErrRepoSecretsUnavailable) {
		t.Fatalf("Set err = %v, want ErrUnavailable", err)
	}
	if _, err := store.Get(ctx, "repo-1"); !errors.Is(err, domain.ErrRepoSecretsUnavailable) {
		t.Fatalf("Get err = %v, want ErrUnavailable", err)
	}
	if err := store.Delete(ctx, "repo-1"); !errors.Is(err, domain.ErrRepoSecretsUnavailable) {
		t.Fatalf("Delete err = %v, want ErrUnavailable", err)
	}
}

func TestStore_ErrorsNeverCarrySecretValues(t *testing.T) {
	ctx := context.Background()
	ring := reposecretstest.NewRing()
	store := reposecretstest.NewStore(ring)
	sec := markerSecrets("leak")
	ring.SetFailSet(true)
	err := store.Set(ctx, "repo-1", sec)
	if err == nil {
		t.Fatal("expected an error from a failing keyring")
	}
	for _, v := range []string{sec.LinearAPIKey, sec.SentryAPIKey} {
		if strings.Contains(err.Error(), v) {
			t.Fatal("Set error string carries a secret value")
		}
	}
}

// awaitEntered waits until a call on key is blocked inside ring.
func awaitEntered(t *testing.T, ring *reposecretstest.Ring, key string) {
	t.Helper()
	select {
	case got := <-ring.Entered():
		if got != key {
			t.Fatalf("ring call entered on %q, want %q", got, key)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no ring call on %q started", key)
	}
}

func encoded(t *testing.T, sec domain.RepoSecrets) string {
	t.Helper()
	data, err := json.Marshal(sec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// promptly is how long a call that must not wait on a blocked ring may take.
const promptly = 2 * time.Second

// within runs call and fails the test, naming what, if it has not returned
// within promptly; a call stuck behind a blocked ring reads as a failure
// rather than a hung test.
func within(t *testing.T, what string, call func() error) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- call() }()
	select {
	case err := <-errCh:
		return err
	case <-time.After(promptly):
		t.Fatalf("%s did not return within %v while another ring call was blocked", what, promptly)
		return nil
	}
}

func TestStore_BlockedItemDoesNotBlockOtherItemsOrCacheHits(t *testing.T) {
	ctx := context.Background()
	g := reposecretstest.NewRing()
	store := reposecretstest.NewStore(g)
	if err := store.Set(ctx, "repo-cached", markerSecrets("c")); err != nil {
		t.Fatalf("seed cached: %v", err)
	}
	if err := store.Set(ctx, "repo-b", markerSecrets("b")); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	store.Forget("repo-b") // force repo-b's Get to reach the ring

	release := g.Block(reposecrets.ItemKey("repo-a"))
	defer release()
	blocked := make(chan error, 1)
	go func() {
		_, err := store.Get(ctx, "repo-a")
		blocked <- err
	}()
	awaitEntered(t, g, reposecrets.ItemKey("repo-a"))

	var got domain.RepoSecrets
	err := within(t, "Get repo-b while repo-a blocked", func() (err error) {
		got, err = store.Get(ctx, "repo-b")
		return err
	})
	if err != nil || got != markerSecrets("b") {
		t.Fatalf("Get repo-b while repo-a blocked = %+v, %v; want %+v, nil", got, err, markerSecrets("b"))
	}
	err = within(t, "cached Get while repo-a blocked", func() (err error) {
		got, err = store.Get(ctx, "repo-cached")
		return err
	})
	if err != nil || got != markerSecrets("c") {
		t.Fatalf("cached Get while repo-a blocked = %+v, %v; want %+v, nil", got, err, markerSecrets("c"))
	}
	err = within(t, "Set repo-b while repo-a blocked", func() error {
		return store.Set(ctx, "repo-b", markerSecrets("b2"))
	})
	if err != nil {
		t.Fatalf("Set repo-b while repo-a blocked: %v", err)
	}

	release()
	if err := <-blocked; err != nil {
		t.Fatalf("blocked Get after release: %v", err)
	}
}

// awaitPending waits until n ops are queued or running on repo-a's item (the
// item every ordering test blocks), so a test knows a call it started is in
// the queue before acting on it.
func awaitPending(t *testing.T, store *reposecrets.Store, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for store.Pending("repo-a") != n {
		if time.Now().After(deadline) {
			t.Fatalf("pending ops on repo-a = %d, want %d", store.Pending("repo-a"), n)
		}
		runtime.Gosched()
	}
}

func TestStore_ReturnsCtxErrWhileRingBlocked(t *testing.T) {
	key := reposecrets.ItemKey("repo-a")
	for _, tc := range []struct {
		name string
		call func(ctx context.Context, s *reposecrets.Store) error
	}{
		{"Get", func(ctx context.Context, s *reposecrets.Store) error { _, err := s.Get(ctx, "repo-a"); return err }},
		{"Set", func(ctx context.Context, s *reposecrets.Store) error { return s.Set(ctx, "repo-a", markerSecrets("x")) }},
		{"Delete", func(ctx context.Context, s *reposecrets.Store) error { return s.Delete(ctx, "repo-a") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := reposecretstest.NewRing()
			store := reposecretstest.NewStore(g)
			release := g.Block(key)
			defer release()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- tc.call(ctx, store) }()
			awaitEntered(t, g, key)
			cancel()
			select {
			case err := <-errCh:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("%s err = %v, want context.Canceled", tc.name, err)
				}
			case <-time.After(promptly):
				t.Fatalf("%s did not return within %v of its ctx ending while the ring was blocked", tc.name, promptly)
			}
		})
	}
}

func TestStore_AbandonedSetNeverLandsAfterLaterWrite(t *testing.T) {
	ctx := context.Background()
	key := reposecrets.ItemKey("repo-a")
	g := reposecretstest.NewRing()
	store := reposecretstest.NewStore(g)

	// Hold repo-a's item with a slow write already inside the ring.
	release := g.Block(key)
	defer release()
	first := make(chan error, 1)
	go func() { first <- store.Set(ctx, "repo-a", markerSecrets("first")) }()
	awaitEntered(t, g, key)

	// A Set queued behind it whose caller gives up.
	abandonCtx, abandon := context.WithCancel(ctx)
	abandoned := make(chan error, 1)
	go func() { abandoned <- store.Set(abandonCtx, "repo-a", markerSecrets("abandoned")) }()
	awaitPending(t, store, 2)
	abandon()
	if err := within(t, "abandoned Set", func() error { return <-abandoned }); !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned Set err = %v, want context.Canceled", err)
	}

	// The caller's next write, issued after it saw the abandoned Set fail.
	later := make(chan error, 1)
	go func() { later <- store.Set(ctx, "repo-a", markerSecrets("later")) }()
	awaitPending(t, store, 3)

	release()
	if err := <-first; err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if err := <-later; err != nil {
		t.Fatalf("later Set: %v", err)
	}

	want := []string{encoded(t, markerSecrets("first")), encoded(t, markerSecrets("later"))}
	if got := g.Writes(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ring took %d writes, want exactly first then later; a cancelled queued write must be skipped", len(got))
	}
	fresh := reposecretstest.NewStore(g)
	if got, err := fresh.Get(ctx, "repo-a"); err != nil || got != markerSecrets("later") {
		t.Fatalf("keyring holds %+v (err %v), want the later write %+v", got, err, markerSecrets("later"))
	}
	if got, err := store.Get(ctx, "repo-a"); err != nil || got != markerSecrets("later") {
		t.Fatalf("cache holds %+v (err %v), want the later write %+v", got, err, markerSecrets("later"))
	}
}

func TestStore_InFlightAbandonedSetLandsBeforeLaterWrite(t *testing.T) {
	ctx := context.Background()
	key := reposecrets.ItemKey("repo-a")
	g := reposecretstest.NewRing()
	store := reposecretstest.NewStore(g)

	// The abandoned Set is already inside the ring when its caller gives up,
	// so it may complete, but only before the caller's next write.
	release := g.Block(key)
	defer release()
	abandonCtx, abandon := context.WithCancel(ctx)
	abandoned := make(chan error, 1)
	go func() { abandoned <- store.Set(abandonCtx, "repo-a", markerSecrets("abandoned")) }()
	awaitEntered(t, g, key)
	abandon()
	if err := within(t, "abandoned Set", func() error { return <-abandoned }); !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned Set err = %v, want context.Canceled", err)
	}

	later := make(chan error, 1)
	go func() { later <- store.Set(ctx, "repo-a", markerSecrets("later")) }()
	awaitPending(t, store, 2)
	// The later write waits in the queue rather than entering the ring beside
	// the stuck one. This is a bounded check for an absence: correct code
	// passes it however long it waits, so the bound only sets how quickly a
	// write that does enter is caught.
	select {
	case k := <-g.Entered():
		t.Fatalf("a second ring call on %q started while the first was still in flight", k)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-later; err != nil {
		t.Fatalf("later Set: %v", err)
	}

	want := []string{encoded(t, markerSecrets("abandoned")), encoded(t, markerSecrets("later"))}
	if got := g.Writes(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ring writes out of order: got %d writes, want abandoned then later", len(got))
	}
	if got, err := store.Get(ctx, "repo-a"); err != nil || got != markerSecrets("later") {
		t.Fatalf("cache holds %+v (err %v), want the later write %+v", got, err, markerSecrets("later"))
	}
}

// TestStore_AbandonedCallsDoNotParkGoroutines: callers giving up on a stuck
// item leave their op queued as data. A goroutine per abandoned call, each
// parked until the keychain answers, would grow without bound while it hangs.
func TestStore_AbandonedCallsDoNotParkGoroutines(t *testing.T) {
	ctx := context.Background()
	key := reposecrets.ItemKey("repo-a")
	g := reposecretstest.NewRing()
	store := reposecretstest.NewStore(g)
	release := g.Block(key)
	defer release()
	first := make(chan error, 1)
	go func() { first <- store.Set(ctx, "repo-a", markerSecrets("first")) }()
	awaitEntered(t, g, key)

	const abandonedCalls = 50
	before := runtime.NumGoroutine()
	for i := 0; i < abandonedCalls; i++ {
		callCtx, cancel := context.WithCancel(ctx)
		errCh := make(chan error, 1)
		go func() { errCh <- store.Set(callCtx, "repo-a", markerSecrets("abandoned")) }()
		awaitPending(t, store, i+2)
		cancel()
		if err := within(t, "abandoned Set", func() error { return <-errCh }); !errors.Is(err, context.Canceled) {
			t.Fatalf("abandoned Set %d err = %v, want context.Canceled", i, err)
		}
	}
	// The callers' goroutines have returned; allow them a moment to exit and
	// leave generous slack for runtime goroutines, far below one per call.
	if grew := runtime.NumGoroutine() - before; grew >= abandonedCalls/2 {
		t.Fatalf("%d abandoned calls on a stuck item left %d more goroutines, want the queue to hold them as data", abandonedCalls, grew)
	}

	release()
	if err := <-first; err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if got, err := store.Get(ctx, "repo-a"); err != nil || got != markerSecrets("first") {
		t.Fatalf("Get after release = %+v (err %v), want the first write", got, err)
	}
	if n := len(g.Writes()); n != 1 {
		t.Fatalf("ring took %d writes, want 1: every abandoned queued write must be skipped", n)
	}
}

// TestStore_RestoreLandsAfterAbandonedWriteAndBeforeLaterOps: Restore is the
// compensation for an abandoned write. It lands after that write even though
// the write is still stuck in the keychain when Restore is called, and before
// any op queued after it, so a later write survives and a later read sees the
// restored value.
func TestStore_RestoreLandsAfterAbandonedWriteAndBeforeLaterOps(t *testing.T) {
	ctx := context.Background()
	key := reposecrets.ItemKey("repo-a")
	g := reposecretstest.NewRing()
	store := reposecretstest.NewStore(g)
	if err := store.Set(ctx, "repo-a", markerSecrets("old")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	release := g.Block(key)
	defer release()
	abandonCtx, abandon := context.WithCancel(ctx)
	abandoned := make(chan error, 1)
	go func() { abandoned <- store.Set(abandonCtx, "repo-a", markerSecrets("abandoned")) }()
	awaitEntered(t, g, key)
	abandon()
	if err := within(t, "abandoned Set", func() error { return <-abandoned }); !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned Set err = %v, want context.Canceled", err)
	}

	var landed <-chan struct{}
	if err := within(t, "Restore", func() error { landed = store.Restore("repo-a", markerSecrets("old")); return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-landed:
		t.Fatal("Restore reported landing while the write it follows is still stuck")
	default:
	}

	read := make(chan domain.RepoSecrets, 1)
	go func() {
		sec, err := store.Get(ctx, "repo-a")
		if err != nil {
			t.Errorf("queued Get: %v", err)
		}
		read <- sec
	}()
	awaitPending(t, store, 3)
	later := make(chan error, 1)
	go func() { later <- store.Set(ctx, "repo-a", markerSecrets("later")) }()
	awaitPending(t, store, 4)

	release()
	if err := within(t, "later Set", func() error { return <-later }); err != nil {
		t.Fatalf("later Set: %v", err)
	}
	<-landed
	if got := <-read; got != markerSecrets("old") {
		t.Fatalf("Get queued after Restore = %+v, want the restored value", got)
	}
	want := []string{
		encoded(t, markerSecrets("old")), encoded(t, markerSecrets("abandoned")),
		encoded(t, markerSecrets("old")), encoded(t, markerSecrets("later")),
	}
	got := g.Writes()
	if len(got) != len(want) {
		t.Fatalf("ring took %d writes, want %d (seed, abandoned, restore, later)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ring write %d out of order; want seed, abandoned, restore, later", i)
		}
	}
	if sec, err := reposecretstest.NewStore(g).Get(ctx, "repo-a"); err != nil || sec != markerSecrets("later") {
		t.Fatalf("keyring holds %+v (err %v), want the later write", sec, err)
	}
}

// panicRing wraps a Ring and panics on its next Set, once.
type panicRing struct {
	reposecrets.Ring
	armed bool
}

func (p *panicRing) Set(key string, data []byte) error {
	if p.armed {
		p.armed = false
		panic("injected keyring panic")
	}
	return p.Ring.Set(key, data)
}

// TestStore_PanickingRingCallFailsItsOpAndFreesTheItem: a Ring call that
// panics must fail only its own op. The caller waiting on it gets an error
// rather than a reported success, and the item's queue keeps running, so the
// next op on the same item completes instead of waiting forever behind a
// worker that died.
func TestStore_PanickingRingCallFailsItsOpAndFreesTheItem(t *testing.T) {
	ctx := context.Background()
	ring := &panicRing{Ring: reposecretstest.NewRing(), armed: true}
	store := reposecrets.New(func() (reposecrets.Ring, error) { return ring, nil })

	err := within(t, "Set whose ring call panics", func() error {
		return store.Set(ctx, "repo-a", markerSecrets("panic"))
	})
	if err == nil {
		t.Fatal("Set reported success although its keyring call panicked")
	}
	if strings.Contains(err.Error(), "lin-marker") || strings.Contains(err.Error(), "sen-marker") {
		t.Fatal("panic error carries a secret value")
	}

	want := markerSecrets("after")
	err = within(t, "next Set on the item whose ring call panicked", func() error {
		return store.Set(ctx, "repo-a", want)
	})
	if err != nil {
		t.Fatalf("Set after a panicked op: %v", err)
	}
	got, err := store.Get(ctx, "repo-a")
	if err != nil || got != want {
		t.Fatalf("Get after a panicked op = %+v, %v; want %+v, nil", got, err, want)
	}
}
