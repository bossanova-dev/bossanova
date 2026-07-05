package proofenvkeyring

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/proofenv"
)

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

// newResolver builds a Resolver over the given store (or open error) with a
// logger writing into buf so tests can assert on log contents.
func newResolver(store secretStore, openErr error, buf *bytes.Buffer) *Resolver {
	return &Resolver{
		open: func() (secretStore, error) {
			if openErr != nil {
				return nil, openErr
			}
			return store, nil
		},
		logger: zerolog.New(buf),
	}
}

func TestResolve_AllSecretsPresent(t *testing.T) {
	var buf bytes.Buffer
	store := fakeStore{data: map[string][]byte{
		ItemAnthropicAPIKey:    []byte("sk-ant-SECRET-anthropic"),
		ItemCloudflareAPIToken: []byte("cf-SECRET-token"),
	}}
	env := newResolver(store, nil, &buf).Resolve()

	want := map[string]string{
		proofenv.EnvAnthropicAPIKey:     "sk-ant-SECRET-anthropic",
		proofenv.EnvCloudflareAPIToken:  "cf-SECRET-token",
		proofenv.EnvR2Bucket:            constR2Bucket,
		proofenv.EnvPublicBaseURL:       constPublicBaseURL,
		proofenv.EnvCloudflareAccountID: constCloudflareAccountID,
	}
	if len(env) != len(want) {
		t.Fatalf("env size = %d, want %d: %v", len(env), len(want), env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, env[k], v)
		}
	}
}

func TestResolve_ConstantsAlwaysPresent(t *testing.T) {
	var buf bytes.Buffer
	// Both secrets fail to resolve; the three constants must still be present.
	store := fakeStore{failKeys: map[string]bool{
		ItemAnthropicAPIKey:    true,
		ItemCloudflareAPIToken: true,
	}}
	env := newResolver(store, nil, &buf).Resolve()

	for _, k := range []string{proofenv.EnvR2Bucket, proofenv.EnvPublicBaseURL, proofenv.EnvCloudflareAccountID} {
		if env[k] == "" {
			t.Errorf("constant %q missing from env: %v", k, env)
		}
	}
	if _, ok := env[proofenv.EnvAnthropicAPIKey]; ok {
		t.Errorf("failed secret %q should be omitted", proofenv.EnvAnthropicAPIKey)
	}
	if _, ok := env[proofenv.EnvCloudflareAPIToken]; ok {
		t.Errorf("failed secret %q should be omitted", proofenv.EnvCloudflareAPIToken)
	}
}

func TestResolve_PartialSecrets(t *testing.T) {
	cases := []struct {
		name       string
		present    string // env key expected present
		missingKey string // item key that fails
	}{
		{"anthropic-only", proofenv.EnvAnthropicAPIKey, ItemCloudflareAPIToken},
		{"cloudflare-only", proofenv.EnvCloudflareAPIToken, ItemAnthropicAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			store := fakeStore{
				data: map[string][]byte{
					ItemAnthropicAPIKey:    []byte("anthropic-val"),
					ItemCloudflareAPIToken: []byte("cf-val"),
				},
				failKeys: map[string]bool{tc.missingKey: true},
			}
			env := newResolver(store, nil, &buf).Resolve()
			if env[tc.present] == "" {
				t.Errorf("expected %q present, env=%v", tc.present, env)
			}
		})
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
	if env[proofenv.EnvCloudflareAPIToken] != "cf-val" {
		t.Errorf("cloudflare token should resolve, got %q", env[proofenv.EnvCloudflareAPIToken])
	}
}

func TestResolve_KeyringOpenFailure(t *testing.T) {
	var buf bytes.Buffer
	env := newResolver(nil, errors.New("keyring locked"), &buf).Resolve()
	// Only constants survive; both secrets omitted.
	if len(env) != 3 {
		t.Fatalf("expected only 3 constants, got %v", env)
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
	env := newResolver(store, nil, &buf).Resolve()

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
