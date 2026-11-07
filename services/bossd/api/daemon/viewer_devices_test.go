package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/streamseal"
)

type viewerApprovalMemory struct {
	entries map[string]*domain.ViewerDeviceApproval
}

func (m *viewerApprovalMemory) Get(_ context.Context, id string) (*domain.ViewerDeviceApproval, error) {
	return m.entries[id], nil
}
func (m *viewerApprovalMemory) List(context.Context) ([]*domain.ViewerDeviceApproval, error) {
	out := []*domain.ViewerDeviceApproval{}
	for _, v := range m.entries {
		out = append(out, v)
	}
	return out, nil
}
func (m *viewerApprovalMemory) Create(_ context.Context, a *domain.ViewerDeviceApproval) error {
	if m.entries[a.ViewerDeviceID] != nil {
		return errors.New("duplicate")
	}
	m.entries[a.ViewerDeviceID] = a
	return nil
}
func (m *viewerApprovalMemory) Delete(_ context.Context, id string) error {
	delete(m.entries, id)
	return nil
}
func TestViewerApprovalRequiresCurrentFingerprint_TerminalEncryption_AC219101(t *testing.T) {
	for _, tc := range []struct {
		name, id, fp string
		changed      bool
		ok           bool
	}{
		{name: "match", id: "viewer", ok: true}, {name: "wrong", id: "viewer", fp: "WRONG"}, {name: "unknown", id: "unknown"}, {name: "changed", id: "viewer", changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := make([]byte, 32)
			key[0] = 1
			fp := streamseal.ViewerFingerprint("viewer", key)
			m := &viewerApprovalMemory{entries: map[string]*domain.ViewerDeviceApproval{}}
			requests := func(context.Context) ([]ViewerDeviceRequest, error) {
				return []ViewerDeviceRequest{{ViewerDeviceID: "viewer", PublicKey: key, Label: "untrusted"}}, nil
			}
			svc := NewViewerApprovals(m, requests, nil, nil)
			s := &Server{viewerApprovals: svc}
			if _, err := s.ListViewerDevices(context.Background(), connect.NewRequest(&pb.ListLocalViewerDevicesRequest{})); err != nil {
				t.Fatal(err)
			}
			if tc.changed {
				key[0] = 2
			}
			if tc.fp != "" {
				fp = tc.fp
			}
			_, err := s.ApproveViewerDevice(context.Background(), connect.NewRequest(&pb.ApproveViewerDeviceRequest{ViewerDeviceId: tc.id, Fingerprint: fp}))
			if tc.ok {
				if err != nil || m.entries["viewer"] == nil {
					t.Fatalf("approval: %v %#v", err, m.entries)
				}
			} else if connect.CodeOf(err) != connect.CodeFailedPrecondition || len(m.entries) != 0 {
				t.Fatalf("refusal: %v %#v", err, m.entries)
			}
		})
	}
}
func TestViewerRevocationDeletesAndNotifies_TerminalEncryption_AC219102(t *testing.T) {
	m := &viewerApprovalMemory{entries: map[string]*domain.ViewerDeviceApproval{"viewer": {ViewerDeviceID: "viewer"}}}
	svc := NewViewerApprovals(m, nil, nil, nil)
	ch, cancel := svc.Subscribe("viewer")
	defer cancel()
	other, stop := svc.Subscribe("other")
	defer stop()
	s := &Server{viewerApprovals: svc}
	if _, err := s.RevokeViewerDevice(context.Background(), connect.NewRequest(&pb.RevokeViewerDeviceRequest{ViewerDeviceId: "viewer"})); err != nil {
		t.Fatal(err)
	}
	if len(m.entries) != 0 {
		t.Fatal("approval retained")
	}
	select {
	case <-ch:
	default:
		t.Fatal("subscriber not notified")
	}
	select {
	case <-other:
		t.Fatal("unrelated viewer notified")
	default:
	}
}
func TestViewerStatusAvailability_TerminalEncryption_AC219103(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		key       ed25519.PrivateKey
		connected bool
		available bool
	}{{"no identity", nil, true, false}, {"disconnected", key, false, false}, {"connected", key, true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{viewerApprovals: NewViewerApprovals(nil, nil, tc.key, func() bool { return tc.connected })}
			r, err := s.GetViewerDeviceStatus(context.Background(), connect.NewRequest(&pb.GetViewerDeviceStatusRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if r.Msg.IsAvailable != tc.available {
				t.Fatalf("status %v", r.Msg)
			}
			if tc.available && r.Msg.DaemonFingerprint != streamseal.DaemonFingerprint(key.Public().(ed25519.PublicKey)) {
				t.Fatal("wrong fingerprint")
			}
		})
	}
}

