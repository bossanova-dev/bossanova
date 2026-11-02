package server

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/devicekey"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type readinessPinStore struct {
	mu   sync.Mutex
	pins map[string]ed25519.PublicKey
}

func (p *readinessPinStore) PinnedIdentity(id string) (ed25519.PublicKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if key, ok := p.pins[id]; ok {
		return key, nil
	}
	return nil, errors.New("not pinned")
}
func (p *readinessPinStore) Pin(id string, key ed25519.PublicKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pins[id] = key
	return nil
}

type readinessFixture struct {
	rpc         *credentialDeviceRPCSpy
	pins        *readinessPinStore
	now         time.Time
	logs        bytes.Buffer
	service     *CredentialDeviceService
	fingerprint string
	ownKey      []byte
	bootPub     ed25519.PublicKey
}

func newReadinessFixture(t *testing.T) *readinessFixture {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bootPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &readinessFixture{rpc: &credentialDeviceRPCSpy{}, pins: &readinessPinStore{pins: map[string]ed25519.PublicKey{}}, now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), ownKey: key.PublicKey().Bytes(), bootPub: bootPub}
	f.fingerprint = devicekey.Fingerprint(pub, f.ownKey)
	logger := zerolog.New(&f.logs)
	f.service = NewCredentialDeviceService(CredentialDeviceConfig{
		DaemonID: "self", IdentityKey: private, Client: f.rpc,
		LoadKey:        func(string) (*ecdh.PrivateKey, error) { return key, nil },
		PinIdentity:    f.pins.Pin,
		PinnedIdentity: f.pins.PinnedIdentity,
		Now:            func() time.Time { return f.now },
		Logger:         &logger,
	})
	return f
}

func (f *readinessFixture) setDevices(self *pb.AgentCredentialDevice) {
	f.rpc.devices = []*pb.AgentCredentialDevice{{DaemonId: "boot", Status: "enrolled", ApprovedByDaemonId: "boot", IdentityPublicKey: f.bootPub, DevicePublicKey: []byte("boot-device")}}
	if self != nil {
		f.rpc.devices = append(f.rpc.devices, self)
	}
}

