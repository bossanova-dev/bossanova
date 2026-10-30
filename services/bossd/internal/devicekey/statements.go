package devicekey

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
)

// EnrollmentRequest binds a requesting daemon to its encryption key.
type EnrollmentRequest struct {
	DaemonID  string
	DevicePub []byte
}

// Approval attests that an enrolled daemon approved a target encryption key.
type Approval struct {
	ApproverDaemonID string
	DaemonID         string
	DevicePub        []byte
}

// Revocation authenticates the revoker and the daemon whose key is revoked.
type Revocation struct {
	RevokerDaemonID string
	DaemonID        string
}

// SignEnrollmentRequest signs a canonical enrollment statement with the identity key.
func SignEnrollmentRequest(key ed25519.PrivateKey, request EnrollmentRequest) ([]byte, error) {
	return sign(key, message("enrollment-request", []byte(request.DaemonID), request.DevicePub))
}

// VerifyEnrollmentRequest checks an enrollment signature against a pinned identity.
func VerifyEnrollmentRequest(key ed25519.PublicKey, request EnrollmentRequest, signature []byte) bool {
	return verify(key, message("enrollment-request", []byte(request.DaemonID), request.DevicePub), signature)
}

// SignApproval signs approval of the target daemon and its encryption key.
func SignApproval(key ed25519.PrivateKey, approval Approval) ([]byte, error) {
	return sign(key, message("approval", []byte(approval.ApproverDaemonID), []byte(approval.DaemonID), approval.DevicePub))
}

// VerifyApproval checks approval against the approver's pinned identity key.
func VerifyApproval(key ed25519.PublicKey, approval Approval, signature []byte) bool {
	return verify(key, message("approval", []byte(approval.ApproverDaemonID), []byte(approval.DaemonID), approval.DevicePub), signature)
}

// SignRevocation signs the revoker and target daemon identifiers.
func SignRevocation(key ed25519.PrivateKey, revocation Revocation) ([]byte, error) {
	return sign(key, message("revocation", []byte(revocation.RevokerDaemonID), []byte(revocation.DaemonID)))
}

// VerifyRevocation checks revocation against the revoker's pinned identity key.
func VerifyRevocation(key ed25519.PublicKey, revocation Revocation, signature []byte) bool {
	return verify(key, message("revocation", []byte(revocation.RevokerDaemonID), []byte(revocation.DaemonID)), signature)
}

// Each field is length-prefixed so different field boundaries cannot alias.
func message(kind string, fields ...[]byte) []byte {
	encoded := []byte("bossanova agent-credential " + kind + " v1\x00")
	for _, field := range fields {
		encoded = binary.BigEndian.AppendUint32(encoded, uint32(len(field)))
		encoded = append(encoded, field...)
	}
	return encoded
}
func sign(key ed25519.PrivateKey, message []byte) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("device key statement: invalid identity private key")
	}
	return ed25519.Sign(key, message), nil
}
func verify(key ed25519.PublicKey, message, signature []byte) bool {
	return len(key) == ed25519.PublicKeySize && ed25519.Verify(key, message, signature)
}
