package credmaterialize

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const (
	failureAccount        = "shared"
	failureSentinelAccess = "sentinel-access-7f3a91"
	failureSentinelSecret = "sentinel-refresh-9c1d44"
	failureSentinelID     = "sentinel-id-4b2e07"
)

var failureBlob = []byte(`{"tokens":{"access_token":"` + failureSentinelAccess + `","refresh_token":"` + failureSentinelSecret + `","id_token":"` + failureSentinelID + `"}}`)

type failureGate struct {
	receiveErr error
	acquire    func() (bool, error)
}

func (g *failureGate) Receive(context.Context, string) error { return g.receiveErr }
func (g *failureGate) Acquire(context.Context, string, []byte, string) (bool, error) {
	if g.acquire != nil {
		return g.acquire()
	}
	return true, nil
}
func (g *failureGate) PublishRefresh(context.Context, string, []byte, []byte) error { return nil }

type failureFixture struct {
	broker         *RefreshBroker
	store          *brokerMemory
	gate           *failureGate
	logs           *bytes.Buffer
	providerStatus *atomic.Int64
	capability     string
}

func newFailureFixture(t *testing.T) *failureFixture {
	t.Helper()
	f := &failureFixture{store: &brokerMemory{}, gate: &failureGate{}, logs: &bytes.Buffer{}, providerStatus: &atomic.Int64{}}
	f.store.setBlob(failureBlob)
	f.providerStatus.Store(http.StatusOK)
	b, err := NewRefreshBroker(f.store, f.gate, zerolog.New(f.logs))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(f.providerStatus.Load()))
		_, _ = io.WriteString(w, `{"access_token":"next-access","refresh_token":"next-secret","id_token":"next-id"}`)
	}))
	t.Cleanup(provider.Close)
	b.providerURL = provider.URL
	f.broker = b
	f.capability, err = b.Capability(context.Background(), failureAccount, failureBlob)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *failureFixture) serve(method, capability string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"client_id": codexOAuthClientID, "grant_type": "refresh_token", "refresh_token": capability})
	recorder := httptest.NewRecorder()
	f.broker.serve(recorder, httptest.NewRequest(method, "/refresh", bytes.NewReader(body)))
	return recorder
}

func (f *failureFixture) markConsumed(t *testing.T) {
	t.Helper()
	raw, _ := f.store.LoadRefreshState(context.Background(), failureAccount)
	var j refreshJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	digest, err := refreshSecretDigest(failureBlob)
	if err != nil {
		t.Fatal(err)
	}
	j.Consumed = append(j.Consumed, digest)
	if err := f.broker.saveJournal(context.Background(), failureAccount, j); err != nil {
		t.Fatal(err)
	}
}

func forgedCapability(accountID string) string {
	raw, _ := json.Marshal(refreshCapability{AccountID: accountID, Digest: strings.Repeat("a", 43), BrokerID: "forged"})
	return brokerCapabilityPrefix + base64.RawURLEncoding.EncodeToString(raw) + ".forged-mac"
}

func gateFailure(class RefreshFailureClass, text string) func(*testing.T, *failureFixture) {
	return func(_ *testing.T, f *failureFixture) { f.gate.receiveErr = NewRefreshFailure(class, errors.New(text)) }
}

type failureCase struct {
	name       string
	setup      func(*testing.T, *failureFixture)
	method     string
	capability func(*failureFixture) string
	status     int
	class      RefreshFailureClass
	remedy     RefreshRemedy
	hint       string
	logAccount string // "account_id" or "claimed_account_id"; empty means neither
	records    bool   // the failure is a refresh failure recorded for RefreshFailureSince
}

