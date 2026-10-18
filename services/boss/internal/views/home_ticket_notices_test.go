package views

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/boss/internal/client"
)

type ticketNoticeClient struct {
	client.BossClient
	updateErr error
	updates   []*pb.UpdateNoteRequest
	listErr   error
	lists     int
}

func (c *ticketNoticeClient) ListNotes(context.Context, *pb.ListNotesRequest) ([]*pb.Note, error) {
	c.lists++
	return nil, c.listErr
}

func (c *ticketNoticeClient) UpdateNote(_ context.Context, _ string, req *pb.UpdateNoteRequest) (*pb.Note, error) {
	c.updates = append(c.updates, req)
	return nil, c.updateErr
}

func ticketNote(id, body string, tags ...string) *pb.Note {
	return &pb.Note{Id: id, RepoId: "r1", Body: body, Tags: append([]string{models.NoteTagTicketNotMoved}, tags...)}
}

func noticeHome(c client.BossClient) HomeModel {
	return HomeModel{ctx: context.Background(), client: c, generation: 7, width: 120, height: 40}
}

func applyNotices(t *testing.T, h HomeModel, msg tea.Msg) HomeModel {
	t.Helper()
	m, _ := h.Update(msg)
	return m.(HomeModel)
}

func TestHomeTicketNotice_AFailedReadKeepsTheNoticeAndSeenNotesStayHidden(t *testing.T) {
	h := noticeHome(&ticketNoticeClient{})
	h = applyNotices(t, h, ticketNoticesMsg{homeGeneration: 7, notes: []*pb.Note{
		ticketNote("a", `PR #6660 merged, but its ticket MAD-1764 could not be moved (state "Done" not found)`),
		ticketNote("b", "PR #5555 merged, but its ticket MAD-1 could not be moved", models.NoteTagSeen),
	}})
	line := h.ticketNoticeLine()
	if !strings.Contains(line, "PR #6660") || strings.Contains(line, "PR #5555") {
		t.Fatalf("notice = %q, want only the unseen note", line)
	}

	h = applyNotices(t, h, ticketNoticesMsg{homeGeneration: 7, err: errors.New("daemon unreachable")})
	if got := h.ticketNoticeLine(); got != line {
		t.Fatalf("after a failed read the notice is %q, want it kept as %q", got, line)
	}

	h = applyNotices(t, h, ticketNoticesMsg{homeGeneration: 6, notes: nil})
	if h.ticketNoticeLine() == "" {
		t.Fatal("a read from a replaced Home cleared the notice")
	}
}

func TestHomeTicketNotice_ManyNoticesListTheFirstFew(t *testing.T) {
	var notes []*pb.Note
	for i := 1; i <= 5; i++ {
		notes = append(notes, ticketNote(fmt.Sprint(i), fmt.Sprintf("PR #%d merged, but its ticket T-%d could not be moved", i, i)))
	}
	want := "The tickets of 5 merged PRs were not updated. [c]lear all\n" +
		"PR #1 merged, but its ticket T-1 could not be moved.\n" +
		"PR #2 merged, but its ticket T-2 could not be moved.\n" +
		"PR #3 merged, but its ticket T-3 could not be moved.\n" +
		"and 2 more"
	if got := ticketNoticeText(notes); got != want {
		t.Fatalf("ticketNoticeText =\n%s\nwant\n%s", got, want)
	}
}

func TestHomeTicketNotice_DismissTagsSeenAndAFailureBringsItBack(t *testing.T) {
	c := &ticketNoticeClient{updateErr: errors.New("note store locked")}
	h := noticeHome(c)
	h = applyNotices(t, h, ticketNoticesMsg{homeGeneration: 7, notes: []*pb.Note{ticketNote("a", "PR #6660 merged, but its ticket MAD-1764 could not be moved", "tracker")}})

	m, cmd := h.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	h = m.(HomeModel)
	if h.ticketNoticeLine() != "" {
		t.Fatal("the notice stayed on screen after [c]")
	}
	if cmd == nil {
		t.Fatal("[c] issued no write")
	}
	result := cmd()
	if len(c.updates) != 1 || c.updates[0].GetId() != "a" || strings.Join(c.updates[0].GetTags().GetTags(), ",") != "ticket-not-moved,tracker,seen" {
		t.Fatalf("updates = %+v, want note a tagged seen with its tags kept", c.updates)
	}

	// A read that was in flight when [c] was pressed must not bring it back.
	h = applyNotices(t, h, ticketNoticesMsg{homeGeneration: 7, notes: []*pb.Note{ticketNote("a", "PR #6660 merged", "tracker")}})
	if h.ticketNoticeLine() != "" {
		t.Fatal("a read in flight before the dismissal re-showed the notice")
	}

	h = applyNotices(t, h, result)
	if !h.statusErr || !strings.Contains(h.status, "could not be dismissed") {
		t.Fatalf("status = %q (err %v), want the failed dismissal said", h.status, h.statusErr)
	}
	h = applyNotices(t, h, ticketNoticesMsg{homeGeneration: 7, notes: []*pb.Note{ticketNote("a", "PR #6660 merged", "tracker")}})
	if !strings.Contains(h.ticketNoticeLine(), "PR #6660") {
		t.Fatal("a note that was never tagged seen did not come back")
	}
}

