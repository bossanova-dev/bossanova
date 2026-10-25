package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossd/internal/adapter/r2upload"
	"github.com/recurser/bossd/internal/domain"
)

type evidenceRepoList struct {
	repoStore
	repo *domain.Repo
}

func (r evidenceRepoList) List(context.Context) ([]*domain.Repo, error) {
	return []*domain.Repo{r.repo}, nil
}

type evidenceSessionList struct {
	sessionStore
	session *domain.Session
}

func (s evidenceSessionList) List(context.Context, string) ([]*domain.Session, error) {
	return []*domain.Session{s.session}, nil
}

type evidenceTargetSource struct{ target domain.EvidenceUploadTarget }

func (s evidenceTargetSource) UploadTarget() (domain.EvidenceUploadTarget, bool) {
	return s.target, s.target.Token != ""
}

func evidenceServer(t *testing.T, endpoint string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	dir := filepath.Join(worktree, ".verify-evidence", "run")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return &Server{
		repos:                evidenceRepoList{repo: &domain.Repo{ID: "repo", LocalPath: root, IsTrusted: true}},
		sessions:             evidenceSessionList{session: &domain.Session{ID: "session", RepoID: "repo", WorktreePath: worktree}},
		evidenceBucketOpener: r2upload.New(evidenceTargetSource{domain.EvidenceUploadTarget{AccountID: "account", Bucket: "bucket", PublicBaseURL: "https://proof.example", Token: "private-token"}}, r2upload.WithBaseURL(endpoint)),
	}, dir
}

func TestUploadEvidencePublishesWithDaemonCredentials(t *testing.T) {
	var keys []string
	expectedTypes := map[string]string{"manifest.json": "application/json", "video.mp4": "video/mp4", "poster.png": "image/png", "request-log.ndjson": "application/x-ndjson", "screenshots/first.webp": "image/webp"}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.Header.Get("Authorization") != "Bearer private-token" {
			t.Errorf("unexpected authenticated method")
		}
		name := strings.SplitN(r.URL.Path, "/verify-evidence/", 2)[1]
		name = strings.SplitN(name, "/", 2)[1]
		if r.Header.Get("Content-Type") != expectedTypes[name] {
			t.Errorf("content type = %s", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" {
			t.Errorf("body = %q", body)
		}
		keys = append(keys, strings.TrimPrefix(r.URL.Path, "/accounts/account/r2/buckets/bucket/objects/"))
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()
	s, dir := evidenceServer(t, endpoint.URL)
	var objects []*pb.EvidenceObject
	for name := range expectedTypes {
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, &pb.EvidenceObject{Name: name, Path: name})
	}
	resp, err := s.UploadEvidence(context.Background(), connect.NewRequest(&pb.UploadEvidenceRequest{EvidenceDir: dir, Objects: objects}))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^verify-evidence/[A-Za-z0-9_-]{22}/$`).MatchString(resp.Msg.KeyPrefix) {
		t.Fatalf("prefix = %q", resp.Msg.KeyPrefix)
	}
	if resp.Msg.PublicBaseUrl != "https://proof.example" || len(keys) != len(expectedTypes) || len(resp.Msg.ObjectKeys) != len(keys) {
		t.Fatalf("unexpected result: %v", resp.Msg)
	}
	for i, key := range keys {
		if !strings.HasPrefix(key, resp.Msg.KeyPrefix) || resp.Msg.ObjectKeys[i] != key {
			t.Fatalf("unexpected published key %s", key)
		}
	}

	t.Logf("daemon minted prefix %s", resp.Msg.KeyPrefix)
}

func TestUploadEvidenceRefusesUnsafeRequestsBeforePUT(t *testing.T) {
	for _, name := range []string{"outside", "untrusted", "wrong shape", "parent escape", "symlink escape", "bad name", "duplicate", "oversize", "too many", "nonregular", "fifo", "absolute path", "unclean path"} {
		t.Run(name, func(t *testing.T) {
			puts := 0
			endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { puts++ }))
			defer endpoint.Close()
			s, dir := evidenceServer(t, endpoint.URL)
			req := &pb.UploadEvidenceRequest{EvidenceDir: dir, Objects: []*pb.EvidenceObject{{Name: "manifest.json", Path: "manifest.json"}}}
			code := connect.CodeInvalidArgument
			switch name {
			case "outside":
				req.EvidenceDir = t.TempDir()
				code = connect.CodePermissionDenied
			case "untrusted":
				s.repos.(evidenceRepoList).repo.IsTrusted = false
				code = connect.CodePermissionDenied
			case "wrong shape":
				req.EvidenceDir = filepath.Dir(dir)
			case "parent escape":
				req.Objects[0].Path = "../manifest.json"
			case "absolute path":
				req.Objects[0].Path = filepath.Join(dir, "manifest.json")
			case "unclean path":
				req.Objects[0].Path = "./manifest.json"
			case "symlink escape":
				outside := filepath.Join(t.TempDir(), "data")
				if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
				req.Objects[0].Path = "link"
			case "bad name":
				req.Objects[0].Name = "credentials.txt"
			case "duplicate":
				req.Objects = append(req.Objects, req.Objects[0])
			case "oversize":
				f, err := os.Create(filepath.Join(dir, "large"))
				if err != nil {
					t.Fatal(err)
				}
				if err = f.Truncate(300*1024*1024 + 1); err != nil {
					t.Fatal(err)
				}
				if err = f.Close(); err != nil {
					t.Fatal(err)
				}
				req.Objects[0].Path = "large"
			case "too many":
				for range 64 {
					req.Objects = append(req.Objects, req.Objects[0])
				}
			case "fifo":
				req.Objects[0].Path = "pipe"
				if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
					t.Fatal(err)
				}
			case "nonregular":
				req.Objects[0].Path = "folder"
				if err := os.Mkdir(filepath.Join(dir, "folder"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.UploadEvidence(context.Background(), connect.NewRequest(req))
			if connect.CodeOf(err) != code {
				t.Fatalf("code = %v, want %v; %v", connect.CodeOf(err), code, err)
			}
			if puts != 0 {
				t.Fatalf("refused request sent %d PUTs", puts)
			}
		})
	}
}

func TestUploadEvidenceNoCredential_Sessions_AC99(t *testing.T) {
	s, dir := evidenceServer(t, "http://unused.invalid")
	for _, opener := range []evidenceBucketOpener{nil, r2upload.New(evidenceTargetSource{})} {
		s.evidenceBucketOpener = opener
		_, err := s.UploadEvidence(context.Background(), connect.NewRequest(&pb.UploadEvidenceRequest{EvidenceDir: dir, Objects: []*pb.EvidenceObject{{Name: "manifest.json", Path: "manifest.json"}}}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("code = %v", connect.CodeOf(err))
		}
	}
}

func TestUploadEvidenceHandlerClearsWriteDeadline(t *testing.T) {
	rw := &deadlineCaptureResponseWriter{header: http.Header{}}
	handler := withStreamingWriteDeadlineOverride(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	handler.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, bossanovav1connect.DaemonServiceUploadEvidenceProcedure, nil))
	if !rw.writeDeadlineSet || !rw.writeDeadline.IsZero() {
		t.Fatal("upload write deadline was not cleared")
	}
}