func (f *readinessFixture) status(t *testing.T) *pb.CredentialDeviceReadiness {
	t.Helper()
	s := &Server{credentialDevices: f.service}
	response, err := s.GetAgentCredentialDeviceStatus(context.Background(), connect.NewRequest(&pb.GetAgentCredentialDeviceStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if response.Msg.GetReadiness() == nil {
		t.Fatal("status response has no readiness block")
	}
	return response.Msg.GetReadiness()
}

func TestDeviceStatusReturnsReadiness(t *testing.T) {
	f := newReadinessFixture(t)
	f.setDevices(&pb.AgentCredentialDevice{DaemonId: "self", Status: "enrolled", ApprovedByDaemonId: "boot", DevicePublicKey: f.ownKey})
	got := f.status(t)
	if got.GetState() != "bootstrap_unpinned" || got.GetBootstrapDaemonId() != "boot" || !slices.Equal(got.GetApproverDaemonIds(), []string{"boot"}) {
		t.Fatalf("readiness=%v", got)
	}
	if !got.GetCheckedAt().AsTime().Equal(f.now) {
		t.Errorf("checked_at=%v", got.GetCheckedAt())
	}
	steps := got.GetRemediationSteps()
	if len(steps) != 2 || steps[0].GetMachineDaemonId() != "boot" || steps[0].GetIsThisMachine() || !steps[1].GetIsThisMachine() {
		t.Fatalf("steps=%v", steps)
	}
	for _, step := range steps {
		if strings.Contains(step.GetCommand(), devicekey.Fingerprint(f.bootPub, []byte("boot-device"))) {
			t.Errorf("bootstrap fingerprint pre-filled: %q", step.GetCommand())
		}
	}
}

func TestDeviceStatusExpiredCarriesLocalFingerprint(t *testing.T) {
	f := newReadinessFixture(t)
	f.setDevices(&pb.AgentCredentialDevice{DaemonId: "self", Status: "pending", DevicePublicKey: f.ownKey, ExpiresAt: timestamppb.New(f.now.Add(-time.Minute))})
	got := f.status(t)
	if got.GetState() != "enrollment_expired" || !got.GetExpiresAt().AsTime().Equal(f.now.Add(-time.Minute)) {
		t.Fatalf("readiness=%v", got)
	}
	steps := got.GetRemediationSteps()
	if len(steps) != 2 || steps[0].GetCommand() != "boss account devices request" || steps[1].GetCommand() != "boss account devices approve self --fingerprint "+f.fingerprint {
		t.Fatalf("steps=%v", steps)
	}
}

func TestDeviceStatusRefreshesOnlyStaleReadiness(t *testing.T) {
	f := newReadinessFixture(t)
	f.setDevices(&pb.AgentCredentialDevice{DaemonId: "self", Status: "pending", DevicePublicKey: f.ownKey, ExpiresAt: timestamppb.New(f.now.Add(time.Hour))})
	if got := f.status(t).GetState(); got != "enrollment_pending" {
		t.Fatalf("first=%q", got)
	}
	f.setDevices(&pb.AgentCredentialDevice{DaemonId: "self", Status: "revoked", DevicePublicKey: f.ownKey})
	f.now = f.now.Add(DefaultCredentialReadinessInterval)
	if got := f.status(t).GetState(); got != "enrollment_pending" {
		t.Fatalf("fresh cache was re-evaluated: %q", got)
	}
	f.now = f.now.Add(time.Second)
	if got := f.status(t).GetState(); got != "revoked" {
		t.Fatalf("stale cache was served: %q", got)
	}
}

func TestApproveBootstrapRefreshesReadiness(t *testing.T) {
	f := newReadinessFixture(t)
	f.setDevices(&pb.AgentCredentialDevice{DaemonId: "self", Status: "enrolled", ApprovedByDaemonId: "boot", DevicePublicKey: f.ownKey})
	if got := f.service.RefreshReadiness(context.Background()).State; got != "bootstrap_unpinned" {
		t.Fatalf("before approve=%q", got)
	}
	s := &Server{credentialDevices: f.service}
	fingerprint := devicekey.Fingerprint(f.bootPub, []byte("boot-device"))
	if _, err := s.ApproveAgentCredentialDevice(context.Background(), connect.NewRequest(&pb.ApproveAgentCredentialDeviceRequest{DaemonId: "boot", Fingerprint: fingerprint})); err != nil {
		t.Fatal(err)
	}
	if got := f.service.Readiness().State; got != "ready" {
		t.Fatalf("after approve=%q", got)
	}
}

func TestReadinessTransitionLogsOneWarning(t *testing.T) {
	f := newReadinessFixture(t)
	f.setDevices(&pb.AgentCredentialDevice{DaemonId: "self", Status: "pending", DevicePublicKey: f.ownKey, ExpiresAt: timestamppb.New(f.now.Add(time.Hour))})
	for range 3 {
		f.service.RefreshReadiness(context.Background())
	}
	if n := strings.Count(f.logs.String(), "credential sharing is not ready"); n != 1 {
		t.Fatalf("warnings after staying pending=%d: %s", n, f.logs.String())
	}
	if !strings.Contains(f.logs.String(), `"state":"enrollment_pending"`) || !strings.Contains(f.logs.String(), `"daemon_id":"self"`) || !strings.Contains(f.logs.String(), "approve self --fingerprint") {
		t.Errorf("warning lacks state, daemon or remediation: %s", f.logs.String())
	}
	f.now = f.now.Add(2 * time.Hour)
	f.service.RefreshReadiness(context.Background())
	f.service.RefreshReadiness(context.Background())
	if n := strings.Count(f.logs.String(), "credential sharing is not ready"); n != 2 {
		t.Fatalf("warnings after expiry=%d: %s", n, f.logs.String())
	}
}

func TestReadinessUnavailableAndUnknownNeverWarn(t *testing.T) {
	f := newReadinessFixture(t)
	f.service.config.PinnedIdentity = nil
	if got := f.service.RefreshReadiness(context.Background()); got.State != "unavailable" || len(got.RemediationSteps) != 0 {
		t.Fatalf("unconfigured=%+v", got)
	}
	f = newReadinessFixture(t)
	f.rpc.listErr = errors.New("bosso unreachable")
	if got := f.service.RefreshReadiness(context.Background()); got.State != "unknown" {
		t.Fatalf("list error=%q", got.State)
	}
	if f.logs.Len() != 0 {
		t.Fatalf("unexpected warning: %s", f.logs.String())
	}
}
