// Home's ticket notice: a merged PR whose tracker ticket bossd could not move.
// bossd records each one as a repo note tagged ticket-not-moved, which the web
// lists under Notes but nothing else in the TUI shows. Home shows every such
// note until the operator dismisses it, which tags the note seen, so the
// dismissal holds across restarts and on every TUI driving the same daemon.

package home

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

// ticketNoticeRefreshInterval bounds how often the poll tick re-reads the
// notes. Every (re)entry to Home reads them once more.
const ticketNoticeRefreshInterval = time.Minute

// ticketNoticeShown caps how many notices are listed before "and N more".
const ticketNoticeShown = 3

// ticketNoticesMsg carries one read of the notices back to the Home that
// issued it.
type ticketNoticesMsg struct {
	HomeGeneration uint64
	notes          []*pb.Note
	Err            error
}

// ticketNoticesDismissedMsg reports a dismissal; ids failed to be tagged seen.
type ticketNoticesDismissedMsg struct {
	HomeGeneration uint64
	failed         []string
	Err            error
}

func fetchTicketNotices(c Client, ctx context.Context, generation uint64) tea.Cmd {
	if c == nil {
		return nil
	}
	return func() tea.Msg {
		notes, err := c.ListNotes(ctx, &pb.ListNotesRequest{Tags: []string{models.NoteTagTicketNotMoved}})
		return ticketNoticesMsg{HomeGeneration: generation, notes: notes, Err: err}
	}
}

// startTicketNoticesFetch issues a read unless one is in flight.
func (h *Model) startTicketNoticesFetch() tea.Cmd {
	if h.ticketNoticesFetching {
		return nil
	}
	cmd := fetchTicketNotices(h.Client, h.Ctx, h.Generation)
	if cmd == nil {
		return nil
	}
	h.ticketNoticesFetching = true
	h.ticketNoticesFetchedAt = h.currentTime()
	return cmd
}

// tickTicketNotices is the poll tick's re-read, at most once per
// ticketNoticeRefreshInterval.
func (h *Model) tickTicketNotices() tea.Cmd {
	if h.currentTime().Sub(h.ticketNoticesFetchedAt) < ticketNoticeRefreshInterval {
		return nil
	}
	return h.startTicketNoticesFetch()
}

// handleTicketNotices keeps the previous notices when the read fails, so one
// failed read neither clears nor flashes the line.
func (h Model) handleTicketNotices(msg ticketNoticesMsg) (tea.Model, tea.Cmd) {
	if msg.HomeGeneration != h.Generation {
		return h, nil
	}
	h.ticketNoticesFetching = false
	if msg.Err != nil {
		return h, nil
	}
	var unseen []*pb.Note
	for _, n := range msg.notes {
		if slices.Contains(n.GetTags(), models.NoteTagSeen) {
			continue
		}
		if _, dismissed := h.ticketNoticesDismissed[n.GetId()]; dismissed {
			continue
		}
		unseen = append(unseen, n)
	}
	h.ticketNotices = unseen
	h.Table.SetHeight(h.TableHeight())
	return h, nil
}

// dismissTicketNotices hides every notice now and tags each note seen. A note
// that cannot be tagged comes back with the next read.
func (h Model) dismissTicketNotices() (Model, tea.Cmd) {
	notes := h.ticketNotices
	if len(notes) == 0 || h.Client == nil {
		return h, nil
	}
	if h.ticketNoticesDismissed == nil {
		h.ticketNoticesDismissed = make(map[string]struct{})
	}
	for _, n := range notes {
		h.ticketNoticesDismissed[n.GetId()] = struct{}{}
	}
	h.ticketNotices = nil
	h.Table.SetHeight(h.TableHeight())
	c, ctx, generation := h.Client, h.Ctx, h.Generation
	return h, func() tea.Msg {
		var failed []string
		var lastErr error
		for _, n := range notes {
			tags := append(slices.Clone(n.GetTags()), models.NoteTagSeen)
			_, err := c.UpdateNote(ctx, n.GetRepoId(), &pb.UpdateNoteRequest{Id: n.GetId(), Tags: &pb.NoteTagSet{Tags: tags}})
			if err != nil {
				failed = append(failed, n.GetId())
				lastErr = err
			}
		}
		return ticketNoticesDismissedMsg{HomeGeneration: generation, failed: failed, Err: lastErr}
	}
}

func (h Model) handleTicketNoticesDismissed(msg ticketNoticesDismissedMsg) (tea.Model, tea.Cmd) {
	if msg.HomeGeneration != h.Generation || msg.Err == nil {
		return h, nil
	}
	for _, id := range msg.failed {
		delete(h.ticketNoticesDismissed, id)
	}
	h.setStatus(ui.RPCStatusMessage("The ticket notice could not be dismissed", msg.Err), statusFailure)
	return h, h.startTicketNoticesFetch()
}

// ticketNoticeText is the notice in plain words: the note itself when there is
// one, else how many there are and the first few.
func ticketNoticeText(notes []*pb.Note) string {
	firstLine := func(n *pb.Note) string {
		line, _, _ := strings.Cut(strings.TrimSpace(n.GetBody()), "\n")
		return strings.TrimSuffix(line, ".") + "."
	}
	if len(notes) == 1 {
		return firstLine(notes[0]) + " [c]lear"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The tickets of %d merged PRs were not updated. [c]lear all", len(notes))
	for i, n := range notes {
		if i == ticketNoticeShown {
			fmt.Fprintf(&b, "\nand %d more", len(notes)-ticketNoticeShown)
			break
		}
		b.WriteString("\n" + firstLine(n))
	}
	return b.String()
}

// ticketNoticeLine renders the notice, or "" when there is none to show.
func (h Model) ticketNoticeLine() string {
	if len(h.ticketNotices) == 0 {
		return ""
	}
	return h.statusLine(ui.ColorWarning, ticketNoticeText(h.ticketNotices))
}
