package agenterr

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempFixturesDir overrides fixturesBaseDir to a not-yet-created
// "cap-fixtures" directory under a fresh t.TempDir(), mirroring the real
// appstate.Path("cap-fixtures") contract: the parent exists but the
// fixtures directory itself does not, so CaptureBanner's os.MkdirAll must
// create it (and its 0700 mode) itself. It returns the outer temp dir, which
// every captured path is expected to fall under.
func withTempFixturesDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	fixtures := filepath.Join(parent, "cap-fixtures")
	orig := fixturesBaseDir
	fixturesBaseDir = func() (string, error) { return fixtures, nil }
	t.Cleanup(func() { fixturesBaseDir = orig })
	return parent
}

func TestCaptureBannerRedactsAndWrites(t *testing.T) {
	base := withTempFixturesDir(t)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no resolvable home directory in this environment")
	}

	secrets := []string{
		"ghp_abcdefghijklmnopqrstuvwxyz1234",
		"sk-ant-api03-abcdefabcdefabcdef1234",
		"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dGhpc2lzYXRlc3Rzand0c2lnbmF0dXJl",
		"API_KEY=hunter2",
		"OPENAI_KEY=sk-proj-abcdef1234567890",
		"someone@example.com",
		filepath.Join(home, "secret-project"),
	}
	snapshot := []byte("usage limit reached\n" + strings.Join(secrets, "\n") + "\ndone")

	path, err := CaptureBanner(KindUsageExhausted, snapshot)
	if err != nil {
		t.Fatalf("CaptureBanner() error = %v", err)
	}

	if !strings.HasPrefix(path, base) {
		t.Fatalf("CaptureBanner() path = %q, want prefix %q", path, base)
	}

	fixturesDir := filepath.Dir(path)
	dirInfo, err := os.Stat(fixturesDir)
	if err != nil {
		t.Fatalf("stat fixtures dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("fixtures dir mode = %o, want 0700", perm)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat captured file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("captured file mode = %o, want 0600", perm)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured file: %v", err)
	}

	for _, secret := range secrets {
		if bytes.Contains(written, []byte(secret)) {
			t.Errorf("captured file contains unredacted secret %q", secret)
		}
	}
	if !bytes.Contains(written, []byte(redactedSentinel)) {
		t.Errorf("captured file does not contain %q sentinel", redactedSentinel)
	}

	base2 := filepath.Base(path)
	if !strings.Contains(base2, "usage_exhausted") {
		t.Errorf("captured filename %q does not contain kind %q", base2, "usage_exhausted")
	}
	if !strings.HasSuffix(base2, ".txt") {
		t.Errorf("captured filename %q does not end in .txt", base2)
	}
}

func TestCaptureBannerCapsSnapshotSize(t *testing.T) {
	withTempFixturesDir(t)

	// Build an oversized snapshot: a distinguishable head we expect to be
	// dropped entirely, followed by a tail *larger than the cap* so the
	// retained bytes are guaranteed to be pure tail (last ~8KB kept).
	head := bytes.Repeat([]byte("H"), 20*1024)
	tail := bytes.Repeat([]byte("T"), 9*1024)
	snapshot := append(head, tail...)

	path, err := CaptureBanner(KindTransientProvider, snapshot)
	if err != nil {
		t.Fatalf("CaptureBanner() error = %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured file: %v", err)
	}

	const capBytes = 8 * 1024
	if len(written) > capBytes {
		t.Fatalf("captured file size = %d bytes, want <= %d", len(written), capBytes)
	}
	if bytes.Contains(written, []byte("H")) {
		t.Errorf("captured file retained head bytes that should have been dropped by the size cap")
	}
	if !bytes.Contains(written, []byte("T")) {
		t.Errorf("captured file dropped tail bytes that should have been kept by the size cap")
	}
}

