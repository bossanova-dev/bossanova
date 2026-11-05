package github

import (
	"bufio"
	"container/list"
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	maxETagEntries = 256
	maxETagBody    = 1 << 20
)

type etagEntry struct{ key, etag, body string }

type etagStore struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	recent  list.List
}

func (s *etagStore) get(key string) (etagEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return etagEntry{}, false
	}
	s.recent.MoveToFront(e)
	entry, ok := e.Value.(etagEntry)
	return entry, ok
}

func (s *etagStore) drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		s.recent.Remove(e)
		delete(s.entries, key)
	}
}

func (s *etagStore) put(key, etag, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		s.recent.Remove(e)
		delete(s.entries, key)
	}
	if etag == "" || len(body) > maxETagBody {
		return
	}
	if s.entries == nil {
		s.entries = make(map[string]*list.Element)
	}
	s.entries[key] = s.recent.PushFront(etagEntry{key, etag, body})
	if len(s.entries) > maxETagEntries {
		old := s.recent.Back()
		if entry, ok := old.Value.(etagEntry); ok {
			delete(s.entries, entry.key)
		}
		s.recent.Remove(old)
	}
}

// parseIncludedResponse accepts gh api -i output, retaining the body byte for byte.
// Bare bodies keep compatibility with executors that do not emit HTTP headers.
func parseIncludedResponse(out string) (int, http.Header, string) {
	headers := make(http.Header)
	if !strings.HasPrefix(out, "HTTP/") {
		return http.StatusOK, headers, out
	}
	end := strings.Index(out, "\r\n\r\n")
	sep := 4
	if end < 0 {
		end = strings.Index(out, "\n\n")
		sep = 2
	}
	if end < 0 {
		return http.StatusOK, headers, out
	}
	scan := bufio.NewScanner(strings.NewReader(out[:end]))
	if !scan.Scan() {
		return http.StatusOK, headers, out
	}
	fields := strings.Fields(scan.Text())
	if len(fields) < 2 {
		return http.StatusOK, headers, out
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return http.StatusOK, headers, out
	}
	for scan.Scan() {
		if k, v, ok := strings.Cut(scan.Text(), ":"); ok {
			headers.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return status, headers, out[end+sep:]
}

func includedOutput(out string, err error) string {
	var ge *ghError
	if errors.As(err, &ge) {
		return ge.stdout
	}
	return out
}

func conditionalKey(path string, extra []string) string {
	host := ""
	for i := 0; i+1 < len(extra); i++ {
		if extra[i] == "--hostname" {
			host = extra[i+1]
		}
	}
	return host + " " + path
}

var nextPageRelation = regexp.MustCompile(`(?i);\s*rel\s*=\s*"?next(?:"|[,;\s]|$)`)

func hasNextPage(headers http.Header) bool {
	for _, link := range headers.Values("Link") {
		if nextPageRelation.MatchString(link) {
			return true
		}
	}
	return false
}

func (p *Provider) ghConditional(ctx context.Context, path string, extra ...string) (string, error) {
	return p.conditionalRead(ctx, path, false, extra...)
}

func (p *Provider) ghConditionalSinglePage(ctx context.Context, path string, extra ...string) (string, error) {
	return p.conditionalRead(ctx, path, true, extra...)
}

func (p *Provider) conditionalRead(ctx context.Context, path string, singlePage bool, extra ...string) (string, error) {
	key := conditionalKey(path, extra)
	entry, stored := p.etags.get(key)
	for attempt := 0; attempt < 2; attempt++ {
		args := append([]string{"api", path, "-i"}, extra...)
		if stored {
			args = append(args, "-H", "If-None-Match: "+entry.etag)
		}
		out, err := p.gh(ctx, args...)
		status, headers, body := parseIncludedResponse(includedOutput(out, err))
		if singlePage && (err == nil || status == http.StatusNotModified) && hasNextPage(headers) {
			p.etags.drop(key)
			args = append([]string{"api", path, "--paginate"}, extra...)
			return p.gh(ctx, args...)
		}
		if status == http.StatusNotModified {
			if stored {
				if etag := headers.Get("ETag"); etag != "" {
					entry.etag = etag
				}
				p.etags.put(key, entry.etag, entry.body)
				return entry.body, nil
			}
			p.etags.drop(key)
			continue
		}
		if err != nil {
			return "", err
		}
		if status == http.StatusOK {
			p.etags.put(key, headers.Get("ETag"), body)
		}
		return body, nil
	}
	return "", errors.New("GitHub returned 304 without a stored response")
}
