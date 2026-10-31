package proofenvkeyring

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/config"

	"github.com/recurser/bossd/internal/adapter/proofenv"
	"github.com/recurser/bossd/internal/domain"
)

// completeDestination is a fixture destination; no real bucket or account.
var completeDestination = config.ProofUploadConfig{
	AccountID:     "fixture-account",
	Bucket:        "fixture-bucket",
	PublicBaseURL: "https://proof.example",
}

// fakeStore is an in-memory secretStore for tests: it never touches a real
// keyring. Items present in data resolve; keys in failKeys return an error.
type fakeStore struct {
	data     map[string][]byte
	failKeys map[string]bool
}

func (f fakeStore) Get(key string) (keyring.Item, error) {
	if f.failKeys[key] {
		return keyring.Item{}, errors.New("keyring: item not found")
	}
	v, ok := f.data[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return keyring.Item{Key: key, Data: v}, nil
}

// newResolver builds a Resolver over the given store (or open error) and a
// complete fixture destination, with a logger writing into buf so tests can
// assert on log contents.
func newResolver(store secretStore, openErr error, buf *bytes.Buffer) *Resolver {
	return &Resolver{
		open: func() (secretStore, error) {
			if openErr != nil {
				return nil, openErr
			}
			return store, nil
		},
		settings: func() (config.ProofUploadConfig, error) { return completeDestination, nil },
		logger:   zerolog.New(buf),
	}
}

func TestResolve_AllSecretsPresent(t *testing.T) {
	var buf bytes.Buffer
	store := fakeStore{data: map[string][]byte{
		ItemAnthropicAPIKey:    []byte("sk-ant-SECRET-anthropic"),
		ItemCloudflareAPIToken: []byte("cf-SECRET-token"),
	}}
	env := newResolver(store, nil, &buf).Resolve()

	want := map[string]string{proofenv.EnvAnthropicAPIKey: "sk-ant-SECRET-anthropic"}
	if len(env) != len(want) {
		t.Fatalf("env size = %d, want %d: %v", len(env), len(want), env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, env[k], v)
		}
	}
}

// Provisioning both credentials still emits only the spawn allowlist.
func TestResolve_EmitsExactlySpawnKeys(t *testing.T) {
	var buf bytes.Buffer
	store := fakeStore{data: map[string][]byte{
		ItemAnthropicAPIKey:    []byte("anthropic-val"),
		ItemCloudflareAPIToken: []byte("cf-val"),
	}}
	emitted := slices.Sorted(maps.Keys(newResolver(store, nil, &buf).Resolve()))
	pinned := slices.Sorted(slices.Values(proofenv.SpawnKeys()))
	if !slices.Equal(emitted, pinned) {
		t.Fatalf("resolver emits %v, proofenv.SpawnKeys() names %v; keep the two in step", emitted, pinned)
	}
}

func TestResolve_EmptySecretOmitted(t *testing.T) {
	var buf bytes.Buffer
	store := fakeStore{data: map[string][]byte{
		ItemAnthropicAPIKey:    {}, // present but empty → omit
		ItemCloudflareAPIToken: []byte("cf-val"),
	}}
	env := newResolver(store, nil, &buf).Resolve()
	if _, ok := env[proofenv.EnvAnthropicAPIKey]; ok {
		t.Errorf("empty secret should be omitted, got %q", env[proofenv.EnvAnthropicAPIKey])
	}
	if _, ok := env[proofenv.EnvCloudflareAPIToken]; ok {
		t.Error("upload token entered spawn overlay")
	}
}

func TestResolve_KeyringOpenFailure(t *testing.T) {
	var buf bytes.Buffer
	env := newResolver(nil, errors.New("keyring locked"), &buf).Resolve()
	// No spawn credential survives an unavailable keyring.
	if len(env) != 0 {
		t.Fatalf("expected empty overlay, got %v", env)
	}
	for _, k := range []string{proofenv.EnvAnthropicAPIKey, proofenv.EnvCloudflareAPIToken} {
		if _, ok := env[k]; ok {
			t.Errorf("%q should be omitted when keyring open fails", k)
		}
	}
}

// TestResolve_NeverLogsSecretValues asserts secret VALUES never reach the log
// stream — only key names. This is the core D11 confidentiality gate.
func TestResolve_NeverLogsSecretValues(t *testing.T) {
	const anthropicSecret = "sk-ant-TOPSECRET-should-never-be-logged"
	const cloudflareSecret = "cf-TOPSECRET-should-never-be-logged"

	var buf bytes.Buffer
	// One secret resolves, one fails — exercise both the success and the
	// failure log path.
	store := fakeStore{
		data:     map[string][]byte{ItemAnthropicAPIKey: []byte(anthropicSecret)},
		failKeys: map[string]bool{ItemCloudflareAPIToken: true},
	}
	r := newResolver(store, nil, &buf)
	env := r.Resolve()
	_, _ = r.UploadTarget()

	if env[proofenv.EnvAnthropicAPIKey] != anthropicSecret {
		t.Fatalf("resolver dropped a valid secret")
	}
	logs := buf.String()
	if strings.Contains(logs, anthropicSecret) {
		t.Errorf("anthropic secret value leaked into logs: %q", logs)
	}
	if strings.Contains(logs, cloudflareSecret) {
		t.Errorf("cloudflare secret value leaked into logs: %q", logs)
	}
	// The failure path must name the key so a human can fix it.
	if !strings.Contains(logs, ItemCloudflareAPIToken) {
		t.Errorf("expected failed key name %q in logs, got %q", ItemCloudflareAPIToken, logs)
	}
}

