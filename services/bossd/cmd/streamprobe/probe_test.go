package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// duplexHandler responds with headers immediately, then drains the body -
// the behavior of a transport that preserves full duplex.
func duplexHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_, _ = io.Copy(io.Discard, r.Body)
}

// halfDuplexHandler reads the entire request body before responding - the
// behavior observed through the Cloudflare proxy path.
func halfDuplexHandler(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
}

func newH2Server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func newHTTP1Server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = false
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeDuplex_FullDuplexPath(t *testing.T) {
	srv := newH2Server(t, duplexHandler)
	res, err := probeDuplex(srv.URL, 2*time.Second, true)
	if err != nil {
		t.Fatalf("probeDuplex: %v", err)
	}
	if !res.Duplex {
		t.Fatalf("expected duplex PASS, got %+v", res)
	}
}

func TestProbeDuplex_HalfDuplexPath(t *testing.T) {
	srv := newH2Server(t, halfDuplexHandler)
	res, err := probeDuplex(srv.URL, 2*time.Second, true)
	if err != nil {
		t.Fatalf("probeDuplex: %v", err)
	}
	if res.Duplex {
		t.Fatalf("expected duplex FAIL, got %+v", res)
	}
}

func TestProbeDuplex_HTTP1EarlyResponseFails(t *testing.T) {
	srv := newHTTP1Server(t, duplexHandler)
	res, err := probeDuplex(srv.URL, 2*time.Second, true)
	if err != nil {
		t.Fatalf("probeDuplex: %v", err)
	}
	if res.Duplex {
		t.Fatalf("expected HTTP/1.1 early response to fail duplex probe, got %+v", res)
	}
	if res.Proto != "HTTP/1.1" {
		t.Fatalf("expected HTTP/1.1 response, got %+v", res)
	}
}
