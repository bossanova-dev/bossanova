package credsync

// CredentialRecipient is a recipient to which a credential key may be sealed.
// A future cloud implementation must use a separate key service whose decrypt
// policy admits only sandbox workload identity with matching user/account/version
// context. Bosso's identity must have no decrypt permission; cloud use requires an
// explicit per-account opt-in. DeviceRecipient is the only implementation today.
type CredentialRecipient interface {
	RecipientID() string
	SealCredentialKey(ck []byte, rc RecipientContext) ([]byte, error)
}

// RecipientContext scopes a sealed credential key.
type RecipientContext struct {
	UserID               string
	KeyGeneration        int64
	SyncedAgentAccountID string
}

// DeviceRecipient seals to an already verified enrolled device.
type DeviceRecipient struct {
	DaemonID  string
	PublicKey []byte
}

func (d DeviceRecipient) RecipientID() string { return d.DaemonID }
func (d DeviceRecipient) SealCredentialKey(ck []byte, rc RecipientContext) ([]byte, error) {
	return SealCredentialKey(d.PublicKey, ck, rc.UserID, rc.KeyGeneration)
}
