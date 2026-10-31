package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
)

func TestWithTrackerLine(t *testing.T) {
	url := "https://linear.app/acme/issue/BOS-2142"
	blank := " \t"
	for _, tt := range []struct {
		name       string
		url        *string
		body, want string
	}{
		{"prepend", &url, "body\n\n## appended\n", "Tracker issue: " + url + "\n\nbody\n\n## appended\n"},
		{"tracker", &url, "\r\n  tRaCkEr issue: other\r\nbody", "\r\n  tRaCkEr issue: other\r\nbody"},
		{"linear", &url, "\nLINEAR issue: other\nbody", "\nLINEAR issue: other\nbody"},
		{"nil", nil, "body", "body"},
		{"blank", &blank, "body", "body"},
		{"later line", &url, "intro\nTracker issue: other", "Tracker issue: " + url + "\n\nintro\nTracker issue: other"},
		{"invalid line", &url, "Tracker issue: two words", "Tracker issue: " + url + "\n\nTracker issue: two words"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := withTrackerLine(tt.url, tt.body); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

type fakeTicketTagProvider struct {
	*mockVCSProvider
	bodies  []string
	bodyErr error
}

func (p *fakeTicketTagProvider) GetPRStatus(ctx context.Context, repo string, n int) (*vcs.PRStatus, error) {
	status, err := p.mockVCSProvider.GetPRStatus(ctx, repo, n)
	if status == nil || err != nil {
		return status, err
	}
	copyStatus := *status
	return &copyStatus, nil
}
func (p *fakeTicketTagProvider) UpdatePRTitle(ctx context.Context, repo string, n int, title string) error {
	if err := p.mockVCSProvider.UpdatePRTitle(ctx, repo, n, title); err != nil {
		return err
	}
	p.nextPRStatus.Title = title
	return nil
}
func (p *fakeTicketTagProvider) UpdatePRBody(_ context.Context, _ string, _ int, body string) error {
	p.bodies = append(p.bodies, body)
	if p.bodyErr != nil {
		return p.bodyErr
	}
	p.nextPRStatus.Body = body
	return nil
}
func ticketTagSession(s *models.Session) {
	id, url, n := "BOS-2142", "https://linear.app/acme/issue/BOS-2142", 42
	s.TrackerID, s.TrackerURL, s.PRNumber = &id, &url, &n
	s.Title = "Current title"
}
func ticketTagHarness(t *testing.T) (*trackerLinkHarness, *fakeTicketTagProvider) {
	t.Helper()
	h := newTrackerLinkHarness(t)
	p := &fakeTicketTagProvider{mockVCSProvider: newMockVCSProvider()}
	p.nextPRStatus.Title = "Current title"
	p.nextPRStatus.Body = "Agent body\n\n## Appended summary\n"
	h.linker.provider = p
	return h, p
}
func TestEnsureTicketTagsAddOnly_Sessions_AC214202(t *testing.T) {
	for _, titleTagged := range []bool{false, true} {
		for _, bodyTagged := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "title", false: "no-title"}[titleTagged], map[bool]string{true: "body", false: "no-body"}[bodyTagged]}, "/"), func(t *testing.T) {
				h, p := ticketTagHarness(t)
				s := h.addSession("s", ticketTagSession)
				if titleTagged {
					p.nextPRStatus.Title = "[BOS-2142] Current title"
				}
				if bodyTagged {
					p.nextPRStatus.Body = "\nLinear issue: another\n" + p.nextPRStatus.Body
				}
				oldBody := p.nextPRStatus.Body
				if _, err := h.linker.EnsureTicketTags(context.Background()); err != nil {
					t.Fatal(err)
				}
				if p.nextPRStatus.Title != "[BOS-2142] Current title" {
					t.Fatal(p.nextPRStatus.Title)
				}
				want := oldBody
				if !bodyTagged {
					want = "Tracker issue: " + *s.TrackerURL + "\n\n" + oldBody
				}
				if p.nextPRStatus.Body != want {
					t.Fatalf("body=%q want=%q", p.nextPRStatus.Body, want)
				}
				if len(p.updatePRTitleCalls) != map[bool]int{true: 0, false: 1}[titleTagged] || len(p.bodies) != map[bool]int{true: 0, false: 1}[bodyTagged] {
					t.Fatal("wrote existing tag")
				}
				runTicketTags(t, h.linker)
				if len(p.getPRStatusPRNumbers) != 1 {
					t.Fatal("tagged PR re-read")
				}
			})
		}
	}
}
func TestEnsureTicketTagsSkips_Sessions_AC214202(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*models.Session)
		closed bool
	}{
		{"no id", func(s *models.Session) { s.TrackerID = nil }, false},
		{"no url", func(s *models.Session) { s.TrackerURL = nil }, false},
		{"blank id", func(s *models.Session) { v := " "; s.TrackerID = &v }, false},
		{"blank url", func(s *models.Session) { v := " "; s.TrackerURL = &v }, false},
		{"quick chat", func(s *models.Session) { s.IsQuickChat = true }, false},
		{"no pr", func(s *models.Session) { s.PRNumber = nil }, false},
		{"closed", func(*models.Session) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p := ticketTagHarness(t)
			h.addSession("s", func(s *models.Session) { ticketTagSession(s); tc.mutate(s) })
			if tc.closed {
				p.nextPRStatus.State = vcs.PRStateClosed
			}
			runTicketTags(t, h.linker)
			if len(p.bodies)+len(p.updatePRTitleCalls) != 0 {
				t.Fatal("unexpected edits")
			}
		})
	}
}
func TestEnsureTicketTagsRetry_Sessions_AC214202(t *testing.T) {
	for _, phase := range []string{"read", "title", "body"} {
		t.Run(phase, func(t *testing.T) {
			h, p := ticketTagHarness(t)
			s := h.addSession("s", ticketTagSession)
			failure := errors.New("transient")
			switch phase {
			case "read":
				p.prStatusErr = failure
			case "title":
				p.updatePRTitleErr = failure
			case "body":
				p.bodyErr = failure
			}
			runTicketTags(t, h.linker)
			if *s.TrackerID != "BOS-2142" || *s.TrackerURL != "https://linear.app/acme/issue/BOS-2142" {
				t.Fatal("link modified")
			}
			p.prStatusErr, p.updatePRTitleErr, p.bodyErr = nil, nil, nil
			runTicketTags(t, h.linker)
			if p.nextPRStatus.Title != "[BOS-2142] Current title" || !hasTrackerLine(p.nextPRStatus.Body) {
				t.Fatal("retry missed tags")
			}
			if phase == "body" && len(p.updatePRTitleCalls) != 1 {
				t.Fatal("retry rewrote title")
			}
			runTicketTags(t, h.linker)
			if len(p.getPRStatusPRNumbers) != 2 {
				t.Fatal("successful retry not memoized")
			}
		})
	}
}
func TestEnsureTicketTagsBudget_Sessions_AC214202(t *testing.T) {
	h, p := ticketTagHarness(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		h.addSession(id, ticketTagSession)
	}
	runTicketTags(t, h.linker)
	if len(p.getPRStatusPRNumbers) != maxTicketTagChecksPerPass {
		t.Fatal("budget exceeded")
	}
	runTicketTags(t, h.linker)
	if len(p.getPRStatusPRNumbers) != 7 {
		t.Fatal("remainder not checked")
	}
	h.linker.provider = nil
	if n, err := h.linker.EnsureTicketTags(context.Background()); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}