func refreshFailureCases() []failureCase {
	refresh := func(name string, class RefreshFailureClass, remedy RefreshRemedy, hint string, setup func(*testing.T, *failureFixture)) failureCase {
		return failureCase{name: name, setup: setup, status: http.StatusServiceUnavailable, class: class, remedy: remedy, hint: hint, logAccount: "account_id", records: true}
	}
	// A journal the broker cannot read fails before the capability authenticates,
	// so its line names the claimed account rather than an authenticated one.
	journal := func(name string, setup func(*testing.T, *failureFixture)) failureCase {
		c := refresh(name, RefreshFailureJournalUnreadable, RefreshRemedyOperatorAction, "check `boss account devices ls`", setup)
		c.logAccount = "claimed_account_id"
		return c
	}
	return []failureCase{
		refresh("peer owns refresh", RefreshFailureOwnedByPeer, RefreshRemedyTransient, "retry shortly", func(_ *testing.T, f *failureFixture) {
			f.broker.peerWait = time.Millisecond
			f.gate.acquire = func() (bool, error) { return false, nil }
		}),
		refresh("sync unreachable", RefreshFailureSyncUnreachable, RefreshRemedyTransient, "retry shortly", func(_ *testing.T, f *failureFixture) {
			f.gate.receiveErr = fmt.Errorf("receive credential: %w", context.DeadlineExceeded)
		}),
		refresh("credential changed while acquiring", RefreshFailureCredentialChanged, RefreshRemedyTransient, "retry shortly", func(_ *testing.T, f *failureFixture) {
			f.gate.acquire = func() (bool, error) {
				f.store.setBlob(bytes.ReplaceAll(failureBlob, []byte(failureSentinelAccess), []byte("rotated-access")))
				return true, nil
			}
		}),
		refresh("unclassified gate error", RefreshFailureUnclassified, RefreshRemedyTransient, "retry shortly", func(_ *testing.T, f *failureFixture) {
			f.gate.receiveErr = errors.New("resolve refresh version: no published version")
		}),
		refresh("identity not pinned", RefreshFailureIdentityNotPinned, RefreshRemedyOperatorAction, "confirm the bootstrap fingerprint with `boss account devices approve`",
			gateFailure(RefreshFailureIdentityNotPinned, "credential bootstrap identity is not locally pinned")),
		refresh("device not enrolled", RefreshFailureDeviceNotEnrolled, RefreshRemedyOperatorAction, "run `boss account devices approve` on an enrolled machine",
			gateFailure(RefreshFailureDeviceNotEnrolled, "credential sync: own enrolled device or approval chain is unverified")),
		refresh("sync unavailable", RefreshFailureSyncUnavailable, RefreshRemedyOperatorAction, "check `boss account devices ls`",
			gateFailure(RefreshFailureSyncUnavailable, "credential synchronization unavailable")),
		journal("journal corrupt", func(_ *testing.T, f *failureFixture) {
			f.store.stateMu.Lock()
			f.store.state = []byte(`{"seed":"c2hvcnQ="}`)
			f.store.stateMu.Unlock()
		}),
		journal("journal store unreadable", func(_ *testing.T, f *failureFixture) {
			f.store.stateMu.Lock()
			f.store.stateErr = errors.New("load refresh journal: keyring locked")
			f.store.stateMu.Unlock()
		}),
		refresh("exchange outcome unknown", RefreshFailureOutcomeUnknown, RefreshRemedyReauthenticate, "run `boss account reauth shared`", func(_ *testing.T, f *failureFixture) {
			f.broker.client = &http.Client{Transport: brokerErrorTransport{}}
		}),
		refresh("secret already consumed", RefreshFailureSecretConsumed, RefreshRemedyReauthenticate, "run `boss account reauth shared`", func(t *testing.T, f *failureFixture) {
			f.markConsumed(t)
		}),
		refresh("provider rejected", RefreshFailureRejected, RefreshRemedyReauthenticate, "run `boss account reauth shared`", func(_ *testing.T, f *failureFixture) {
			f.providerStatus.Store(http.StatusUnauthorized)
		}),
		refresh("no usable refresh token", RefreshFailureTokenUnavailable, RefreshRemedyReauthenticate, "run `boss account reauth shared`", func(_ *testing.T, f *failureFixture) {
			f.store.setBlob(bytes.ReplaceAll(failureBlob, []byte(failureSentinelSecret), nil))
		}),
		{name: "request invalid", method: http.MethodGet, status: http.StatusBadRequest, class: RefreshFailureRequestInvalid, remedy: RefreshRemedyOperatorAction, hint: "invalid refresh request"},
		{name: "capability invalid", capability: func(*failureFixture) string { return forgedCapability(failureAccount) }, status: http.StatusUnauthorized, class: RefreshFailureCapabilityInvalid, remedy: RefreshRemedyOperatorAction, hint: "invalid refresh capability", logAccount: "claimed_account_id"},
		// No journal means no state for this account: authentication must not
		// treat an unknown account as a daemon-side failure.
		{name: "journal absent", setup: func(_ *testing.T, f *failureFixture) {
			f.store.stateMu.Lock()
			f.store.state = nil
			f.store.stateMu.Unlock()
		}, status: http.StatusUnauthorized, class: RefreshFailureCapabilityInvalid, remedy: RefreshRemedyOperatorAction, hint: "invalid refresh capability", logAccount: "claimed_account_id"},
		// A capability another broker instance minted is rejected before the journal is read.
		{name: "foreign broker with corrupt journal", capability: func(*failureFixture) string { return forgedCapability(failureAccount) }, setup: func(_ *testing.T, f *failureFixture) {
			f.store.stateMu.Lock()
			f.store.state = []byte("not json")
			f.store.stateMu.Unlock()
		}, status: http.StatusUnauthorized, class: RefreshFailureCapabilityInvalid, remedy: RefreshRemedyOperatorAction, hint: "invalid refresh capability", logAccount: "claimed_account_id"},
		{name: "broker stopped", setup: func(_ *testing.T, f *failureFixture) {
			f.broker.requestMu.Lock()
			f.broker.closed = true
			f.broker.requestMu.Unlock()
		}, status: http.StatusServiceUnavailable, class: RefreshFailureBrokerStopped, remedy: RefreshRemedyTransient, hint: "credential refresh broker stopped"},
	}
}