// TestCaptureBannerRedactsBeforeCapping guards the fail-closed invariant when a
// secret straddles the size-cap cut boundary. The snapshot is built so the
// trailing captureSizeCap window begins partway through a provider key: if
// CaptureBanner capped before redacting, the key's `\bsk-` prefix would be
// dropped and its unmatched suffix ("material9999") written to disk raw.
// Redacting before capping must leave neither the suffix nor any raw key bytes.
func TestCaptureBannerRedactsBeforeCapping(t *testing.T) {
	withTempFixturesDir(t)

	const capBytes = 8 * 1024
	secret := "sk-ant-api03-supersecretkeymaterial9999"
	suffixThatWouldLeak := "material9999"
	// Size the tail filler so the cut lands inside the secret (after "sk-…").
	head := bytes.Repeat([]byte("A"), 100)
	tail := bytes.Repeat([]byte("B"), capBytes-12)
	snapshot := append(head, []byte(" "+secret)...)
	snapshot = append(snapshot, tail...)
	if len(snapshot) <= capBytes {
		t.Fatalf("test setup: snapshot %d bytes must exceed cap %d", len(snapshot), capBytes)
	}

	path, err := CaptureBanner(KindUsageExhausted, snapshot)
	if err != nil {
		t.Fatalf("CaptureBanner() error = %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured file: %v", err)
	}

	if bytes.Contains(written, []byte(suffixThatWouldLeak)) {
		t.Errorf("captured file leaked straddling-secret suffix %q (cap-before-redact regression)", suffixThatWouldLeak)
	}
	if bytes.Contains(written, []byte(secret)) {
		t.Errorf("captured file contains the raw provider key")
	}
	if !bytes.Contains(written, []byte(redactedSentinel)) {
		t.Errorf("captured file has no %q sentinel; straddling secret was not redacted", redactedSentinel)
	}
	if len(written) > capBytes {
		t.Errorf("captured file size = %d bytes, want <= %d", len(written), capBytes)
	}
}

func TestCaptureBannerFixturesDirError(t *testing.T) {
	orig := fixturesBaseDir
	wantErr := os.ErrPermission
	fixturesBaseDir = func() (string, error) { return "", wantErr }
	t.Cleanup(func() { fixturesBaseDir = orig })

	if _, err := CaptureBanner(KindUsageExhausted, []byte("x")); err == nil {
		t.Fatalf("CaptureBanner() error = nil, want non-nil when fixtures dir is unresolvable")
	}
}