func TestEnsureTicketTagsTitleOnlyProvider_Sessions_AC214202(t *testing.T) {
	h := newTrackerLinkHarness(t)
	p := newMockVCSProvider()
	p.nextPRStatus.Title = "Current title"
	h.linker.provider = p
	h.addSession("s", ticketTagSession)
	runTicketTags(t, h.linker)
	if len(p.updatePRTitleCalls) != 1 {
		t.Fatal("title-only provider skipped")
	}
	runTicketTags(t, h.linker)
	if len(p.getPRStatusPRNumbers) != 1 {
		t.Fatal("missing optional body capability not memoized")
	}
}
func TestEnsureTicketTagsRowTitle_Sessions_AC133(t *testing.T) {
	for _, title := range []string{"Current title", "Operator title", "Nightly build"} {
		t.Run(title, func(t *testing.T) {
			h, p := ticketTagHarness(t)
			s := h.addSession("s", func(s *models.Session) { ticketTagSession(s); s.Title = title })
			if title == "Nightly build" {
				id := "cron"
				s.CronJobID = &id
				h.linker.cronJobs = &namedCronJobStore{name: "Nightly build"}
			}
			runTicketTags(t, h.linker)
			want := title
			if title != "Operator title" {
				want = "[BOS-2142] Current title"
			}
			got, _ := h.sessions.Get(context.Background(), s.ID)
			if got.Title != want {
				t.Fatalf("row=%q want=%q", got.Title, want)
			}
			if (len(h.notified) > 0) != (title != "Operator title") {
				t.Fatal("notify mismatch")
			}
			written, err := WriteSessionPRTitle(context.Background(), p, "repo", s, 42, "Renamed")
			if err != nil || written != "[BOS-2142] Renamed" {
				t.Fatal(written, err)
			}
		})
	}
}

