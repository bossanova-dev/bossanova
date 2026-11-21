package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/recurser/bossd/internal/ghusage"
	"github.com/rs/zerolog"
)

func testAPIExecutor(t testing.TB, handler http.HandlerFunc) (*apiExecutor, *httptest.Server) {
	t.Helper()
	s := httptest.NewTLSServer(handler)
	t.Cleanup(s.Close)
	e := newAPIExecutor(apiExecConfig{baseURL: s.URL + "/", transport: s.Client().Transport, userToken: func(context.Context) (string, error) { return "user-secret", nil }, fallback: func(context.Context, ...string) (string, error) { return "", errors.New("unexpected subprocess") }})
	return e, s
}

func TestAPIExecReusesOneConnection(t *testing.T) {
	var connections atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `[]`) }))
	s.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	s.StartTLS()
	defer s.Close()
	e := newAPIExecutor(apiExecConfig{baseURL: s.URL + "/", transport: s.Client().Transport, userToken: func(context.Context) (string, error) { return "token", nil }})
	for range 5 {
		if _, err := e.run(context.Background(), "api", "repos/o/r"); err != nil {
			t.Fatal(err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("opened %d connections, want one", got)
	}
}

func TestAPIExecRequestShape(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		method, uri  string
		body         map[string]any
		installation bool
	}{
		{name: "escaped path", args: []string{"api", "repos/o/r/branches/a%2Fb?x=1"}, method: "GET", uri: "/repos/o/r/branches/a%2Fb?x=1"},
		{name: "fields", args: []string{"api", "repos/o/r", "-f", "raw=true", "--field", "bool=true", "-F", "nil=null", "-F", "n=42", "-F", "text=hello"}, method: "POST", uri: "/repos/o/r", body: map[string]any{"raw": "true", "bool": true, "nil": nil, "n": float64(42), "text": "hello"}, installation: true},
		{name: "GET fields", args: []string{"api", "repos/o/r?x=1", "--method", "GET", "-F", "n=2", "-F", "empty=null"}, method: "GET", uri: "/repos/o/r?empty=&n=2&x=1"},
		{name: "graphql", args: []string{"api", "graphql", "-f", "query=query X{}", "-f", "operationName=X", "-F", "pr=2"}, method: "POST", uri: "/graphql", body: map[string]any{"query": "query X{}", "operationName": "X", "variables": map[string]any{"pr": float64(2)}}},
		{name: "explicit method", args: []string{"api", "-X", "PATCH", "repos/o/r", "-f", "name=x"}, method: "PATCH", uri: "/repos/o/r", body: map[string]any{"name": "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := testAPIExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.RequestURI != tc.uri {
					t.Errorf("request %s %s, want %s %s", r.Method, r.RequestURI, tc.method, tc.uri)
				}
				wantToken := "Bearer user-secret"
				if tc.installation {
					wantToken = "Bearer install-secret"
				}
				if r.Header.Get("Authorization") != wantToken {
					t.Error("wrong Authorization header")
				}
				if r.Header.Get("Accept") != "custom" || r.Header.Get("X-GitHub-Api-Version") != "test-version" {
					t.Error("caller headers did not override defaults")
				}
				if tc.body != nil {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if !reflect.DeepEqual(body, tc.body) {
						t.Errorf("body %#v, want %#v", body, tc.body)
					}
				}
				_, _ = fmt.Fprint(w, `{}`)
			})
			ctx := context.Background()
			if tc.installation {
				ctx = withIdentity(ctx, ghIdentity{key: "installation:1", token: "install-secret"})
			}
			args := append([]string(nil), tc.args...)
			args = append(args, "-H", "Accept: custom", "--header", "X-GitHub-Api-Version: test-version")
			if _, err := e.run(ctx, args...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAPIExecFallsBackToSubprocess(t *testing.T) {
	for _, args := range [][]string{{"pr", "view", "1"}, {"api", "x", "--jq", ".x"}, {"api", "x", "--input", "-"}, {"api", "{owner}/x"}, {"api", "x", "--hostname", "elsewhere"}, {"api", "x", "-F", "x=@file"}, {"api", "x", "--paginate", "--slurp"}, {"api", "graphql", "--paginate"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var called bool
			e := newAPIExecutor(apiExecConfig{fallback: func(_ context.Context, got ...string) (string, error) {
				called = true
				if !reflect.DeepEqual(got, args) {
					t.Errorf("args changed: %v", got)
				}
				return "fallback", nil
			}})
			out, err := e.run(context.Background(), args...)
			if err != nil || out != "fallback" || !called {
				t.Fatalf("fallback=%q,%v,%t", out, err, called)
			}
		})
	}
	t.Run("GH_HOST", func(t *testing.T) {
		t.Setenv("GH_HOST", "enterprise.example")
		e := newAPIExecutor(apiExecConfig{fallback: func(context.Context, ...string) (string, error) { return "fallback", nil }})
		if out, _ := e.run(context.Background(), "api", "x"); out != "fallback" {
			t.Fatal("did not fall back")
		}
	})
	t.Run("unresolved token", func(t *testing.T) {
		e := newAPIExecutor(apiExecConfig{userToken: func(context.Context) (string, error) { return "", errors.New("no token") }, fallback: func(context.Context, ...string) (string, error) { return "fallback", nil }})
		if out, _ := e.run(context.Background(), "api", "x"); out != "fallback" {
			t.Fatal("did not fall back")
		}
	})
}

func TestAPIExecUserToken(t *testing.T) {
	var resolutions atomic.Int32
	var calls atomic.Int32
	e, _ := testAPIExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 3 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"message":"Bad credentials"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{}`)
	})
	e.userToken = func(context.Context) (string, error) { resolutions.Add(1); return "sensitive-token", nil }
	for i := range 5 {
		_, err := e.run(context.Background(), "api", "x")
		if i == 2 {
			if err == nil || strings.Contains(err.Error(), "sensitive-token") {
				t.Fatalf("unsafe or missing error %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if got := resolutions.Load(); got != 2 {
		t.Fatalf("resolved %d times, want twice", got)
	}
}

func TestAPIExecUserTokenRefreshesAfterAccountSwitch(t *testing.T) {
	now := time.Now()
	account := "first-account"
	resolutions := 0
	e := newAPIExecutor(apiExecConfig{userToken: func(context.Context) (string, error) {
		resolutions++
		return account, nil
	}})
	e.now = func() time.Time { return now }
	credential := func(want string) {
		t.Helper()
		got, err := e.userCredential(context.Background())
		if err != nil || got != want {
			t.Fatalf("credential = %q, %v; want %q", got, err, want)
		}
	}
	credential("first-account")
	account = "second-account"
	credential("first-account")
	if resolutions != 1 {
		t.Fatalf("warm credential resolved %d times, want once", resolutions)
	}
	now = now.Add(time.Minute)
	credential("second-account")
	if resolutions != 2 {
		t.Fatalf("account switch resolved %d times, want twice", resolutions)
	}
	account = ""
	now = now.Add(time.Minute)
	credential("")
}

func TestAPIExecUserTokenWaitRespectsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		var resolutions, fallbacks atomic.Int32
		e := newAPIExecutor(apiExecConfig{
			userToken: func(context.Context) (string, error) {
				resolutions.Add(1)
				close(started)
				<-release
				return "shared-token", nil
			},
			transport: apiExecBrokenTransport{},
			fallback: func(context.Context, ...string) (string, error) {
				fallbacks.Add(1)
				return "", context.Canceled
			},
		})
		results := make(chan string, 5)
		read := func() {
			token, err := e.userCredential(context.Background())
			if err != nil {
				results <- "error"
				return
			}
			results <- token
		}
		go read()
		<-started
		for range 4 {
			go read()
		}
		ctx, cancel := context.WithCancel(context.Background())
		canceled := make(chan error, 1)
		go func() {
			_, err := e.run(ctx, "api", "x")
			canceled <- err
		}()
		synctest.Wait()
		cancel()
		select {
		case err := <-canceled:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("waiting call returned %v, want cancellation", err)
			}
		case <-time.After(time.Second):
			t.Error("canceled call waited for the unrelated token lookup")
		}
		close(release)
		for range 5 {
			if token := <-results; token != "shared-token" {
				t.Errorf("concurrent credential = %q, want shared token", token)
			}
		}
		if resolutions.Load() != 1 || fallbacks.Load() != 0 {
			t.Errorf("resolutions=%d fallbacks=%d, want 1 and 0", resolutions.Load(), fallbacks.Load())
		}
	})
}

func TestAPIExecDropsAuthOnCrossHostRedirect(t *testing.T) {
	var auth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = fmt.Fprint(w, "log")
	}))
	defer target.Close()
	e, _ := testAPIExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(target.URL, "127.0.0.1", "localhost", 1), http.StatusFound)
	})
	if out, err := e.run(context.Background(), "api", "logs"); err != nil || out != "log" {
		t.Fatalf("result %q %v", out, err)
	}
	if auth != "" {
		t.Fatal("redirect leaked authorization")
	}
}

func TestAPIExecOutputMatchesGHClassifiers(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		status                         int
		body, stderr                   string
		auth, rate, install, transient bool
	}{
		{name: "success", status: 200, body: `{"ok":true}`},
		{name: "not modified", status: 304, stderr: "gh: HTTP 304"},
		{name: "bad credentials", status: 401, body: `{"message":"Bad credentials"}`, stderr: "gh: Bad credentials (HTTP 401)", auth: true, install: true},
		{name: "rate limit", status: 403, body: `{"message":"API rate limit exceeded"}`, stderr: "gh: API rate limit exceeded (HTTP 403)", rate: true},
		{name: "integration", status: 403, body: `{"message":"Resource not accessible by integration"}`, stderr: "gh: Resource not accessible by integration (HTTP 403)", install: true},
		{name: "not found", status: 404, body: `{"message":"Not Found"}`, stderr: "gh: Not Found (HTTP 404)"},
		{name: "bare bad gateway", status: 502, body: `bad gateway`, stderr: "gh: HTTP 502", transient: true},
		{name: "graphql errors", status: 200, body: `{"errors":[{"message":"one","path":["repo"]},{"message":"two"}]}`, stderr: "gh: one\ntwo"},
		{name: "graphql limit", status: 200, body: `{"errors":[{"message":"API rate limit exceeded"}]}`, stderr: "gh: API rate limit exceeded", rate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := testAPIExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"abc"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			})
			out, err := e.run(context.Background(), "api", "graphql", "-i")
			if tc.stderr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var ge *ghError
				if !errors.As(err, &ge) || ge.stderr != tc.stderr {
					t.Fatalf("error=%v, want %q", err, tc.stderr)
				}
			}
			status, headers, body := parseIncludedResponse(includedOutput(out, err))
			if status != tc.status || headers.Get("ETag") != `"abc"` {
				t.Fatalf("included status=%d headers=%v", status, headers)
			}
			if tc.status != 304 && body != tc.body {
				t.Errorf("body=%q want %q", body, tc.body)
			}
			if isGitHubAuthFailure(err) != tc.auth || isRateLimitRefusal(err) != tc.rate || isInstallationRefusal(err) != tc.install || isGitHubTransient(err) != tc.transient {
				t.Fatalf("classifiers auth=%t rate=%t install=%t transient=%t", isGitHubAuthFailure(err), isRateLimitRefusal(err), isInstallationRefusal(err), isGitHubTransient(err))
			}
		})
	}
	t.Run("pagination", func(t *testing.T) {
		var base string
		e, s := testAPIExecutor(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				_, _ = fmt.Fprint(w, `[2]`)
			} else {
				w.Header().Set("Link", "<"+base+"/items?page=2>; rel=\"next\"")
				_, _ = fmt.Fprint(w, `[1]`)
			}
		})
		base = s.URL
		out, err := e.run(context.Background(), "api", "items", "--paginate")
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodePaginatedPages[int](out, "items")
		if err != nil || !reflect.DeepEqual(got, []int{1, 2}) {
			t.Fatalf("pages=%v %v", got, err)
		}
	})
}

func BenchmarkAPIExecWarm(b *testing.B) {
	e, _ := testAPIExecutor(b, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `[]`) })
	if _, err := e.run(context.Background(), "api", "x"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := e.run(context.Background(), "api", "x"); err != nil {
			b.Fatal(err)
		}
	}
}

func TestHotPathAPIReadsMakeNoSubprocessCall(t *testing.T) {
	for _, installation := range []bool{false, true} {
		t.Run(fmt.Sprintf("installation=%t", installation), func(t *testing.T) {
			var requests atomic.Int32
			var fallbacks atomic.Int32
			var userLookups atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				want := "Bearer user-secret"
				if installation {
					want = "Bearer " + testInstallTok
				}
				if r.Header.Get("Authorization") != want {
					t.Error("wrong identity on HTTP request")
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/graphql":
					_, _ = fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`)
				case strings.Contains(r.URL.Path, "/git/ref/heads/"):
					_, _ = fmt.Fprintf(w, `{"object":{"sha":%q}}`, testSHA)
				case strings.Contains(r.URL.Path, "/commits/"):
					_, _ = fmt.Fprint(w, `{"statuses":[{"context":"boss/verify","state":"success","description":"verified (approved)"}]}`)
				default:
					_, _ = fmt.Fprint(w, `[]`)
				}
			}))
			defer srv.Close()
			ledger := &fakeUsageRecorder{}
			p := New(zerolog.Nop(), withAPIExecConfig(apiExecConfig{baseURL: srv.URL + "/", transport: srv.Client().Transport, userToken: func(context.Context) (string, error) { userLookups.Add(1); return "user-secret", nil }, fallback: func(context.Context, ...string) (string, error) {
				fallbacks.Add(1)
				return "", errors.New("unexpected subprocess")
			}}), WithUsageRecorder(ledger))
			if installation {
				p.SetInstallationTokens(installationSource(nil))
			}
			ctx := ghusage.WithLabel(context.Background(), "test:pooled")
			if reviews, err := p.GetReviewComments(ctx, testOrigin, 7); err != nil || len(reviews) != 0 {
				t.Fatalf("reviews=%v %v", reviews, err)
			}
			if observation, err := p.GetReviewObservation(ctx, testOrigin, 7); err != nil || observation.Total != 0 {
				t.Fatalf("observation=%v %v", observation, err)
			}
			if authors, err := p.BlockingThreadAuthors(ctx, testOrigin, 7, map[string]bool{"bot": true}); err != nil || len(authors) != 0 {
				t.Fatalf("authors=%v %v", authors, err)
			}
			if sha, err := p.GetBranchHeadSHA(ctx, testOrigin, "main"); err != nil || sha != testSHA {
				t.Fatalf("sha=%q %v", sha, err)
			}
			if pass, err := p.VerifyPassedAtHead(ctx, testOrigin, testSHA); err != nil || !pass {
				t.Fatalf("verify=%t %v", pass, err)
			}
			if fallbacks.Load() != 0 || requests.Load() != 5 {
				t.Fatalf("fallbacks=%d requests=%d", fallbacks.Load(), requests.Load())
			}
			identity := "user"
			if installation {
				identity = "installation:42"
			}
			if len(ledger.records) != 5 {
				t.Fatalf("ledger entries=%d", len(ledger.records))
			}
			for i, row := range ledger.records {
				quota := "core"
				if i == 2 {
					quota = "graphql"
				}
				if row != (recordedUsage{label: "test:pooled", quota: quota, identity: identity}) {
					t.Fatalf("ledger row=%+v", row)
				}
			}
			wantLookups := int32(1)
			if installation {
				wantLookups = 0
			}
			if userLookups.Load() != wantLookups {
				t.Fatal("wrong user credential resolution count")
			}
		})
	}
}

func TestAPIExecCancellationAndTransportError(t *testing.T) {
	e, _ := testAPIExecutor(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `{}`) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.run(ctx, "api", "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
	e.client.Transport = apiExecBrokenTransport{}
	_, err = e.run(context.Background(), "api", "x")
	if err == nil || strings.Contains(err.Error(), "user-secret") || isGitHubTransient(err) {
		t.Fatalf("transport error=%v", err)
	}
}

type apiExecBrokenTransport struct{}

func (apiExecBrokenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection failed with user-secret")
}
