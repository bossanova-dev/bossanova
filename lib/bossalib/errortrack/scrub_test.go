package errortrack

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
)

func TestScrub_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "github pat",
			in:   "token ghp_AbCdEf0123456789AbCdEf0123456789AbCd is leaked",
			want: "token [REDACTED] is leaked",
		},
		{
			name: "github ghs",
			in:   "Authorization: token ghs_AbCdEf0123456789AbCdEf0123456789AbCd",
			want: "Authorization: token [REDACTED]",
		},
		{
			name: "jwt",
			in:   "bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dGhpc2lzbm90YXNpZ25hdHVyZWJ1dGl0c2xvbmdlbm91Z2g",
			want: "bearer [REDACTED]",
		},
		{
			name: "email",
			in:   "user person@example.invalid hit an error",
			want: "user [REDACTED] hit an error",
		},
		{
			name: "github fine grained pat",
			in:   "token github_pat_11EXAMPLE0abcdefghijklmnopqrstuvwxyz1234567890ABCDEFGHIJKLMNOPQRSTUVWXYZ leaked",
			want: "token [REDACTED] leaked",
		},
		{
			name: "authorization bearer",
			in:   "Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456",
			want: "Authorization: Bearer [REDACTED]",
		},
		{
			name: "bearer opaque",
			in:   "Bearer abcdefghijklmnopqrstuvwxyz123456 failed",
			want: "Bearer [REDACTED] failed",
		},
		{
			name: "authorization basic",
			in:   "Authorization: Basic dXNlcjpwYXNzd29yZA== failed",
			want: "Authorization: Basic [REDACTED] failed",
		},
		{
			name: "authorization token lowercase",
			in:   "authorization: token abc123secret extra",
			want: "authorization: token [REDACTED] extra",
		},
		{
			name: "authorization digest with spaced commas",
			in:   `Authorization: Digest username="u", realm="r", nonce="n", response="r1"`,
			want: "Authorization: Digest [REDACTED]",
		},
		{
			name: "authorization digest preserves later lines",
			in:   "Authorization: Digest username=\"u\", realm=\"r\", response=\"r1\"\nnext line",
			want: "Authorization: Digest [REDACTED]\nnext line",
		},
		{
			name: "authorization mac with spaced commas",
			in:   `Authorization: MAC id="h480djs93hd8", ts="1336363200", nonce="dj83hs9s", mac="bhCQXTVyfj5cmA9uKkPFx1zeOXM="`,
			want: "Authorization: MAC [REDACTED]",
		},
		{
			name: "authorization aws4 with spaced commas",
			in:   `Authorization: AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20211231/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc123`,
			want: "Authorization: AWS4-HMAC-SHA256 [REDACTED]",
		},
		{
			name: "env secret",
			in:   "API_KEY=sk_live_abcdef123 failed",
			want: "API_KEY=[REDACTED] failed",
		},
		{
			name: "colon secret",
			in:   "password: hunter2 was wrong",
			want: "password: [REDACTED] was wrong",
		},
		{
			name: "innocuous",
			in:   "ordinary error with no secrets",
			want: "ordinary error with no secrets",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scrub(tt.in, ""); got != tt.want {
				t.Fatalf("scrub(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestScrub_HomePathNormalized(t *testing.T) {
	got := scrub("open /Users/example/.bossanova/config", "/Users/example")
	want := "open ~/.bossanova/config"
	if got != want {
		t.Fatalf("scrub home = %q, want %q", got, want)
	}
}

func TestBeforeSend_StripsRequestBodyAndCookies(t *testing.T) {
	event := &sentry.Event{
		Message: "user person@example.invalid failed",
		Request: &sentry.Request{
			Data:        `{"password":"hunter2"}`,
			Cookies:     "session=abc123",
			Headers:     map[string]string{"Authorization": "token ghp_AbCdEf0123456789AbCdEf0123456789AbCd"},
			QueryString: "email=person@example.invalid&token=ghp_AbCdEf0123456789AbCdEf0123456789AbCd",
		},
	}

	got := beforeSend("test")(event, nil)
	if got.Request.Data != "" {
		t.Fatalf("request data = %q, want empty", got.Request.Data)
	}
	if got.Request.Cookies != "" {
		t.Fatalf("request cookies = %q, want empty", got.Request.Cookies)
	}
	if len(got.Request.Headers) != 0 {
		t.Fatalf("request headers = %#v, want empty", got.Request.Headers)
	}
	if strings.Contains(got.Request.QueryString, "person@example.invalid") || strings.Contains(got.Request.QueryString, "ghp_") {
		t.Fatalf("request query string not scrubbed: %q", got.Request.QueryString)
	}
}

func TestBeforeSend_TruncatesGiantMessages(t *testing.T) {
	event := &sentry.Event{Message: strings.Repeat("a", 5001)}

	got := beforeSend("test")(event, nil)
	if len(got.Message) > 2100 {
		t.Fatalf("message length = %d, want capped around 2000", len(got.Message))
	}
	if !strings.HasSuffix(got.Message, "[truncated]") {
		t.Fatalf("message suffix = %q, want [truncated]", got.Message[len(got.Message)-20:])
	}
}

func TestBeforeSend_ScrubsStacktraces(t *testing.T) {
	home := testHome(t)
	event := &sentry.Event{
		Exception: []sentry.Exception{{
			Type:  "token github_pat_11EXAMPLE0abcdefghijklmnopqrstuvwxyz1234567890ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			Value: "Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456",
			Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{
				Filename:    home + "/project/main.go",
				AbsPath:     home + "/project/main.go",
				Function:    "run ghp_AbCdEf0123456789AbCdEf0123456789AbCd",
				Module:      "person@example.invalid/module",
				Package:     "pkg github_pat_11EXAMPLE0abcdefghijklmnopqrstuvwxyz1234567890ABCDEFGHIJKLMNOPQRSTUVWXYZ",
				ContextLine: "Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456",
				PreContext:  []string{"email person@example.invalid"},
				PostContext: []string{"token ghp_AbCdEf0123456789AbCdEf0123456789AbCd"},
				Vars:        map[string]interface{}{"secret": "ghp_AbCdEf0123456789AbCdEf0123456789AbCd"},
			}}},
		}},
		Threads: []sentry.Thread{{
			Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{
				AbsPath:     home + "/thread.go",
				ContextLine: "person@example.invalid",
			}}},
		}},
	}

	got := beforeSend("test")(event, nil)
	frame := got.Exception[0].Stacktrace.Frames[0]
	for _, value := range []string{
		got.Exception[0].Type,
		got.Exception[0].Value,
		frame.Filename,
		frame.AbsPath,
		frame.Function,
		frame.Module,
		frame.Package,
		frame.ContextLine,
		strings.Join(frame.PreContext, " "),
		strings.Join(frame.PostContext, " "),
		got.Threads[0].Stacktrace.Frames[0].AbsPath,
		got.Threads[0].Stacktrace.Frames[0].ContextLine,
	} {
		assertNoPII(t, value)
	}
	if len(frame.Vars) != 0 {
		t.Fatalf("frame vars = %#v, want cleared", frame.Vars)
	}
	assertEventNoPII(t, got)
}