func TestViewerListPreservesLocalPins(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 4
	m := &viewerApprovalMemory{entries: map[string]*domain.ViewerDeviceApproval{"gone": {ViewerDeviceID: "gone", PublicKey: key, Label: "local", ApprovedAt: time.Now()}, "changed": {ViewerDeviceID: "changed", PublicKey: key, Label: "pinned", ApprovedAt: time.Now()}}}
	changed := make([]byte, 32)
	changed[0] = 5
	svc := NewViewerApprovals(m, func(context.Context) ([]ViewerDeviceRequest, error) {
		return []ViewerDeviceRequest{{ViewerDeviceID: "pending", PublicKey: changed, Label: "browser"}, {ViewerDeviceID: "changed", PublicKey: changed, Label: "forged"}}, nil
	}, nil, nil)
	s := &Server{viewerApprovals: svc}
	r, err := s.ListViewerDevices(context.Background(), connect.NewRequest(&pb.ListLocalViewerDevicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Msg.Devices) != 3 {
		t.Fatalf("devices: %v", r.Msg.Devices)
	}
	for _, d := range r.Msg.Devices {
		if d.ViewerDeviceId == "pending" {
			if d.IsApproved {
				t.Fatal("registry authorized pending device")
			}
			continue
		}
		if !d.IsApproved || d.Fingerprint != streamseal.ViewerFingerprint(d.ViewerDeviceId, key) {
			t.Fatalf("lost pin: %v", d)
		}
	}
	_, err = s.ApproveViewerDevice(context.Background(), connect.NewRequest(&pb.ApproveViewerDeviceRequest{ViewerDeviceId: "changed", Fingerprint: streamseal.ViewerFingerprint("changed", changed)}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("replaced existing pin: %v", err)
	}
	if !bytes.Equal(m.entries["changed"].PublicKey, key) {
		t.Fatal("pin mutated")
	}
}

func TestViewerApprovalNormalizesAndCopiesKey(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 3
	m := &viewerApprovalMemory{entries: map[string]*domain.ViewerDeviceApproval{}}
	svc := NewViewerApprovals(m, func(context.Context) ([]ViewerDeviceRequest, error) {
		return []ViewerDeviceRequest{{ViewerDeviceID: "viewer", PublicKey: key}}, nil
	}, nil, nil)
	s := &Server{viewerApprovals: svc}
	fp := strings.ToLower(strings.ReplaceAll(streamseal.ViewerFingerprint("viewer", key), "-", ""))
	if _, err := s.ApproveViewerDevice(context.Background(), connect.NewRequest(&pb.ApproveViewerDeviceRequest{ViewerDeviceId: "viewer", Fingerprint: fp})); err != nil {
		t.Fatal(err)
	}
	key[0] = 99
	if m.entries["viewer"].PublicKey[0] != 3 {
		t.Fatal("registry changed stored approval memory")
	}
}

func TestViewerListRegistryFailure(t *testing.T) {
	registryErr := errors.New("registry unavailable")
	key := bytes.Repeat([]byte{7}, ed25519.PublicKeySize)
	approvedAt := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	for _, hasApproval := range []bool{false, true} {
		t.Run(map[bool]string{false: "no local approvals", true: "local approval"}[hasApproval], func(t *testing.T) {
			m := &viewerApprovalMemory{entries: map[string]*domain.ViewerDeviceApproval{}}
			if hasApproval {
				m.entries["local"] = &domain.ViewerDeviceApproval{ViewerDeviceID: "local", PublicKey: key, Label: "trusted", ApprovedAt: approvedAt}
			}
			s := &Server{viewerApprovals: NewViewerApprovals(m, func(context.Context) ([]ViewerDeviceRequest, error) {
				return []ViewerDeviceRequest{{ViewerDeviceID: "partial", PublicKey: key}}, registryErr
			}, nil, nil)}
			resp, err := s.ListViewerDevices(context.Background(), connect.NewRequest(&pb.ListLocalViewerDevicesRequest{}))
			if !hasApproval {
				if !errors.Is(err, registryErr) {
					t.Fatalf("expected registry failure without local approvals, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(resp.Msg.Devices) != 1 {
					t.Fatalf("devices: %v", resp.Msg.Devices)
				}
				d := resp.Msg.Devices[0]
				if d.ViewerDeviceId != "local" || d.Label != "trusted" || !d.IsApproved || d.Fingerprint != streamseal.ViewerFingerprint("local", key) || !d.ApprovedAt.AsTime().Equal(approvedAt) {
					t.Fatalf("lost local approval: %v", d)
				}
			}
			_, err = s.ApproveViewerDevice(context.Background(), connect.NewRequest(&pb.ApproveViewerDeviceRequest{ViewerDeviceId: "partial", Fingerprint: streamseal.ViewerFingerprint("partial", key)}))
			if !errors.Is(err, registryErr) || m.entries["partial"] != nil {
				t.Fatalf("registry failure must prevent approval: %v", err)
			}
		})
	}
}

func TestViewerRegistryRejectionIsNotSocketAuth(t *testing.T) {
	rejected := connect.NewError(connect.CodeUnauthenticated, errors.New("invalid bearer"))
	s := &Server{viewerApprovals: NewViewerApprovals(&viewerApprovalMemory{entries: map[string]*domain.ViewerDeviceApproval{}}, func(context.Context) ([]ViewerDeviceRequest, error) {
		return nil, rejected
	}, nil, nil)}
	_, listErr := s.ListViewerDevices(context.Background(), connect.NewRequest(&pb.ListLocalViewerDevicesRequest{}))
	_, approveErr := s.ApproveViewerDevice(context.Background(), connect.NewRequest(&pb.ApproveViewerDeviceRequest{ViewerDeviceId: "viewer", Fingerprint: "AAAA"}))
	for name, err := range map[string]error{"list": listErr, "approve": approveErr} {
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "boss login") || !errors.Is(err, rejected) {
			t.Fatalf("%s: registry rejection must name boss login, got %v", name, err)
		}
	}
}
