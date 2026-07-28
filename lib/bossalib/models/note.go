package models

import "time"

// NoteMaxBodyBytes caps a note's body. 64 KiB is generous for an agent's
// post-run write-up while keeping a runaway writer from bloating the daemon
// database. It is the canonical value shared by the daemon store and every
// authoring surface (CLI, MCP), so no surface can drift from the store that
// rejects the write.
const NoteMaxBodyBytes = 64 * 1024

// NoteMaxTagLength caps a single tag, measured in bytes after trimming.
const NoteMaxTagLength = 64

// NoteMaxTags caps how many distinct tags one note may carry. Counted after
// normalisation, so case variants that fold together count once.
const NoteMaxTags = 32

// Note is a durable free-text record attached to a repository, optionally
// carrying the session and chat that produced it.
//
// Ownership is repo-scoped: SessionID and ChatID are nullable PROVENANCE, not
// owners. Deleting or archiving the referenced session leaves the note intact —
// notes exist to be read after the run that wrote them is gone.
//
// Tag normalisation is LOSSY: tags are trimmed, lowercased, and de-duplicated
// on write, so a caller cannot round-trip display casing ("Tech-Debt" is stored
// and returned as "tech-debt"). This is deliberate — it is what makes the
// note_tags primary key meaningful and tag filtering case-insensitive.
type Note struct {
	ID        string
	RepoID    string
	SessionID *string // nil = not session-scoped
	ChatID    *string // nil = not chat-scoped
	Body      string
	// Tags are normalised (trimmed, lowercased, de-duplicated) and returned in
	// ascending order for a deterministic read.
	Tags      []string
	CreatedAt time.Time
	UpdatedAt time.Time
}
