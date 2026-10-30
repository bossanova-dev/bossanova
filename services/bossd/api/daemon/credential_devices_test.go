package server

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/devicekey"
)

type credentialDeviceRPCSpy struct {
	CredentialDeviceUpstream
	devices       []*pb.AgentCredentialDevice
	approvals     int
	signature     []byte
	enrollments   int
	enrollment    *pb.RequestAgentCredentialDeviceEnrollmentRequest
	revokedKeys   map[string]bool
	rejectAllKeys bool
	enrollmentErr error
}

func (f *credentialDeviceRPCSpy) ListAgentCredentialDevices(context.Context, *connect.Request[pb.ListAgentCredentialDevicesRequest]) (*connect.Response[pb.ListAgentCredentialDevicesResponse], error) {
	return connect.NewResponse(&pb.ListAgentCredentialDevicesResponse{Devices: f.devices}), nil
}
func (f *credentialDeviceRPCSpy) ApproveAgentCredentialDevice(_ context.Context, r *connect.Request[pb.ApproveAgentCredentialDeviceRequest]) (*connect.Response[pb.ApproveAgentCredentialDeviceResponse], error) {
	f.approvals++
	f.signature = r.Msg.ApprovalSignature
	return connect.NewResponse(&pb.ApproveAgentCredentialDeviceResponse{}), nil
}
func TestApproveCredentialDeviceFingerprint_Accounts_AC21(t *testing.T) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	targetPub := make([]byte, 32)
	targetPub[0] = 7
	expected := devicekey.Fingerprint(pub, targetPub)
	for _, tc := range []struct {
		name, typed string
		want        connect.Code
	}{
		{"normalized", strings.ToLower(strings.ReplaceAll(expected, "-", " ")), 0},
		{"mismatch", "AAAA-AAAA-AAAA-AAAA", connect.CodeFailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := &credentialDeviceRPCSpy{devices: []*pb.AgentCredentialDevice{{DaemonId: "new", IdentityPublicKey: pub, DevicePublicKey: targetPub, Status: "pending"}}}
			s := &Server{credentialDevices: NewCredentialDeviceService(CredentialDeviceConfig{DaemonID: "approver", IdentityKey: private, Client: rpc})}
			_, err := s.ApproveAgentCredentialDevice(context.Background(), connect.NewRequest(&pb.ApproveAgentCredentialDeviceRequest{DaemonId: "new", Fingerprint: tc.typed}))
			if (tc.want == 0 && err != nil) || (tc.want != 0 && connect.CodeOf(err) != tc.want) {
				t.Fatalf("error %v", err)
			}
			if tc.want != 0 {
				if rpc.approvals != 0 {
					t.Fatal("mismatch sent approval")
				}
				return
			}
			if rpc.approvals != 1 || !devicekey.VerifyApproval(pub, devicekey.Approval{ApproverDaemonID: "approver", DaemonID: "new", DevicePub: targetPub}, rpc.signature) {
				t.Fatal("approval was not bound to the fingerprint's key")
			}
		})
	}
}

