package domain

import "time"

// ViewerDeviceApproval binds a locally approved viewer device to its public key.
type ViewerDeviceApproval struct {
	ViewerDeviceID string
	PublicKey      []byte
	Label          string
	ApprovedAt     time.Time
	CreatedAt      time.Time
}
