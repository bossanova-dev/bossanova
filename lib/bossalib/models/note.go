package models

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

// NoteTagTicketNotMoved marks the note bossd leaves when a merged PR's ticket
// could not be moved. Home shows each one until the operator dismisses it.
const NoteTagTicketNotMoved = "ticket-not-moved"

// NoteTagSeen marks a note the operator dismissed from Home.
const NoteTagSeen = "seen"