func runTicketTags(t *testing.T, l *TrackerLinker) {
	t.Helper()
	if _, err := l.EnsureTicketTags(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapDraftTags_Sessions_AC214201(t *testing.T) {
	sessions, _, provider, lc := newBackgroundDraftPRFixture(t)
	sess := sessions.sessions["sess-1"]
	id, url := "BOS-2142", "https://linear.app/acme/issue/BOS-2142"
	sess.TrackerID, sess.TrackerURL = &id, &url
	sess.Plan = "Implement BOS-2142; leave BOS-9999 for later"
	wantBody := "Tracker issue: " + url + "\n\n" + draftPRBody(sess.Plan, sess.TrackerID)
	if err := lc.StartSession(context.Background(), sess.ID, StartSessionOpts{}); err != nil {
		t.Fatal(err)
	}
	awaitDraftPR(t, lc, sess.ID)
	if len(provider.createPRCalls) != 1 {
		t.Fatalf("creates=%v", provider.createPRCalls)
	}
	got := provider.createPRCalls[0]
	if got.Body != wantBody || got.Title != "[BOS-2142] Test Session" {
		t.Fatalf("created title=%q body=%q", got.Title, got.Body)
	}
}

func TestEnsureTicketTagsPersistentFailuresRotate(t *testing.T) {
	h, p := ticketTagHarness(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		h.addSession(id, func(s *models.Session) {
			ticketTagSession(s)
			n := int(id[0])
			s.PRNumber = &n
		})
	}
	p.prStatusErr = errors.New("persistent failure")
	for range 3 {
		runTicketTags(t, h.linker)
	}
	counts := make(map[int]int)
	for _, n := range p.getPRStatusPRNumbers {
		counts[n]++
	}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		if counts[int(id[0])] < 2 {
			t.Fatalf("session %s was starved: attempts=%v", id, counts)
		}
	}
}

func TestEnsureTicketTagsRetriesLocalTitle(t *testing.T) {
	for _, title := range []string{"Current title", "Operator title", "Nightly build"} {
		t.Run(title, func(t *testing.T) {
			h, p := ticketTagHarness(t)
			s := h.addSession("s", func(s *models.Session) { ticketTagSession(s); s.Title = title })
			if title == "Nightly build" {
				id := "cron"
				s.CronJobID = &id
				h.linker.cronJobs = &namedCronJobStore{name: title}
			}
			h.sessions.updateErr[s.ID] = errors.New("database unavailable")
			runTicketTags(t, h.linker)
			if p.nextPRStatus.Title != "[BOS-2142] Current title" || s.Title != title {
				t.Fatalf("remote=%q local=%q", p.nextPRStatus.Title, s.Title)
			}
			delete(h.sessions.updateErr, s.ID)
			runTicketTags(t, h.linker)
			want := "[BOS-2142] Current title"
			if title == "Operator title" {
				want = title
			}
			if s.Title != want || (len(h.notified) == 1) != (title != "Operator title") {
				t.Fatalf("local=%q notifications=%d", s.Title, len(h.notified))
			}
			if len(p.updatePRTitleCalls) != 1 {
				t.Fatal("retried remote title write")
			}
			reads := len(p.getPRStatusPRNumbers)
			runTicketTags(t, h.linker)
			if len(p.getPRStatusPRNumbers) != reads {
				t.Fatal("successful retry not memoized")
			}
		})
	}
}