func TestHomeTicketNotice_ClearKeyWithNoNoticeDoesNothing(t *testing.T) {
	c := &ticketNoticeClient{}
	m, cmd := noticeHome(c).Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if cmd != nil {
		t.Fatal("[c] with no notice scheduled work, want none")
	}
	if c.lists != 0 || len(c.updates) != 0 {
		t.Fatalf("[c] with no notice made %d reads and %d writes, want none", c.lists, len(c.updates))
	}
	if m.(HomeModel).ticketNoticesFetching {
		t.Fatal("[c] with no notice started a notes read")
	}
}

func TestHomeTicketNotice_TickRereadsAtMostOncePerMinute(t *testing.T) {
	c := &ticketNoticeClient{listErr: errors.New("daemon unreachable")}
	t0 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	clock := t0
	h := noticeHome(c)
	h.now = func() time.Time { return clock }
	h.ticketNoticesFetchedAt = t0

	var inFlight tea.Cmd
	tickAt := func(at time.Duration, wantRead bool) {
		t.Helper()
		clock = t0.Add(at)
		before := c.lists
		cmd := h.tickTicketNotices()
		if cmd != nil {
			inFlight = cmd
			inFlight()
		}
		want := 0
		if wantRead {
			want = 1
		}
		if got := c.lists - before; got != want {
			t.Fatalf("tick at %v made %d reads, want read=%v", at, got, wantRead)
		}
	}
	finish := func() {
		t.Helper()
		h = applyNotices(t, h, inFlight())
	}

	tickAt(59*time.Second, false)
	tickAt(61*time.Second, true)
	tickAt(125*time.Second, false) // the 61s read is still in flight
	finish()
	tickAt(125*time.Second, true)
	finish()
	tickAt(184*time.Second, false) // 59s after the last read started
	tickAt(186*time.Second, true)
}

func TestHomeTicketNotice_EachKeyClearsOnlyItsOwnLine(t *testing.T) {
	oldCachePath := upgradeCachePath
	t.Cleanup(func() { upgradeCachePath = oldCachePath })
	cachePath := filepath.Join(t.TempDir(), "upgrade-cache.json")
	upgradeCachePath = func() string { return cachePath }

	bothShown := func(c *ticketNoticeClient) HomeModel {
		h := noticeHome(c)
		h = applyNotices(t, h, upgradeCheckMsg{current: "v1.2.3", latest: "v1.2.4", url: "https://example.test/release", available: true})
		h = applyNotices(t, h, ticketNoticesMsg{homeGeneration: 7, notes: []*pb.Note{ticketNote("a", "PR #6660 merged, but its ticket MAD-1764 could not be moved")}})
		if !strings.Contains(h.upgradeStatusView(), "[d]ismiss") || !strings.Contains(h.ticketNoticeLine(), "[c]lear") {
			t.Fatalf("want both lines shown, got upgrade %q and notice %q", h.upgradeStatusView(), h.ticketNoticeLine())
		}
		return h
	}

	t.Run("d snoozes the upgrade only", func(t *testing.T) {
		c := &ticketNoticeClient{}
		h := applyNotices(t, bothShown(c), tea.KeyPressMsg{Code: 'd', Text: "d"})
		if h.upgradeAvailable || strings.Contains(h.upgradeStatusView(), "Upgrade available") {
			t.Fatal("[d] left the upgrade banner shown")
		}
		if !strings.Contains(h.ticketNoticeLine(), "PR #6660") || len(c.updates) != 0 {
			t.Fatalf("[d] touched the ticket notice: line %q, %d note writes", h.ticketNoticeLine(), len(c.updates))
		}
	})

	t.Run("c clears the ticket notice only", func(t *testing.T) {
		c := &ticketNoticeClient{}
		m, cmd := bothShown(c).Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
		h := m.(HomeModel)
		if h.ticketNoticeLine() != "" || cmd == nil {
			t.Fatalf("[c] did not clear the ticket notice: line %q", h.ticketNoticeLine())
		}
		cmd()
		if len(c.updates) != 1 {
			t.Fatalf("[c] made %d note writes, want 1", len(c.updates))
		}
		if !h.upgradeAvailable || !strings.Contains(h.upgradeStatusView(), "Upgrade available") {
			t.Fatal("[c] cleared the upgrade banner")
		}
	})
}
