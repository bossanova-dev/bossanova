package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// defaultSentryBaseURL is the production Sentry API base. Self-hosted Sentry
// (custom base URL) is out of scope for v1; the field is overridable in tests.
const defaultSentryBaseURL = "https://sentry.io"

// issueListLimit caps how many issues the picker fetches in one call.
const issueListLimit = 100

// statsPeriod bounds the issue list to recently-active issues.
const statsPeriod = "14d"

// maxStackFrames is how many of the most-relevant (innermost) stacktrace frames
// to include in the rendered markdown.
const maxStackFrames = 5

// eventFetchConcurrency bounds how many latest-event requests run at once when
// enriching the issue list. Without a bound, a project with many unresolved
// issues would serialize up to issueListLimit round-trips and make the picker
// sluggish; the cap keeps wall-clock low without hammering the Sentry API.
const eventFetchConcurrency = 8

// eventEnrichmentBudget bounds the total time spent fetching latest events for
// the issue list, independent of the daemon's (longer) plugin RPC deadline.
// Enrichment is best-effort: without its own budget, a single stalled event
// request would block the whole list call until the RPC deadline fired, so the
// picker would show no issues even though the base issue list already
// succeeded. Issues whose event fetch is cancelled simply render without a
// stacktrace.
const eventEnrichmentBudget = 8 * time.Second

// usefulTagKeys are the tag keys surfaced in the markdown, in display order.
var usefulTagKeys = []string{"environment", "release", "server_name", "transaction"}

// httpClient is used for Sentry API requests with a 30-second timeout to
// prevent hung connections from blocking the TUI indefinitely.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// sentryClient is a REST client for the Sentry issues API.
type sentryClient struct {
	token   string
	org     string
	baseURL string
	http    *http.Client
	// enrichBudget bounds latest-event enrichment; overridable in tests.
	enrichBudget time.Duration
}

// newSentryClient creates a Sentry client for the given credentials, targeting
// the production API base.
func newSentryClient(token, org string) *sentryClient {
	return &sentryClient{
		token:        token,
		org:          org,
		baseURL:      defaultSentryBaseURL,
		http:         httpClient,
		enrichBudget: eventEnrichmentBudget,
	}
}

