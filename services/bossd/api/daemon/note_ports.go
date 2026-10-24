package server

import (
	"context"

	"github.com/recurser/bossd/internal/domain"
)

type noteStore interface {
	Create(context.Context, domain.CreateNoteParams) (*domain.Note, error)
	Get(context.Context, string) (*domain.Note, error)
	List(context.Context, domain.ListNotesFilter) ([]*domain.Note, error)
	Update(context.Context, domain.UpdateNoteParams) (*domain.Note, error)
	Delete(context.Context, string) error
}

// NoteSyncCounter reports how many note sync outbox rows sit in each state.
type NoteSyncCounter interface {
	CountByState(ctx context.Context) (map[domain.NoteSyncStatus]int64, error)
}

// NoteSyncNudger asks the note sync worker to drain now. Nudge must never
// block: it runs on the note write path. *notesync.Worker satisfies it.
type NoteSyncNudger interface {
	Nudge()
}