func TestRedact(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no resolvable home directory in this environment")
	}

	cases := []struct {
		name        string
		input       string
		mustNotHave []string
		mustHave    []string
	}{
		{
			name:        "github token",
			input:       "token=ghp_abcdefghijklmnopqrstuvwxyz1234",
			mustNotHave: []string{"ghp_abcdefghijklmnopqrstuvwxyz1234"},
			mustHave:    []string{redactedSentinel},
		},
		{
			name:        "jwt",
			input:       "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dGhpc2lzYXRlc3Rzand0c2lnbmF0dXJl",
			mustNotHave: []string{"eyJzdWIiOiIxMjM0NTY3ODkwIn0"},
			mustHave:    []string{"Authorization:", redactedSentinel},
		},
		{
			name:        "env secret",
			input:       "API_KEY=hunter2",
			mustNotHave: []string{"hunter2"},
			mustHave:    []string{"API_KEY=", redactedSentinel},
		},
		{
			name:        "bare provider api key in prose",
			input:       "your key sk-ant-api03-abcdefabcdefabcdef1234 is invalid",
			mustNotHave: []string{"sk-ant-api03-abcdefabcdefabcdef1234"},
			mustHave:    []string{redactedSentinel},
		},
		{
			name:        "multiline pem private key body is fully redacted",
			input:       "PRIVATE_KEY=\"-----BEGIN PRIVATE KEY-----\nMIIBVwIBADANBgkqhkiG9w0BAQEFAASCAT\nkwggE1AgEAAkEAqZ7\n-----END PRIVATE KEY-----\"",
			mustNotHave: []string{"MIIBVwIBADANBgkqhkiG9w0BAQEFAASCAT", "kwggE1AgEAAkEAqZ7"},
			mustHave:    []string{redactedSentinel},
		},
		{
			name:        "labelled rsa pem private key body is fully redacted",
			input:       "-----BEGIN RSA PRIVATE KEY-----\nMIICXAIBAAKBgQDR\nsecretbodyline2\n-----END RSA PRIVATE KEY-----",
			mustNotHave: []string{"MIICXAIBAAKBgQDR", "secretbodyline2"},
			mustHave:    []string{redactedSentinel},
		},
		{
			name:        "non-api KEY= env assignment",
			input:       "OPENAI_KEY=sk-proj-abcdef1234567890",
			mustNotHave: []string{"sk-proj-abcdef1234567890"},
			mustHave:    []string{"OPENAI_KEY=", redactedSentinel},
		},
		{
			name:        "multi-segment env token name",
			input:       "AWS_SESSION_TOKEN=AQoDYXdzEJr1234567890example",
			mustNotHave: []string{"AQoDYXdzEJr1234567890example"},
			mustHave:    []string{"AWS_SESSION_TOKEN=", redactedSentinel},
		},
		{
			name:        "multi-segment env secret name",
			input:       "MY_APP_SECRET=s3cr3t-value-here",
			mustNotHave: []string{"s3cr3t-value-here"},
			mustHave:    []string{"MY_APP_SECRET=", redactedSentinel},
		},
		{
			name:        "double-quoted secret value with spaces",
			input:       `PASSWORD="correct horse battery staple"`,
			mustNotHave: []string{"correct", "horse", "battery", "staple"},
			mustHave:    []string{"PASSWORD=", redactedSentinel},
		},
		{
			name:        "single-quoted secret value with spaces",
			input:       "TOKEN='abc def ghi'",
			mustNotHave: []string{"abc", "def", "ghi"},
			mustHave:    []string{"TOKEN=", redactedSentinel},
		},
		{
			name:        "unterminated quoted secret value",
			input:       `SECRET="abc def ghi`,
			mustNotHave: []string{"abc", "def", "ghi"},
			mustHave:    []string{"SECRET=", redactedSentinel},
		},
		{
			name:        "json-style quoted key secret",
			input:       `{"access_token":"ya29.raw-oauth-token"}`,
			mustNotHave: []string{"ya29.raw-oauth-token"},
			mustHave:    []string{`"access_token":`, redactedSentinel},
		},
		{
			name:        "quoted config key with spaced separator",
			input:       `"client_secret" = "s3cr3t value here"`,
			mustNotHave: []string{"s3cr3t", "value", "here"},
			mustHave:    []string{"client_secret", redactedSentinel},
		},
		{
			name:        "json secret with escaped quote consumes through real closing quote",
			input:       `{"password":"abc\"def ghi"}`,
			mustNotHave: []string{"abc", "def", "ghi"},
			mustHave:    []string{`{"password":`, redactedSentinel},
		},
		{
			name:        "benign word ending in key is not redacted",
			input:       "monkey=banana",
			mustNotHave: []string{redactedSentinel},
			mustHave:    []string{"monkey=banana"},
		},
		{
			name:        "benign hyphenated prose containing sk- is not redacted",
			input:       "task-oriented-development-branch and risk-assessment-framework",
			mustNotHave: []string{redactedSentinel},
			mustHave:    []string{"task-oriented-development-branch", "risk-assessment-framework"},
		},
		{
			name:        "email",
			input:       "contact someone@example.com for help",
			mustNotHave: []string{"someone@example.com"},
			mustHave:    []string{redactedSentinel},
		},
		{
			name:        "authorization basic scheme",
			input:       "Authorization: Basic dXNlcjpwYXNzd29yZA==",
			mustNotHave: []string{"dXNlcjpwYXNzd29yZA=="},
			mustHave:    []string{"Authorization: Basic ", redactedSentinel},
		},
		{
			name:        "authorization token scheme",
			input:       "Authorization: Token abc123def456ghi789",
			mustNotHave: []string{"abc123def456ghi789"},
			mustHave:    []string{"Authorization: Token ", redactedSentinel},
		},
		{
			name:        "authorization digest multi-parameter scheme",
			input:       `Authorization: Digest username="alice", response="deadbeefcafe1234"`,
			mustNotHave: []string{"alice", "deadbeefcafe1234"},
			mustHave:    []string{"Authorization: Digest ", redactedSentinel},
		},
		{
			name:        "authorization aws4-hmac-sha256 multi-parameter scheme",
			input:       "Authorization: AWS4-HMAC-SHA256 Credential=AKIA/x, Signature=abcdef123456",
			mustNotHave: []string{"Signature=abcdef123456", "Credential=AKIA"},
			mustHave:    []string{"Authorization: AWS4-HMAC-SHA256 ", redactedSentinel},
		},
		{
			name:        "bare bearer token without authorization prefix",
			input:       "using Bearer abcdefghijklmnopqrstuvwxyz1234 now",
			mustNotHave: []string{"abcdefghijklmnopqrstuvwxyz1234"},
			mustHave:    []string{"Bearer ", redactedSentinel},
		},
		{
			name:        "home path",
			input:       home + "/secret-project/config.yaml",
			mustNotHave: []string{home},
			mustHave:    []string{"~/secret-project/config.yaml"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(Redact([]byte(tc.input)))
			for _, s := range tc.mustNotHave {
				if strings.Contains(got, s) {
					t.Errorf("Redact(%q) = %q, must not contain %q", tc.input, got, s)
				}
			}
			for _, s := range tc.mustHave {
				if !strings.Contains(got, s) {
					t.Errorf("Redact(%q) = %q, must contain %q", tc.input, got, s)
				}
			}
		})
	}
}
