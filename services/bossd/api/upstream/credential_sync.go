package upstream

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossd/internal/credsync"
)

// CredentialRemote transports only opaque ciphertext and public enrollment evidence.
type CredentialRemote struct {
	Client   bossanovav1connect.OrchestratorServiceClient
	Token    func() string
	DaemonID string
}

func credentialRequest[T any](r *CredentialRemote, v *T) *connect.Request[T] {
	q := connect.NewRequest(v)
	q.Header().Set("Authorization", "Bearer "+r.Token())
	return q
}
func credentialError(err error) error {
	if connect.CodeOf(err) == connect.CodeAborted {
		return errors.Join(credsync.ErrConflict, err)
	}
	if connect.CodeOf(err) == connect.CodeNotFound {
		return errors.Join(credsync.ErrNotFound, err)
	}
	return err
}
func (r *CredentialRemote) ListDevices(ctx context.Context) ([]credsync.Device, error) {
	out, err := r.Client.ListAgentCredentialDevices(ctx, credentialRequest(r, &pb.ListAgentCredentialDevicesRequest{}))
	if err != nil {
		return nil, credentialError(err)
	}
	var result []credsync.Device
	for _, d := range out.Msg.Devices {
		result = append(result, credsync.Device{DaemonID: d.DaemonId, Status: d.Status, ApprovedByDaemonID: d.ApprovedByDaemonId, DevicePublicKey: d.DevicePublicKey, IdentityPublicKey: d.IdentityPublicKey, ApprovalSignature: d.ApprovalSignature})
	}
	return result, nil
}
func (r *CredentialRemote) GetKeySeal(ctx context.Context) (int64, []byte, error) {
	out, err := r.Client.GetAgentCredentialKeySeal(ctx, credentialRequest(r, &pb.GetAgentCredentialKeySealRequest{}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return 0, nil, nil
		}
		return 0, nil, credentialError(err)
	}
	return out.Msg.KeyGeneration, out.Msg.SealedCredentialKey, nil
}
func credentialSeals(seals []credsync.KeySeal) []*pb.AgentCredentialKeySeal {
	out := make([]*pb.AgentCredentialKeySeal, 0, len(seals))
	for _, s := range seals {
		out = append(out, &pb.AgentCredentialKeySeal{DaemonId: s.DaemonID, SealedCredentialKey: s.SealedCredentialKey})
	}
	return out
}
func credentialBlob(b credsync.Blob) *pb.AgentCredentialBlob {
	return &pb.AgentCredentialBlob{SyncedAgentAccountId: b.SyncedAgentAccountID, CredentialVersion: b.CredentialVersion, KeyGeneration: b.KeyGeneration, Ciphertext: b.Ciphertext}
}
func (r *CredentialRemote) PutKeySeals(ctx context.Context, expected, generation int64, seals []credsync.KeySeal) error {
	_, err := r.Client.PutAgentCredentialKeySeals(ctx, credentialRequest(r, &pb.PutAgentCredentialKeySealsRequest{ExpectedKeyGeneration: expected, KeyGeneration: generation, Seals: credentialSeals(seals)}))
	return credentialError(err)
}
func (r *CredentialRemote) GetBlob(ctx context.Context, id string) (credsync.Blob, error) {
	out, err := r.Client.GetAgentCredentialBlob(ctx, credentialRequest(r, &pb.GetAgentCredentialBlobRequest{SyncedAgentAccountId: id}))
	if err != nil {
		return credsync.Blob{}, credentialError(err)
	}
	b := out.Msg.GetBlob()
	return credsync.Blob{SyncedAgentAccountID: b.GetSyncedAgentAccountId(), CredentialVersion: b.GetCredentialVersion(), KeyGeneration: b.GetKeyGeneration(), Ciphertext: b.GetCiphertext()}, nil
}
func (r *CredentialRemote) PutBlob(ctx context.Context, expected int64, b credsync.Blob) error {
	_, err := r.Client.PutAgentCredentialBlob(ctx, credentialRequest(r, &pb.PutAgentCredentialBlobRequest{ExpectedVersion: expected, Blob: credentialBlob(b)}))
	return credentialError(err)
}
func (r *CredentialRemote) ListAgentAccounts(ctx context.Context) ([]credsync.AgentAccountMetadata, error) {
	out, err := r.Client.ListSyncedAgentAccounts(ctx, credentialRequest(r, &pb.ListSyncedAgentAccountsRequest{}))
	if err != nil {
		return nil, credentialError(err)
	}
	var result []credsync.AgentAccountMetadata
	for _, a := range out.Msg.Accounts {
		scope := a.SyncScope == "all_daemons"
		for _, m := range a.Members {
			if m.DaemonId == r.DaemonID && m.IsSelected {
				scope = true
			}
		}
		result = append(result, credsync.AgentAccountMetadata{SyncedAgentAccountID: a.Id, Provider: a.Provider, Label: a.Label, DesiredStatus: a.DesiredStatus, Priority: int(a.Priority), IsInScope: scope})
	}
	return result, nil
}
func (r *CredentialRemote) RollKey(ctx context.Context, expected, generation int64, seals []credsync.KeySeal, blobs []credsync.Blob) error {
	p := &pb.RollAgentCredentialKeyRequest{ExpectedKeyGeneration: expected, KeyGeneration: generation, Seals: credentialSeals(seals)}
	for _, b := range blobs {
		p.Blobs = append(p.Blobs, credentialBlob(b))
	}
	_, err := r.Client.RollAgentCredentialKey(ctx, credentialRequest(r, p))
	return credentialError(err)
}

// AcquireRefresh owns rotating Codex refreshes for the server's bounded lease.
func (r *CredentialRemote) AcquireRefresh(ctx context.Context, id string, expected int64, fingerprint []byte, attemptID string) (bool, error) {
	out, err := r.Client.AcquireAgentCredentialRefreshLease(ctx, credentialRequest(r, &pb.AcquireAgentCredentialRefreshLeaseRequest{SyncedAgentAccountId: id, ExpectedCredentialVersion: expected, RefreshTokenFingerprint: fingerprint, RefreshAttemptId: attemptID}))
	if err != nil {
		return false, credentialError(err)
	}
	return out.Msg.IsAcquired, nil
}

// ReleaseRefresh relinquishes refresh ownership after publishing the updated blob.
func (r *CredentialRemote) ReleaseRefresh(ctx context.Context, id string) error {
	_, err := r.Client.ReleaseAgentCredentialRefreshLease(ctx, credentialRequest(r, &pb.ReleaseAgentCredentialRefreshLeaseRequest{SyncedAgentAccountId: id}))
	return credentialError(err)
}
