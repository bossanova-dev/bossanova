package r2upload

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

type uploadTestSource struct{ token string }

func (s *uploadTestSource) UploadTarget() (domain.EvidenceUploadTarget, bool) {
	return domain.EvidenceUploadTarget{AccountID: "account", Bucket: "proof", PublicBaseURL: "https://proof.example", Token: s.token}, s.token != ""
}

func TestUploadHTTP(t *testing.T) {
	const token = "secret-upload-token"
	wantBody := "png"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.EscapedPath() != "/accounts/account/r2/buckets/proof/objects/verify-evidence%2Frun%2Fa%20b.png" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Content-Type") != "image/png" || r.ContentLength != int64(len(wantBody)) {
			t.Error("incorrect upload headers")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != wantBody {
			t.Error("incorrect body")
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	source := &uploadTestSource{token: token}
	uploader := New(source, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	bucket, err := uploader.OpenEvidenceBucket()
	if err != nil {
		t.Fatal(err)
	}
	if bucket.PublicBaseURL() != "https://proof.example" {
		t.Fatal("wrong public URL")
	}
	if err := bucket.Put(context.Background(), "verify-evidence/run/a b.png", "image/png", strings.NewReader("png"), 3); err != nil {
		t.Fatal(err)
	}
	wantBody = ""
	if err := bucket.Put(context.Background(), "verify-evidence/run/a b.png", "image/png", io.LimitReader(strings.NewReader(wantBody), 0), 0); err != nil {
		t.Fatal(err)
	}
	source.token = ""
	if _, err := uploader.OpenEvidenceBucket(); !errors.Is(err, domain.ErrEvidenceUploadNotConfigured) {
		t.Fatalf("credential was cached: %v", err)
	}
}

func TestUploadFailureSanitizesReflectedToken(t *testing.T) {
	const token = "a-secret-that-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, strings.Repeat("x", 500)+token+strings.Repeat("y", 2000))
	}))
	defer server.Close()
	bucket, err := New(&uploadTestSource{token: token}, WithBaseURL(server.URL)).OpenEvidenceBucket()
	if err != nil {
		t.Fatal(err)
	}
	err = bucket.Put(context.Background(), "key", "text/plain", strings.NewReader("x"), 1)
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), token) || len(err.Error()) > 1100 {
		t.Fatalf("unsafe or missing failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bucket.Put(ctx, "key", "text/plain", strings.NewReader("x"), 1); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("expected safe cancellation failure: %v", err)
	}
}