// sentryIssue is the subset of the Sentry issue object we consume. Sentry
// returns count as a string and userCount as a number.
type sentryIssue struct {
	ID        string `json:"id"`
	ShortID   string `json:"shortId"`
	Title     string `json:"title"`
	Culprit   string `json:"culprit"`
	Permalink string `json:"permalink"`
	Level     string `json:"level"`
	Status    string `json:"status"`
	Count     string `json:"count"`
	UserCount int    `json:"userCount"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
	Metadata  struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"metadata"`
	// Project is populated by the org-level issues endpoint, identifying which
	// project (service) the issue belongs to in a multi-project org.
	Project struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"project"`
}

// sentryFrame is a single stacktrace frame.
type sentryFrame struct {
	Filename string `json:"filename"`
	AbsPath  string `json:"absPath"`
	Module   string `json:"module"`
	Function string `json:"function"`
	LineNo   int    `json:"lineNo"`
}

// sentryEvent is the subset of the latest-event object we consume. The
// exception/stacktrace data shape varies across platforms, so entry data is
// decoded lazily and parsed defensively.
type sentryEvent struct {
	Entries []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"entries"`
	Tags []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"tags"`
}

// ListIssues fetches unresolved issues for the configured project, most
// recently-seen first, and renders each into a TrackerIssue. The latest event
// is fetched best-effort per issue to enrich the markdown with a stacktrace;
// a failure there omits the stacktrace section rather than dropping the issue.
func (c *sentryClient) ListIssues(ctx context.Context, query string) ([]*bossanovav1.TrackerIssue, error) {
	issues, err := c.listUnresolvedIssues(ctx, query)
	if err != nil {
		return nil, err
	}

	// Enrich each issue with its latest event (for the stacktrace) concurrently
	// but bounded. Each goroutine writes only its own out[i] slot, so the writes
	// are race-free without a mutex; wg.Wait happens-before the return read.
	//
	// Enrichment runs under its own short budget (not the caller's full RPC
	// deadline). Because latestEvent honours enrichCtx, every goroutine returns
	// within the budget — a stalled event request is cancelled and renders the
	// issue without a stacktrace rather than blocking the whole list call.
	enrichCtx, cancel := context.WithTimeout(ctx, c.enrichBudget)
	defer cancel()

	out := make([]*bossanovav1.TrackerIssue, len(issues))
	sem := make(chan struct{}, eventFetchConcurrency)
	var wg sync.WaitGroup
	for i := range issues {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Don't block on a concurrency slot past the budget: once it is
			// spent, queued issues skip enrichment and render un-enriched.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-enrichCtx.Done():
				out[i] = toTrackerIssue(issues[i], nil)
				return
			}
			event, _ := c.latestEvent(enrichCtx, issues[i].ID) // best-effort; nil on error/timeout
			out[i] = toTrackerIssue(issues[i], event)
		}(i)
	}
	wg.Wait()
	return out, nil
}

// listUnresolvedIssues performs the issues GET and maps Sentry HTTP errors to
// actionable messages.
func (c *sentryClient) listUnresolvedIssues(ctx context.Context, userQuery string) ([]sentryIssue, error) {
	search := strings.TrimSpace("is:unresolved " + strings.TrimSpace(userQuery))

	// Organization-level issues endpoint: lists issues across every project in
	// the org. project=-1 means "all projects" (a project-scoped endpoint would
	// force picking a single service in a monorepo with one project per service).
	endpoint := fmt.Sprintf("%s/api/0/organizations/%s/issues/",
		c.baseURL, url.PathEscape(c.org))

	params := url.Values{}
	params.Set("query", search)
	params.Set("sort", "date")
	params.Set("limit", fmt.Sprintf("%d", issueListLimit))
	params.Set("statsPeriod", statsPeriod)
	params.Set("project", "-1")

	body, err := c.get(ctx, endpoint+"?"+params.Encode())
	if err != nil {
		return nil, err
	}

	var issues []sentryIssue
	if err := json.Unmarshal(body, &issues); err != nil {
		return nil, fmt.Errorf("decode Sentry issues: %w", err)
	}
	return issues, nil
}

// latestEvent fetches the latest event for an issue (for the stacktrace). It is
// best-effort: callers ignore the error and render without a stacktrace.
func (c *sentryClient) latestEvent(ctx context.Context, issueID string) (*sentryEvent, error) {
	endpoint := fmt.Sprintf("%s/api/0/organizations/%s/issues/%s/events/latest/",
		c.baseURL, url.PathEscape(c.org), url.PathEscape(issueID))
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var event sentryEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("decode Sentry event: %w", err)
	}
	return &event, nil
}

// get performs an authenticated GET and maps non-2xx responses to typed,
// actionable errors mirroring how the Linear plugin surfaces failures.
func (c *sentryClient) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("sentry auth failed (status %d): check the API token and that it has the event:read scope", resp.StatusCode)
	case http.StatusNotFound:
		return nil, fmt.Errorf("sentry resource not found (status 404): check the organization slug")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("sentry rate-limited (status 429): retry shortly")
	default:
		return nil, fmt.Errorf("sentry API error (status %d): %s", resp.StatusCode, snippet(body))
	}
}

// toTrackerIssue maps a Sentry issue (+ optional latest event) into the
// TrackerIssue proto consumed by the new-session picker.
func toTrackerIssue(issue sentryIssue, event *sentryEvent) *bossanovav1.TrackerIssue {
	return &bossanovav1.TrackerIssue{
		ExternalId:  issue.ShortID,
		Title:       issue.Title,
		Description: renderMarkdown(issue, event),
		Url:         issue.Permalink,
		State:       issue.Status,
		BranchName:  branchSlug(issue.ShortID),
	}
}

// branchSlug derives a git branch name from a Sentry short id, mirroring how
// Linear supplies a suggested branch (e.g. "MYPROJ-1AB" -> "fix/myproj-1ab").
func branchSlug(shortID string) string {
	s := strings.ToLower(strings.TrimSpace(shortID))
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '/':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	return "fix/" + strings.Trim(b.String(), "-")
}

// renderMarkdown builds the prompt-seed markdown for an issue. Missing pieces
// are individually omitted; a missing/unparseable event drops only the
// stacktrace section.
func renderMarkdown(issue sentryIssue, event *sentryEvent) string {
	var b strings.Builder

	meta := []string{}
	if issue.Project.Slug != "" {
		meta = append(meta, "**Project:** "+issue.Project.Slug)
	}
	if issue.Culprit != "" {
		meta = append(meta, "**Culprit:** "+issue.Culprit)
	}
	if issue.Level != "" {
		meta = append(meta, "**Level:** "+issue.Level)
	}
	if issue.Count != "" {
		meta = append(meta, "**Events:** "+issue.Count)
	}
	if issue.UserCount > 0 {
		meta = append(meta, fmt.Sprintf("**Users:** %d", issue.UserCount))
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, "   "))
		b.WriteString("\n")
	}

	seen := []string{}
	if issue.FirstSeen != "" {
		seen = append(seen, "**First seen:** "+issue.FirstSeen)
	}
	if issue.LastSeen != "" {
		seen = append(seen, "**Last seen:** "+issue.LastSeen)
	}
	if len(seen) > 0 {
		b.WriteString(strings.Join(seen, "   "))
		b.WriteString("\n")
	}

	if issue.Permalink != "" {
		b.WriteString("**Permalink:** ")
		b.WriteString(issue.Permalink)
		b.WriteString("\n")
	}

	if frames := extractFrames(event); len(frames) > 0 {
		var lines []string
		for _, f := range frames {
			if s := formatFrame(f); s != "" {
				lines = append(lines, s)
			}
		}
		if len(lines) > 0 {
			b.WriteString("\n### Latest stacktrace\n")
			b.WriteString("```\n")
			for _, l := range lines {
				b.WriteString(l)
				b.WriteString("\n")
			}
			b.WriteString("```\n")
		}
	}

	if tags := extractTags(event); len(tags) > 0 {
		b.WriteString("\n### Tags\n")
		for _, t := range tags {
			fmt.Fprintf(&b, "- %s: %s\n", t[0], t[1])
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// formatFrame renders a single frame as "file:function:line", omitting empty
// components.
func formatFrame(f sentryFrame) string {
	file := f.Filename
	if file == "" {
		file = f.AbsPath
	}
	if file == "" {
		file = f.Module
	}
	// A frame with no file and no function carries nothing useful; skip it
	// rather than emit a bare ":line".
	if file == "" && f.Function == "" {
		return ""
	}
	parts := []string{}
	if file != "" {
		parts = append(parts, file)
	}
	if f.Function != "" {
		parts = append(parts, f.Function)
	}
	line := strings.Join(parts, ":")
	if f.LineNo > 0 {
		line = fmt.Sprintf("%s:%d", line, f.LineNo)
	}
	return line
}

// extractFrames pulls the innermost (most-relevant) stacktrace frames from the
// latest event. Sentry orders frames oldest-call-first, so the last frames are
// the crash site; we return up to maxStackFrames of them in crash-first order.
// The exception/stacktrace data shape varies by platform, so both the
// "exception" (values[].stacktrace.frames) and bare "stacktrace" (frames)
// entry shapes are handled, and anything unparseable yields no frames.
func extractFrames(event *sentryEvent) []sentryFrame {
	if event == nil {
		return nil
	}
	var frames []sentryFrame
	for _, entry := range event.Entries {
		switch entry.Type {
		case "exception":
			var data struct {
				Values []struct {
					Stacktrace struct {
						Frames []sentryFrame `json:"frames"`
					} `json:"stacktrace"`
				} `json:"values"`
			}
			if err := json.Unmarshal(entry.Data, &data); err != nil {
				continue
			}
			for _, v := range data.Values {
				frames = append(frames, v.Stacktrace.Frames...)
			}
		case "stacktrace":
			var data struct {
				Frames []sentryFrame `json:"frames"`
			}
			if err := json.Unmarshal(entry.Data, &data); err != nil {
				continue
			}
			frames = append(frames, data.Frames...)
		}
	}
	if len(frames) == 0 {
		return nil
	}

	// Keep the innermost maxStackFrames, then reverse so the crash site is first.
	if len(frames) > maxStackFrames {
		frames = frames[len(frames)-maxStackFrames:]
	}
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}
	return frames
}

// extractTags returns the useful tags present on the event, in display order,
// as [key, value] pairs.
func extractTags(event *sentryEvent) [][2]string {
	if event == nil {
		return nil
	}
	have := map[string]string{}
	for _, t := range event.Tags {
		have[t.Key] = t.Value
	}
	var out [][2]string
	for _, key := range usefulTagKeys {
		if v, ok := have[key]; ok && v != "" {
			out = append(out, [2]string{key, v})
		}
	}
	return out
}

// snippet truncates a response body for inclusion in an error message.
func snippet(body []byte) string {
	const max = 200
	s := strings.TrimSpace(string(body))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