func TestResolve_FreshEachCall(t *testing.T) {
	var buf bytes.Buffer
	store := fakeStore{data: map[string][]byte{}}
	r := newResolver(store, nil, &buf)

	if _, ok := r.Resolve()[proofenv.EnvAnthropicAPIKey]; ok {
		t.Fatal("secret unexpectedly present before provisioning")
	}
	// Simulate re-provisioning between calls: the resolver must pick it up
	// without reconstruction (no permanent caching).
	store.data[ItemAnthropicAPIKey] = []byte("freshly-provisioned")
	if got := r.Resolve()[proofenv.EnvAnthropicAPIKey]; got != "freshly-provisioned" {
		t.Errorf("re-provisioned secret not picked up, got %q", got)
	}
}

func TestUploadTargetFreshAndMissing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		token   []byte
		openErr error
		want    bool
	}{
		{name: "provisioned", token: []byte("upload-secret"), want: true},
		{name: "absent"}, {name: "empty", token: []byte{}},
		{name: "unavailable", openErr: errors.New("must-not-log-this-sensitive-backend-error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			store := fakeStore{data: map[string][]byte{}}
			if tc.token != nil {
				store.data[ItemCloudflareAPIToken] = tc.token
			}
			r := newResolver(store, tc.openErr, &buf)
			target, ok := r.UploadTarget()
			if ok != tc.want {
				t.Fatalf("ok=%v, want %v", ok, tc.want)
			}
			want := domain.EvidenceUploadTarget{AccountID: completeDestination.AccountID, Bucket: completeDestination.Bucket, PublicBaseURL: completeDestination.PublicBaseURL, Token: string(tc.token)}
			if ok && target != want {
				t.Errorf("target = %+v, want %+v", target, want)
			}
			if strings.Contains(buf.String(), "upload-secret") || strings.Contains(buf.String(), "must-not-log-this") {
				t.Fatal("secret logged")
			}
			if tc.want {
				store.data[ItemCloudflareAPIToken] = []byte("replacement")
				target, ok = r.UploadTarget()
				if !ok || target.Token != "replacement" {
					t.Fatal("upload credential cached")
				}
			}
		})
	}
}

func TestUploadTargetTakesDestinationFromSettings_Sessions_AC208101(t *testing.T) {
	var buf bytes.Buffer
	store := fakeStore{data: map[string][]byte{ItemCloudflareAPIToken: []byte("cf-SECRET-token")}}
	r := newResolver(store, nil, &buf)
	dest := config.ProofUploadConfig{AccountID: "acct-1", Bucket: "bucket-1", PublicBaseURL: "https://one.example/base", PagesEnabled: true}
	r.settings = func() (config.ProofUploadConfig, error) { return dest, nil }
	target, ok := r.UploadTarget()
	want := domain.EvidenceUploadTarget{AccountID: "acct-1", Bucket: "bucket-1", PublicBaseURL: "https://one.example/base", PagesEnabled: true, Token: "cf-SECRET-token"}
	if !ok || target != want {
		t.Fatalf("target = %+v ok=%v, want %+v", target, ok, want)
	}
	// Settings are re-read on every call: an edit takes effect without a restart.
	dest = config.ProofUploadConfig{AccountID: "acct-2", Bucket: "bucket-2", PublicBaseURL: "https://two.example"}
	target, ok = r.UploadTarget()
	want = domain.EvidenceUploadTarget{AccountID: "acct-2", Bucket: "bucket-2", PublicBaseURL: "https://two.example", Token: "cf-SECRET-token"}
	if !ok || target != want {
		t.Fatalf("settings were cached: target = %+v ok=%v", target, ok)
	}
}

func TestUploadTargetIncompleteDestinationIsNotConfigured_Sessions_AC208101(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.ProofUploadConfig)
		err    error
	}{
		{name: "absent", mutate: func(c *config.ProofUploadConfig) { *c = config.ProofUploadConfig{} }},
		{name: "no account", mutate: func(c *config.ProofUploadConfig) { c.AccountID = "" }},
		{name: "no bucket", mutate: func(c *config.ProofUploadConfig) { c.Bucket = "" }},
		{name: "no base url", mutate: func(c *config.ProofUploadConfig) { c.PublicBaseURL = "" }},
		{name: "http base url", mutate: func(c *config.ProofUploadConfig) { c.PublicBaseURL = "http://proof.example" }},
		{name: "relative base url", mutate: func(c *config.ProofUploadConfig) { c.PublicBaseURL = "proof.example" }},
		{name: "hostless base url", mutate: func(c *config.ProofUploadConfig) { c.PublicBaseURL = "https:///path" }},
		{name: "userinfo base url", mutate: func(c *config.ProofUploadConfig) { c.PublicBaseURL = "https://user:pw@proof.example" }},
		{name: "settings unreadable", err: errors.New("parse settings: must-not-log-this")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			store := fakeStore{data: map[string][]byte{ItemCloudflareAPIToken: []byte("cf-SECRET-token")}}
			r := newResolver(store, nil, &buf)
			dest := completeDestination
			dest.PagesEnabled = true
			if tc.mutate != nil {
				tc.mutate(&dest)
			}
			r.settings = func() (config.ProofUploadConfig, error) { return dest, tc.err }
			if target, ok := r.UploadTarget(); ok || target != (domain.EvidenceUploadTarget{}) {
				t.Fatalf("incomplete destination configured: %+v", target)
			}
			logs := buf.String()
			for _, leaked := range []string{"cf-SECRET-token", "must-not-log-this", "pw@", "fixture-account", "fixture-bucket"} {
				if strings.Contains(logs, leaked) {
					t.Fatalf("log carried a value %q: %s", leaked, logs)
				}
			}
		})
	}
}
