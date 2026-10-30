package server

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/devicekey"
)

// CredentialDeviceUpstream is the daemon-authenticated enrollment transport.
type CredentialDeviceUpstream interface {
	RequestAgentCredentialDeviceEnrollment(context.Context, *connect.Request[pb.RequestAgentCredentialDeviceEnrollmentRequest]) (*connect.Response[pb.RequestAgentCredentialDeviceEnrollmentResponse], error)
	ListAgentCredentialDevices(context.Context, *connect.Request[pb.ListAgentCredentialDevicesRequest]) (*connect.Response[pb.ListAgentCredentialDevicesResponse], error)
	ApproveAgentCredentialDevice(context.Context, *connect.Request[pb.ApproveAgentCredentialDeviceRequest]) (*connect.Response[pb.ApproveAgentCredentialDeviceResponse], error)
	RevokeAgentCredentialDevice(context.Context, *connect.Request[pb.RevokeAgentCredentialDeviceRequest]) (*connect.Response[pb.RevokeAgentCredentialDeviceResponse], error)
}

// CredentialDeviceConfig provides local keyring access and a rotating daemon-session token.
type CredentialDeviceConfig struct {
	DaemonID       string
	IdentityKey    ed25519.PrivateKey
	LoadKey        func(string) (*ecdh.PrivateKey, error)
	RotateKey      func(string) (*ecdh.PrivateKey, error)
	Client         CredentialDeviceUpstream
	SessionToken   func() string
	PinIdentity    func(string, ed25519.PublicKey) error
	OnTrustChanged func(context.Context)
}

// CredentialDeviceService keeps private keys behind local keyring access.
type CredentialDeviceService struct {
	mu     sync.Mutex
	config CredentialDeviceConfig
}

// NewCredentialDeviceService constructs the local enrollment service.
func NewCredentialDeviceService(config CredentialDeviceConfig) *CredentialDeviceService {
	return &CredentialDeviceService{config: config}
}

func (d *CredentialDeviceService) authorize(header interface{ Set(string, string) }) {
	if d.config.SessionToken != nil {
		header.Set("Authorization", "Bearer "+d.config.SessionToken())
	}
}
func (d *CredentialDeviceService) list(ctx context.Context) ([]*pb.AgentCredentialDevice, error) {
	if d.config.Client == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("upstream is not configured"))
	}
	r := connect.NewRequest(&pb.ListAgentCredentialDevicesRequest{})
	d.authorize(r.Header())
	response, err := d.config.Client.ListAgentCredentialDevices(ctx, r)
	if err != nil {
		return nil, err
	}
	for _, entry := range response.Msg.Devices {
		entry.Fingerprint = devicekey.Fingerprint(ed25519.PublicKey(entry.IdentityPublicKey), entry.DevicePublicKey)
	}
	return response.Msg.Devices, nil
}

// EnsureEnrollment runs on every upstream connect; errors are retryable on the next connect.
func (d *CredentialDeviceService) EnsureEnrollment(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config.LoadKey == nil || len(d.config.IdentityKey) != ed25519.PrivateKeySize {
		return errors.New("device identity is unavailable")
	}
	key, err := d.config.LoadKey(d.config.DaemonID)
	if err != nil {
		return err
	}
	devices, err := d.list(ctx)
	if err != nil {
		return err
	}
	rotated := false
	for _, entry := range devices {
		if entry.DaemonId != d.config.DaemonID {
			continue
		}
		switch entry.Status {
		case "enrolled":
			if subtle.ConstantTimeCompare(key.PublicKey().Bytes(), entry.DevicePublicKey) == 1 {
				return nil
			}
		case "pending":
			if subtle.ConstantTimeCompare(key.PublicKey().Bytes(), entry.DevicePublicKey) == 1 && entry.ExpiresAt != nil && time.Now().Before(entry.ExpiresAt.AsTime()) {
				return nil
			}
		case "revoked":
			if d.config.RotateKey == nil {
				return errors.New("device key rotation is unavailable")
			}
			// Only rotate the revoked public key: a prior request failure may already have rotated it.
			if subtle.ConstantTimeCompare(key.PublicKey().Bytes(), entry.DevicePublicKey) == 1 {
				rotated = true
				key, err = d.config.RotateKey(d.config.DaemonID)
				if err != nil {
					return err
				}
			}
		}
	}
	err = d.requestEnrollment(ctx, key)
	if rotated || !isCredentialDeviceKeyRevoked(err) {
		return err
	}
	if d.config.RotateKey == nil {
		return errors.New("device key rotation is unavailable")
	}
	key, err = d.config.RotateKey(d.config.DaemonID)
	if err != nil {
		return err
	}
	return d.requestEnrollment(ctx, key)
}

func isCredentialDeviceKeyRevoked(err error) bool {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeFailedPrecondition {
		return false
	}
	for _, detail := range connectErr.Details() {
		value, err := detail.Value()
		if err != nil {
			continue
		}
		if _, ok := value.(*pb.AgentCredentialDeviceKeyRevoked); ok {
			return true
		}
	}
	return false
}