func TestDeviceStatusUsesLocalKeys_Accounts_AC20(t *testing.T) {
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	remoteKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	rpc := &credentialDeviceRPCSpy{devices: []*pb.AgentCredentialDevice{{DaemonId: "local", IdentityPublicKey: pub, DevicePublicKey: remoteKey.PublicKey().Bytes(), Status: "enrolled"}}}
	s := &Server{credentialDevices: NewCredentialDeviceService(CredentialDeviceConfig{DaemonID: "local", IdentityKey: private, Client: rpc, LoadKey: func(string) (*ecdh.PrivateKey, error) { return key, nil }})}
	response, err := s.GetAgentCredentialDeviceStatus(context.Background(), connect.NewRequest(&pb.GetAgentCredentialDeviceStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if response.Msg.Device.Fingerprint != devicekey.Fingerprint(pub, key.PublicKey().Bytes()) {
		t.Fatal("status trusted substituted upstream key")
	}
	if response.Msg.Device.Status == "enrolled" {
		t.Fatal("status trusted enrollment of a different key")
	}
}

func (f *credentialDeviceRPCSpy) RequestAgentCredentialDeviceEnrollment(_ context.Context, r *connect.Request[pb.RequestAgentCredentialDeviceEnrollmentRequest]) (*connect.Response[pb.RequestAgentCredentialDeviceEnrollmentResponse], error) {
	f.enrollments++
	f.enrollment = r.Msg
	if r.Header().Get("Authorization") != "Bearer daemon-session" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing daemon bearer"))
	}
	if f.enrollmentErr != nil {
		return nil, f.enrollmentErr
	}
	if f.rejectAllKeys || f.revokedKeys[string(r.Msg.DevicePublicKey)] {
		err := connect.NewError(connect.CodeFailedPrecondition, errors.New("device key revoked"))
		detail, detailErr := connect.NewErrorDetail(&pb.AgentCredentialDeviceKeyRevoked{})
		if detailErr != nil {
			return nil, detailErr
		}
		err.AddDetail(detail)
		return nil, err
	}
	return connect.NewResponse(&pb.RequestAgentCredentialDeviceEnrollmentResponse{}), nil
}
func TestEnsureCredentialDeviceEnrollment_Accounts_AC20_AC22(t *testing.T) {
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	for _, tc := range []struct {
		status          string
		expiry          time.Time
		want, rotations int
	}{
		{"", time.Time{}, 1, 0}, {"enrolled", time.Time{}, 0, 0},
		{"pending", time.Now().Add(time.Hour), 0, 0}, {"expired", time.Now().Add(-time.Hour), 1, 0},
		{"pending", time.Now().Add(-time.Hour), 1, 0}, {"revoked", time.Time{}, 1, 1},
	} {
		t.Run(tc.status+tc.expiry.String(), func(t *testing.T) {
			key, _ := ecdh.X25519().GenerateKey(rand.Reader)
			rpc := &credentialDeviceRPCSpy{}
			if tc.status != "" {
				rpc.devices = []*pb.AgentCredentialDevice{{DaemonId: "self", Status: tc.status, DevicePublicKey: key.PublicKey().Bytes(), ExpiresAt: timestamppb.New(tc.expiry)}}
			}
			rotations := 0
			service := NewCredentialDeviceService(CredentialDeviceConfig{DaemonID: "self", IdentityKey: private, Client: rpc, SessionToken: func() string { return "daemon-session" },
				LoadKey: func(string) (*ecdh.PrivateKey, error) { return key, nil },
				RotateKey: func(string) (*ecdh.PrivateKey, error) {
					rotations++
					var err error
					key, err = ecdh.X25519().GenerateKey(rand.Reader)
					return key, err
				},
			})
			if err := service.EnsureEnrollment(context.Background()); err != nil {
				t.Fatal(err)
			}
			if rpc.enrollments != tc.want || rotations != tc.rotations {
				t.Fatalf("requests=%d rotations=%d", rpc.enrollments, rotations)
			}
			if tc.want == 1 {
				r := rpc.enrollment
				if !devicekey.VerifyEnrollmentRequest(pub, devicekey.EnrollmentRequest{DaemonID: "self", DevicePub: key.PublicKey().Bytes()}, r.RequestSignature) {
					t.Fatal("invalid enrollment evidence")
				}
				if !devicekey.VerifyApproval(pub, devicekey.Approval{ApproverDaemonID: "self", DaemonID: "self", DevicePub: key.PublicKey().Bytes()}, r.BootstrapApprovalSignature) {
					t.Fatal("bootstrap has no signed approval")
				}
			}
		})
	}
}

func TestApproveEnrolledDevicePinsVerifiedBootstrap(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rpc := &credentialDeviceRPCSpy{devices: []*pb.AgentCredentialDevice{{DaemonId: "root", IdentityPublicKey: public, DevicePublicKey: key.PublicKey().Bytes(), Status: "enrolled"}}}
	pinned := false
	service := NewCredentialDeviceService(CredentialDeviceConfig{DaemonID: "new", IdentityKey: private, Client: rpc, PinIdentity: func(id string, pub ed25519.PublicKey) error {
		if id != "root" || !pub.Equal(public) {
			t.Fatal("wrong identity pin")
		}
		pinned = true
		return nil
	}})
	s := &Server{credentialDevices: service}
	_, err = s.ApproveAgentCredentialDevice(context.Background(), connect.NewRequest(&pb.ApproveAgentCredentialDeviceRequest{DaemonId: "root", Fingerprint: "wrong"}))
	if err == nil || pinned {
		t.Fatal("unverified identity pinned")
	}
	_, err = s.ApproveAgentCredentialDevice(context.Background(), connect.NewRequest(&pb.ApproveAgentCredentialDeviceRequest{DaemonId: "root", Fingerprint: devicekey.Fingerprint(public, key.PublicKey().Bytes())}))
	if err != nil {
		t.Fatal(err)
	}
	if !pinned || rpc.approvals != 0 {
		t.Fatal("bootstrap confirmation did not stay local")
	}
}

func TestEnsureHiddenRevokedCredentialDevice_Accounts_AC29(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                                              string
		plainError, rejectAll, noRotation, visibleRevoked bool
		wantRequests, wantRotations                       int
	}{
		{name: "recover", wantRequests: 2, wantRotations: 1},
		{name: "plain precondition", plainError: true, wantRequests: 1},
		{name: "all keys revoked", rejectAll: true, wantRequests: 2, wantRotations: 1},
		{name: "rotation unavailable", noRotation: true, wantRequests: 1},
		{name: "visible revoked retry bounded", visibleRevoked: true, rejectAll: true, wantRequests: 1, wantRotations: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			oldPublic := key.PublicKey().Bytes()
			// Account B cannot see account A's revoked device row.
			rpc := &credentialDeviceRPCSpy{revokedKeys: map[string]bool{string(oldPublic): true}, rejectAllKeys: tc.rejectAll}
			if tc.visibleRevoked {
				rpc.devices = []*pb.AgentCredentialDevice{{DaemonId: "self", DevicePublicKey: oldPublic, Status: "revoked"}}
			}
			if tc.plainError {
				rpc.enrollmentErr = connect.NewError(connect.CodeFailedPrecondition, errors.New("identity not pinned"))
			}
			rotations := 0
			config := CredentialDeviceConfig{
				DaemonID: "self", IdentityKey: private, Client: rpc,
				SessionToken: func() string { return "daemon-session" },
				LoadKey:      func(string) (*ecdh.PrivateKey, error) { return key, nil },
				RotateKey: func(id string) (*ecdh.PrivateKey, error) {
					if id != "self" {
						t.Fatalf("rotated daemon %q", id)
					}
					rotations++
					var err error
					key, err = ecdh.X25519().GenerateKey(rand.Reader)
					return key, err
				},
			}
			if tc.noRotation {
				config.RotateKey = nil
			}
			err = NewCredentialDeviceService(config).EnsureEnrollment(context.Background())
			if rpc.enrollments != tc.wantRequests || rotations != tc.wantRotations {
				t.Fatalf("requests=%d rotations=%d error=%v", rpc.enrollments, rotations, err)
			}
			switch {
			case tc.plainError:
				if !errors.Is(err, rpc.enrollmentErr) {
					t.Fatalf("error=%v, want original precondition", err)
				}
			case tc.rejectAll:
				if connect.CodeOf(err) != connect.CodeFailedPrecondition {
					t.Fatalf("second revoked-key error=%v", err)
				}
			case tc.noRotation:
				if err == nil || !strings.Contains(err.Error(), "device key rotation is unavailable") {
					t.Fatalf("rotation error=%v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				freshPublic := rpc.enrollment.DevicePublicKey
				if !bytes.Equal(freshPublic, key.PublicKey().Bytes()) || devicekey.Fingerprint(public, freshPublic) == devicekey.Fingerprint(public, oldPublic) {
					t.Fatal("enrollment did not use a fresh device key")
				}
				if !devicekey.VerifyEnrollmentRequest(public, devicekey.EnrollmentRequest{DaemonID: "self", DevicePub: freshPublic}, rpc.enrollment.RequestSignature) {
					t.Fatal("fresh enrollment signature is invalid")
				}
				if !devicekey.VerifyApproval(public, devicekey.Approval{ApproverDaemonID: "self", DaemonID: "self", DevicePub: freshPublic}, rpc.enrollment.BootstrapApprovalSignature) {
					t.Fatal("fresh bootstrap approval signature is invalid")
				}
			}
		})
	}
}
