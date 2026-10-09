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
	// Sync is the note's cloud-sync outbox state (BOS-1429). Nil when the note
	// has no outbox row, which a store that predates the outbox never writes.
	Sync *NoteSyncState
}

// NoteSyncStatus is where a note's latest local version stands in the
// daemon-to-Bosso sync pipeline. The values mirror the CHECK constraint on
// note_sync_states.sync_state.
type NoteSyncStatus string

const (
	// NoteSyncPending means a local version has not yet been accepted by the
	// cloud; the sync worker will send it when next_attempt_at is due.
	NoteSyncPending NoteSyncStatus = "pending"
	// NoteSyncSynced means the cloud accepted the version in SyncedVersion.
	NoteSyncSynced NoteSyncStatus = "synced"
	// NoteSyncRejected means the cloud permanently refused the payload as
	// invalid; retrying the same version cannot succeed.
	NoteSyncRejected NoteSyncStatus = "rejected"
	// NoteSyncRateLimited means the cloud asked the daemon to back off; the
	// worker retries after next_attempt_at.
	NoteSyncRateLimited NoteSyncStatus = "rate_limited"
	// NoteSyncExpired means the note is older than the cloud retention window,
	// so the cloud will never hold it.
	NoteSyncExpired NoteSyncStatus = "expired"
	// NoteSyncNotEntitled means the connected organization's plan does not
	// include note sync.
	NoteSyncNotEntitled NoteSyncStatus = "not_entitled"
	// NoteSyncRefused means the cloud declined the note for a policy reason
	// other than entitlement.
	NoteSyncRefused NoteSyncStatus = "refused"
	// NoteSyncSuppressed means sync was deliberately skipped for this note
	// (for example the repo is not mapped to an organization).
	NoteSyncSuppressed NoteSyncStatus = "suppressed"
	// NoteSyncFailed means the last attempt hit a transient error; the worker
	// retries after next_attempt_at.
	NoteSyncFailed NoteSyncStatus = "failed"
)

// Valid reports whether s is one of the defined sync states.
func (s NoteSyncStatus) Valid() bool {
	switch s {
	case NoteSyncPending, NoteSyncSynced, NoteSyncRejected, NoteSyncRateLimited,
		NoteSyncExpired, NoteSyncNotEntitled, NoteSyncRefused, NoteSyncSuppressed,
		NoteSyncFailed:
		return true
	}
	return false
}

// NoteSyncState is one note's row in the sync outbox. SourceVersion counts
// local writes (create = 1, every update or delete bumps it); SyncedVersion is
// the last version the cloud accepted, 0 when none has been.
type NoteSyncState struct {
	State           NoteSyncStatus
	SourceVersion   int64
	SyncedVersion   int64
	IsDeleted       bool
	AttemptCount    int
	NextAttemptAt   *time.Time
	LastAttemptedAt *time.Time
	SyncedAt        *time.Time
	OrganizationID  *string
	LastError       *string
}
