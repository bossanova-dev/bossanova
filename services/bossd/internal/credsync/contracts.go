package credsync

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
)

// ErrConflict is a remote compare-and-set conflict. ErrNotFound means no seal/blob/local row exists.
var (
	ErrConflict = errors.New("credential sync conflict")
	ErrNotFound = errors.New("credential sync not found")
	ErrRollback = errors.New("credential sync rollback refused")
)

// Device contains public enrollment evidence; identities are checked against local pins.
type Device struct {
	DaemonID, Status, ApprovedByDaemonID                  string
	DevicePublicKey, IdentityPublicKey, ApprovalSignature []byte
}

// KeySeal is an opaque box destined for one enrolled daemon.
type KeySeal struct {
	DaemonID            string
	SealedCredentialKey []byte
}

// Blob is a versioned encrypted credential. Its version is also authenticated by AES-GCM.
type Blob struct {
	SyncedAgentAccountID             string
	CredentialVersion, KeyGeneration int64
	Ciphertext                       []byte
}

// AgentAccountMetadata carries the registry state used when receiving a missing local row.
type AgentAccountMetadata struct {
	SyncedAgentAccountID, Provider, Label, DesiredStatus string
	Priority                                             int
	IsInScope                                            bool
}

// LocalCredential distinguishes independently added credentials from received copies.
type LocalCredential struct {
	LocalID                 string
	SyncedCredentialVersion int64
	IsAuthInvalid           bool
}

// Remote is the opaque cloud storage and registry boundary.
type Remote interface {
	ListDevices(context.Context) ([]Device, error)
	GetKeySeal(context.Context) (int64, []byte, error)
	PutKeySeals(context.Context, int64, int64, []KeySeal) error
	GetBlob(context.Context, string) (Blob, error)
	PutBlob(context.Context, int64, Blob) error
	ListAgentAccounts(context.Context) ([]AgentAccountMetadata, error)
	RollKey(context.Context, int64, int64, []KeySeal, []Blob) error
}

// Local installs credentials and links rows atomically with their received version.
// Install must refuse to overwrite a concurrently added independent working copy.
type Local interface {
	Lookup(context.Context, string) (LocalCredential, error)
	Read(context.Context, string) ([]byte, error)
	Install(context.Context, AgentAccountMetadata, []byte, int64) error
	SetVersion(context.Context, string, int64) error
}

// Trust resolves locally pinned signing identities, never a key supplied by bosso alone.
type Trust interface {
	ApprovalCache
	CredentialSealCache
	RotationCache
	PinnedIdentity(string) (ed25519.PublicKey, error)
}

// ApprovalCache persists authenticated enrollment evidence locally. It allows a
// surviving device to retain previously verified chains after an approver is
// revoked without admitting fresh signatures from that revoked identity.
type ApprovalCache interface {
	ApprovedDevice(string) (Device, error)
	SaveApprovedDevice(Device) error
}

// CredentialSealCache pins accepted generations and seals in durable local storage.
// Highest generation is advanced before a key rotation is uploaded so a crash
// cannot allow the cloud to replay an older generation afterward.
type CredentialSealCache interface {
	AcceptedKeySeal(int64) ([]byte, error)
	SaveAcceptedKeySeal(int64, []byte) error
	HighestKeyGeneration() (int64, error)
}

// KeyRotation is a durable opaque compare-and-set proposal. OwnSeal allows the
// local trust store to pin the generation atomically with its retry intent.
type KeyRotation struct {
	ExpectedGeneration int64
	KeyGeneration      int64
	OwnSeal            []byte
	Seals              []KeySeal
	Blobs              []Blob
}

// RotationCache retains an interrupted upload until its remote commit is verified.
type RotationCache interface {
	PendingRotation() (*KeyRotation, error)
	SavePendingRotation(KeyRotation) error
	ClearPendingRotation() error
}

// Config identifies this enrolled daemon and its locally pinned bootstrap anchor.
type Config struct {
	UserID, DaemonID, BootstrapDaemonID string
	IdentityKey                         ed25519.PrivateKey
	DeviceKey                           *ecdh.PrivateKey
}