func TestBeforeSend_ClearsUserAndRequestEnvScrubsURL(t *testing.T) {
	event := &sentry.Event{
		User: sentry.User{
			ID:    "123",
			Email: "person@example.invalid",
			Name:  "Example User",
		},
		Request: &sentry.Request{
			URL:         "https://example.invalid/path/person@example.invalid?token=github_pat_11EXAMPLE0abcdefghijklmnopqrstuvwxyz1234567890ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			QueryString: "Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456",
			Env:         map[string]string{"API_KEY": "sk_live_example"},
		},
	}

	got := beforeSend("test")(event, nil)
	if !got.User.IsEmpty() {
		t.Fatalf("user = %#v, want empty", got.User)
	}
	if len(got.Request.Env) != 0 {
		t.Fatalf("request env = %#v, want empty", got.Request.Env)
	}
	assertNoPII(t, got.Request.URL)
	assertNoPII(t, got.Request.QueryString)
	assertEventNoPII(t, got)
}

func TestBeforeSend_ScrubsNestedEventData(t *testing.T) {
	home := testHome(t)
	event := &sentry.Event{
		Transaction: "GET /users/person@example.invalid",
		ServerName:  "host-person@example.invalid",
		Fingerprint: []string{
			"github_pat_11EXAMPLE0abcdefghijklmnopqrstuvwxyz1234567890ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		},
		Tags: map[string]string{
			"auth":                   "Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456",
			"person@example.invalid": "tag-key",
		},
		Contexts: map[string]sentry.Context{
			"custom": {
				"nested": map[string]interface{}{
					"email": "person@example.invalid",
					"list": []interface{}{
						"ghp_AbCdEf0123456789AbCdEf0123456789AbCd",
						map[string]interface{}{"path": home + "/project"},
					},
				},
			},
			"person@example.invalid": {
				"key": "context-key",
			},
		},
		Breadcrumbs: []*sentry.Breadcrumb{{
			Message: "github_pat_11EXAMPLE0abcdefghijklmnopqrstuvwxyz1234567890ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			Data: map[string]interface{}{
				"nested": map[string]interface{}{
					"bearer": "Bearer abcdefghijklmnopqrstuvwxyz123456",
				},
				"slice": []interface{}{"person@example.invalid"},
			},
		}},
	}

	got := beforeSend("test")(event, nil)
	for _, value := range []string{
		got.Transaction,
		got.ServerName,
		strings.Join(got.Fingerprint, " "),
		got.Tags["auth"],
		got.Breadcrumbs[0].Message,
		got.Contexts["custom"]["nested"].(map[string]interface{})["email"].(string),
		got.Contexts["custom"]["nested"].(map[string]interface{})["list"].([]interface{})[0].(string),
		got.Contexts["custom"]["nested"].(map[string]interface{})["list"].([]interface{})[1].(map[string]interface{})["path"].(string),
		got.Breadcrumbs[0].Data["nested"].(map[string]interface{})["bearer"].(string),
		got.Breadcrumbs[0].Data["slice"].([]interface{})[0].(string),
	} {
		assertNoPII(t, value)
	}
	assertEventNoPII(t, got)
}

func assertNoPII(t *testing.T, value string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	for _, leaked := range []string{
		"person@example.invalid",
		"github_pat_",
		"ghp_",
		"abcdefghijklmnopqrstuvwxyz123456",
		"/Users/example",
		home,
	} {
		if leaked != "" && strings.Contains(value, leaked) {
			t.Fatalf("%q still contains %q", value, leaked)
		}
	}
}

func assertEventNoPII(t *testing.T, event *sentry.Event) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	assertNoPII(t, string(payload))
}

func testHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("user home unavailable")
	}
	return home
}