func TestRefreshFailureEveryClassLogsOnceAndNamesTheRemedy_Accounts_AC223401(t *testing.T) {
	for _, tc := range refreshFailureCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newFailureFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			method, capability := http.MethodPost, f.capability
			if tc.method != "" {
				method = tc.method
			}
			if tc.capability != nil {
				capability = tc.capability(f)
			}
			start := f.broker.now()
			f.logs.Reset()
			response := f.serve(method, capability)
			body := response.Body.String()
			logs := f.logs.String()

			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", response.Code, tc.status, body)
			}
			if !strings.Contains(body, tc.hint) || strings.Contains(body, "retry after synchronization or reauthenticate") {
				t.Fatalf("body %q does not carry %q", body, tc.hint)
			}
			if tc.records && !strings.Contains(body, fmt.Sprintf("[%s: %s]", tc.remedy, tc.class)) {
				t.Fatalf("body %q does not name [%s: %s]", body, tc.remedy, tc.class)
			}
			for _, secret := range []string{failureSentinelAccess, failureSentinelSecret, failureSentinelID, capability} {
				if strings.Contains(logs, secret) || strings.Contains(body, secret) {
					t.Fatalf("credential bytes reached the log or body: %q", secret)
				}
			}
			lines := strings.Split(strings.TrimSpace(logs), "\n")
			if len(lines) != 1 {
				t.Fatalf("want exactly one log line, got %d: %q", len(lines), logs)
			}
			t.Logf("SAMPLE_WARN: %s", lines[0])
			var entry map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
				t.Fatal(err)
			}
			if entry["level"] != "warn" || entry["refresh_failure_class"] != string(tc.class) || entry["refresh_remedy"] != string(tc.remedy) {
				t.Fatalf("log entry %v, want warn %s/%s", entry, tc.class, tc.remedy)
			}
			if tc.logAccount != "" && entry[tc.logAccount] != failureAccount {
				t.Fatalf("log entry %v lacks %s=%s", entry, tc.logAccount, failureAccount)
			}
			recorded, ok := f.broker.RefreshFailureSince(failureAccount, start)
			if want := tc.records; ok != want || (want && recorded != tc.class) {
				t.Fatalf("RefreshFailureSince = %q, %v; want recorded=%v class %q", recorded, ok, want, tc.class)
			}
		})
	}
}

func TestRefreshFailureSinceHonoursSinceAndClearsOnSuccess(t *testing.T) {
	f := newFailureFixture(t)
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f.broker.now = func() time.Time { return clock }
	f.gate.receiveErr = NewRefreshFailure(RefreshFailureSyncUnavailable, errors.New("credential synchronization unavailable"))
	if response := f.serve(http.MethodPost, f.capability); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
	if class, ok := f.broker.RefreshFailureSince(failureAccount, clock); !ok || class != RefreshFailureSyncUnavailable {
		t.Fatalf("at since: %q, %v", class, ok)
	}
	if _, ok := f.broker.RefreshFailureSince(failureAccount, clock.Add(time.Nanosecond)); ok {
		t.Fatal("a failure recorded before since was reported")
	}
	if _, ok := f.broker.RefreshFailureSince("other", clock); ok {
		t.Fatal("a failure leaked to another account")
	}
	f.gate.receiveErr = nil
	if response := f.serve(http.MethodPost, f.capability); response.Code != http.StatusOK {
		t.Fatalf("recovery status = %d (%s)", response.Code, response.Body.String())
	}
	if _, ok := f.broker.RefreshFailureSince(failureAccount, clock.Add(-time.Hour)); ok {
		t.Fatal("a successful refresh did not clear the recorded failure")
	}
	var nilBroker *RefreshBroker
	if _, ok := nilBroker.RefreshFailureSince(failureAccount, clock); ok {
		t.Fatal("nil broker reported a failure")
	}
}

func TestClassifyRefreshErrorPrefersTheGateClassAndDefaultsToUnclassified(t *testing.T) {
	wrapped := fmt.Errorf("gate: %w", NewRefreshFailure(RefreshFailureDeviceNotEnrolled, context.DeadlineExceeded))
	if got := ClassifyRefreshError(wrapped); got != RefreshFailureDeviceNotEnrolled {
		t.Fatalf("gate class lost: %q", got)
	}
	if got := ClassifyRefreshError(errors.New("something new")); got != RefreshFailureUnclassified || got.Remedy() != RefreshRemedyTransient {
		t.Fatalf("unrecognised error = %q", got)
	}
	if NewRefreshFailure(RefreshFailureRejected, nil) != nil {
		t.Fatal("a nil error gained a class")
	}
	if RefreshFailureClass("future_class").Remedy() != RefreshRemedyTransient {
		t.Fatal("an unknown class must be transient")
	}
}