func (d *CredentialDeviceService) requestEnrollment(ctx context.Context, key *ecdh.PrivateKey) error {
	signature, err := devicekey.SignEnrollmentRequest(d.config.IdentityKey, devicekey.EnrollmentRequest{DaemonID: d.config.DaemonID, DevicePub: key.PublicKey().Bytes()})
	if err != nil {
		return err
	}
	bootstrapApproval, err := devicekey.SignApproval(d.config.IdentityKey, devicekey.Approval{ApproverDaemonID: d.config.DaemonID, DaemonID: d.config.DaemonID, DevicePub: key.PublicKey().Bytes()})
	if err != nil {
		return err
	}
	request := connect.NewRequest(&pb.RequestAgentCredentialDeviceEnrollmentRequest{DevicePublicKey: key.PublicKey().Bytes(), RequestSignature: signature, BootstrapApprovalSignature: bootstrapApproval})
	d.authorize(request.Header())
	_, err = d.config.Client.RequestAgentCredentialDeviceEnrollment(ctx, request)
	return err
}

func (s *Server) GetAgentCredentialDeviceStatus(ctx context.Context, _ *connect.Request[pb.GetAgentCredentialDeviceStatusRequest]) (*connect.Response[pb.GetAgentCredentialDeviceStatusResponse], error) {
	d := s.credentialDevices
	if d == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("device enrollment is not configured"))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config.LoadKey == nil || len(d.config.IdentityKey) != ed25519.PrivateKeySize {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("device identity is unavailable"))
	}
	key, err := d.config.LoadKey(d.config.DaemonID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("device key unavailable"))
	}
	identity := ed25519.PublicKey(append([]byte(nil), d.config.IdentityKey[ed25519.SeedSize:]...))
	device := &pb.AgentCredentialDevice{DaemonId: d.config.DaemonID, Status: "unenrolled"}
	devices, err := d.list(ctx)
	if err != nil {
		return nil, err
	}
	for _, entry := range devices {
		if entry.DaemonId == d.config.DaemonID {
			device = entry
			if subtle.ConstantTimeCompare(key.PublicKey().Bytes(), entry.DevicePublicKey) != 1 {
				device.Status = "unenrolled"
			}
			break
		}
	}
	// The new machine's display is the out-of-band trust anchor: never display bosso's key as this machine's.
	device.IdentityPublicKey = identity
	device.DevicePublicKey = key.PublicKey().Bytes()
	device.Fingerprint = devicekey.Fingerprint(identity, device.DevicePublicKey)
	return connect.NewResponse(&pb.GetAgentCredentialDeviceStatusResponse{Device: device}), nil
}
func (s *Server) ListAgentCredentialDevices(ctx context.Context, _ *connect.Request[pb.ListAgentCredentialDevicesRequest]) (*connect.Response[pb.ListAgentCredentialDevicesResponse], error) {
	if s.credentialDevices == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("device enrollment is not configured"))
	}
	devices, err := s.credentialDevices.list(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.ListAgentCredentialDevicesResponse{Devices: devices}), nil
}
func normalizeDeviceFingerprint(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToUpper(value))
}
func (s *Server) ApproveAgentCredentialDevice(ctx context.Context, req *connect.Request[pb.ApproveAgentCredentialDeviceRequest]) (*connect.Response[pb.ApproveAgentCredentialDeviceResponse], error) {
	d := s.credentialDevices
	if d == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("device enrollment is not configured"))
	}
	devices, err := d.list(ctx)
	if err != nil {
		return nil, err
	}
	for _, entry := range devices {
		if entry.DaemonId != req.Msg.DaemonId {
			continue
		}
		actual := normalizeDeviceFingerprint(entry.Fingerprint)
		typed := normalizeDeviceFingerprint(req.Msg.Fingerprint)
		if typed == "" || subtle.ConstantTimeCompare([]byte(actual), []byte(typed)) != 1 {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("fingerprint does not match — do not approve"))
		}
		if d.config.PinIdentity != nil {
			if err := d.config.PinIdentity(entry.DaemonId, ed25519.PublicKey(entry.IdentityPublicKey)); err != nil {
				return nil, connect.NewError(connect.CodeInternal, errors.New("could not persist verified device identity"))
			}
		}
		// Confirming an already enrolled device pins its identity on this daemon.
		// This lets a newly approved device verify the bootstrap chain out of band.
		if entry.Status == "enrolled" {
			if d.config.OnTrustChanged != nil {
				d.config.OnTrustChanged(ctx)
			}
			return connect.NewResponse(&pb.ApproveAgentCredentialDeviceResponse{}), nil
		}
		signature, err := devicekey.SignApproval(d.config.IdentityKey, devicekey.Approval{ApproverDaemonID: d.config.DaemonID, DaemonID: entry.DaemonId, DevicePub: entry.DevicePublicKey})
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		request := connect.NewRequest(&pb.ApproveAgentCredentialDeviceRequest{DaemonId: entry.DaemonId, ApprovalSignature: signature})
		d.authorize(request.Header())
		response, err := d.config.Client.ApproveAgentCredentialDevice(ctx, request)
		if err == nil && d.config.OnTrustChanged != nil {
			d.config.OnTrustChanged(ctx)
		}
		return response, err
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("device not found"))
}
func (s *Server) RevokeAgentCredentialDevice(ctx context.Context, req *connect.Request[pb.RevokeAgentCredentialDeviceRequest]) (*connect.Response[pb.RevokeAgentCredentialDeviceResponse], error) {
	d := s.credentialDevices
	if d == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("device enrollment is not configured"))
	}
	signature, err := devicekey.SignRevocation(d.config.IdentityKey, devicekey.Revocation{RevokerDaemonID: d.config.DaemonID, DaemonID: req.Msg.DaemonId})
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	request := connect.NewRequest(&pb.RevokeAgentCredentialDeviceRequest{DaemonId: req.Msg.DaemonId, RevocationSignature: signature})
	d.authorize(request.Header())
	return d.config.Client.RevokeAgentCredentialDevice(ctx, request)
}
