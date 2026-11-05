package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestConditionalReadRevalidatesAndReplaces(t *testing.T) {
	calls := 0
	p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
		calls++
		header := strings.Join(args, " ")
		switch calls {
		case 1:
			if strings.Contains(header, "If-None-Match") {
				t.Fatal(header)
			}
			return "HTTP/2.0 200 OK\r\nETag: W/\"one\"\r\n\r\n{\"id\":1}", nil
		case 2:
			if !strings.Contains(header, "-H If-None-Match: W/\"one\"") {
				t.Fatal(header)
			}
			return "", &ghError{stdout: "HTTP/2.0 304 Not Modified\n\n", err: errors.New("exit status 1")}
		case 3:
			return "HTTP/1.1 200 OK\nEtag: \"two\"\n\n{\"id\":2}", nil
		default:
			if !strings.Contains(header, "If-None-Match: \"two\"") {
				t.Fatal(header)
			}
			return "", &ghError{stdout: "HTTP/2.0 304 Not Modified\n\n", err: errors.New("exit status 1")}
		}
	}))
	for i, want := range []string{`{"id":1}`, `{"id":1}`, `{"id":2}`, `{"id":2}`} {
		got, err := p.ghConditional(context.Background(), "repos/o/r/pulls/7")
		if err != nil || got != want {
			t.Fatalf("read %d = %q, %v; want %q", i, got, err, want)
		}
	}
}

func TestIncludedResponse(t *testing.T) {
	for _, tc := range []struct {
		out        string
		status     int
		etag, body string
		next       bool
	}{
		{"HTTP/1.1 200 OK\r\neTaG: weak\r\nLink: <url>; rel=\"next\"\r\n\r\na\r\nb", 200, "weak", "a\r\nb", true},
		{"HTTP/2.0 304 Not Modified\nEtag: strong\nLink: <url>; rel=\"prev\"\n\n", 304, "strong", "", false},
		{`{"id":1}`, 200, "", `{"id":1}`, false},
		{"HTTP/garbled", 200, "", "HTTP/garbled", false},
	} {
		status, h, body := parseIncludedResponse(tc.out)
		if status != tc.status || h.Get("ETag") != tc.etag || body != tc.body || hasNextPage(h) != tc.next {
			t.Fatalf("parse %q = %d %v %q", tc.out, status, h, body)
		}
	}
}

func TestConditionalMissingBodyAndErrors(t *testing.T) {
	calls := 0
	failure := errors.New("not found")
	p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
		calls++
		if strings.Contains(strings.Join(args, " "), "If-None-Match") {
			t.Fatal("unexpected validator")
		}
		switch calls {
		case 1:
			return "", &ghError{stdout: "HTTP/2.0 304 Not Modified\n\n", err: errors.New("exit 1")}
		case 2:
			return `[]`, nil
		default:
			return "", failure
		}
	}))
	out, err := p.ghConditional(context.Background(), "path")
	if err != nil || out != "[]" || calls != 2 {
		t.Fatalf("retry = %q %v, calls %d", out, err, calls)
	}
	if _, err = p.ghConditional(context.Background(), "path"); err != failure {
		t.Fatalf("error = %v", err)
	}
	if _, ok := p.etags.get(" path"); ok {
		t.Fatal("unexpected stored entry")
	}
}

func TestConditionalSinglePageFallback(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint(cached), func(t *testing.T) {
			calls := 0
			p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
				calls++
				if calls == 1 {
					out := "HTTP/2.0 200 OK\nEtag: one\nLink: <next>; rel=\"next\"\n\n[1]"
					if cached {
						return "", &ghError{stdout: strings.Replace(out, "200 OK", "304 Not Modified", 1), err: errors.New("exit 1")}
					}
					return out, nil
				}
				if strings.Join(args, " ") != "api path --paginate" {
					t.Fatalf("fallback args %v", args)
				}
				return "[1]\n[2]", nil
			}))
			if cached {
				p.etags.put(" path", "one", "[1]")
			}
			out, err := p.ghConditionalSinglePage(context.Background(), "path")
			values, decodeErr := decodePaginatedPages[int](out, "numbers")
			if err != nil || decodeErr != nil || len(values) != 2 || calls != 2 {
				t.Fatalf("fallback %q %v %v", out, err, decodeErr)
			}
			if _, ok := p.etags.get(" path"); ok {
				t.Fatal("multi-page response cached")
			}
		})
	}
}

func TestETagStoreBounds(t *testing.T) {
	var s etagStore
	for i := 0; i < maxETagEntries; i++ {
		s.put(fmt.Sprint(i), "tag", "body")
	}
	s.get("0")
	s.put("extra", "tag", "body")
	if _, ok := s.get("1"); ok {
		t.Fatal("least recently used entry retained")
	}
	if _, ok := s.get("0"); !ok {
		t.Fatal("recent entry evicted")
	}
	if len(s.entries) != maxETagEntries {
		t.Fatal(len(s.entries))
	}
	s.put("extra", "tag", strings.Repeat("x", maxETagBody+1))
	if _, ok := s.get("extra"); ok {
		t.Fatal("oversized body stored")
	}
}

func TestDefaultRunGHRetainsErrorStdout(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'HTTP/2.0 304 Not Modified\\r\\nEtag: one\\r\\n\\r\\n'\nprintf 'gh: HTTP 304\\n' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := defaultRunGH(context.Background(), "api", "path", "-i")
	var ge *ghError
	if out != "" || !errors.As(err, &ge) {
		t.Fatalf("result %q %v", out, err)
	}
	status, _, _ := parseIncludedResponse(ge.stdout)
	if status != 304 || ge.Error() != "gh api path -i: exit status 1: gh: HTTP 304" || ghResponseText(err) != "gh: HTTP 304\nexit status 1" {
		t.Fatalf("gh error = %#v", ge)
	}
}

func TestConditionalSinglePageAndHostSeparation(t *testing.T) {
	counts := map[string]int{}
	p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
		host := args[4]
		counts[host]++
		if counts[host] == 1 {
			if strings.Contains(strings.Join(args, " "), "If-None-Match") {
				t.Fatalf("host %s reused another host's validator", host)
			}
			return "HTTP/2.0 200 OK\nEtag: tag\n\n[7]", nil
		}
		return "", &ghError{stdout: "HTTP/2.0 304 Not Modified\n\n", err: errors.New("exit 1")}
	}))
	for _, host := range []string{"github.com", "example.test", "github.com"} {
		out, err := p.ghConditionalSinglePage(context.Background(), "path", "--hostname", host)
		values, decodeErr := decodePaginatedPages[int](out, "values")
		if err != nil || decodeErr != nil || len(values) != 1 || values[0] != 7 {
			t.Fatalf("single page %q %v %v", out, err, decodeErr)
		}
	}
}

func TestConditionalResponseWithoutValidatorDropsEntry(t *testing.T) {
	calls := 0
	p := New(zerolog.Nop(), WithRunGH(func(_ context.Context, args ...string) (string, error) {
		calls++
		if calls == 1 {
			return "HTTP/2.0 200 OK\nETag: tag\n\nold", nil
		}
		if calls == 3 && strings.Contains(strings.Join(args, " "), "If-None-Match") {
			t.Fatal("validator retained after headerless response")
		}
		return "new", nil
	}))
	for _, want := range []string{"old", "new", "new"} {
		out, err := p.ghConditional(context.Background(), "path")
		if err != nil || out != want {
			t.Fatalf("%q %v", out, err)
		}
	}
}
