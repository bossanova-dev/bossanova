package server

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/streamseal"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ViewerDeviceRequest is untrusted public registry material. Its fingerprint is computed locally.
type ViewerDeviceRequest struct {
	ViewerDeviceID string
	PublicKey      []byte
	Label          string
}
type viewerApprovalStore interface {
	Get(context.Context, string) (*domain.ViewerDeviceApproval, error)
	List(context.Context) ([]*domain.ViewerDeviceApproval, error)
	Create(context.Context, *domain.ViewerDeviceApproval) error
	Delete(context.Context, string) error
}

// ViewerApprovals owns local approvals and per-device revocation subscriptions.
// A closed subscription means the attach must stop; notifications cannot be dropped or block revoke.
type ViewerApprovals struct {
	store       viewerApprovalStore
	requests    func(context.Context) ([]ViewerDeviceRequest, error)
	identity    ed25519.PublicKey
	connected   func() bool
	mu          sync.Mutex
	subscribers map[string]map[chan struct{}]struct{}
}

// NewViewerApprovals wires only the local socket service, never the cloud command dispatcher.
func NewViewerApprovals(store viewerApprovalStore, requests func(context.Context) ([]ViewerDeviceRequest, error), identity ed25519.PrivateKey, connected func() bool) *ViewerApprovals {
	v := &ViewerApprovals{store: store, requests: requests, connected: connected, subscribers: make(map[string]map[chan struct{}]struct{})}
	if len(identity) == ed25519.PrivateKeySize {
		v.identity = append(ed25519.PublicKey(nil), identity[ed25519.SeedSize:]...)
	}
	return v
}

// Subscribe returns a one-shot revocation signal and an idempotent unsubscribe function.
// Consumers subscribe before checking Get so revocation cannot race attach authorization.
func (v *ViewerApprovals) Subscribe(id string) (<-chan struct{}, func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ch := make(chan struct{})
	if v.subscribers[id] == nil {
		v.subscribers[id] = make(map[chan struct{}]struct{})
	}
	v.subscribers[id][ch] = struct{}{}
	return ch, func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		delete(v.subscribers[id], ch)
		if len(v.subscribers[id]) == 0 {
			delete(v.subscribers, id)
		}
	}
}

// Get returns only the locally pinned key; registry material never authorizes an attach.
func (v *ViewerApprovals) Get(ctx context.Context, id string) (*domain.ViewerDeviceApproval, error) {
	return v.store.Get(ctx, id)
}

func (s *Server) GetViewerDeviceStatus(_ context.Context, _ *connect.Request[pb.GetViewerDeviceStatusRequest]) (*connect.Response[pb.GetViewerDeviceStatusResponse], error) {
	out := &pb.GetViewerDeviceStatusResponse{}
	v := s.viewerApprovals
	if v != nil && len(v.identity) == ed25519.PublicKeySize && v.connected != nil && v.connected() {
		out.IsAvailable = true
		out.DaemonFingerprint = streamseal.DaemonFingerprint(v.identity)
	}
	return connect.NewResponse(out), nil
}
func (s *Server) ListViewerDevices(ctx context.Context, _ *connect.Request[pb.ListLocalViewerDevicesRequest]) (*connect.Response[pb.ListLocalViewerDevicesResponse], error) {
	v := s.viewerApprovals
	if v == nil || v.store == nil {
		return nil, viewerUnavailable()
	}
	approvals, err := v.store.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	rows := make(map[string]*pb.LocalViewerDevice)
	if v.requests != nil {
		requests, listErr := v.requests(ctx)
		if listErr != nil {
			if len(approvals) == 0 {
				return nil, viewerRegistryErr(listErr)
			}
			// Local approvals remain manageable while the registry is unavailable.
			requests = nil
		}
		for _, r := range requests {
			if r.ViewerDeviceID != "" && len(r.PublicKey) == 32 {
				rows[r.ViewerDeviceID] = &pb.LocalViewerDevice{ViewerDeviceId: r.ViewerDeviceID, Label: r.Label, Fingerprint: streamseal.ViewerFingerprint(r.ViewerDeviceID, r.PublicKey)}
			}
		}
	}
	for _, a := range approvals {
		rows[a.ViewerDeviceID] = &pb.LocalViewerDevice{ViewerDeviceId: a.ViewerDeviceID, Label: a.Label, Fingerprint: streamseal.ViewerFingerprint(a.ViewerDeviceID, a.PublicKey), IsApproved: true, ApprovedAt: timestamppb.New(a.ApprovedAt)}
	}
	out := &pb.ListLocalViewerDevicesResponse{Devices: make([]*pb.LocalViewerDevice, 0, len(rows))}
	for _, r := range rows {
		out.Devices = append(out.Devices, r)
	}
	sort.Slice(out.Devices, func(i, j int) bool { return out.Devices[i].ViewerDeviceId < out.Devices[j].ViewerDeviceId })
	return connect.NewResponse(out), nil
}
func (s *Server) ApproveViewerDevice(ctx context.Context, req *connect.Request[pb.ApproveViewerDeviceRequest]) (*connect.Response[pb.ApproveViewerDeviceResponse], error) {
	v := s.viewerApprovals
	if v == nil || v.store == nil || v.requests == nil {
		return nil, viewerUnavailable()
	}
	requests, err := v.requests(ctx)
	if err != nil {
		return nil, viewerRegistryErr(err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, r := range requests {
		if r.ViewerDeviceID != req.Msg.ViewerDeviceId || len(r.PublicKey) != 32 {
			continue
		}
		actual := normalizeViewerFingerprint(streamseal.ViewerFingerprint(r.ViewerDeviceID, r.PublicKey))
		typed := normalizeViewerFingerprint(req.Msg.Fingerprint)
		if typed == "" || subtle.ConstantTimeCompare([]byte(actual), []byte(typed)) != 1 {
			return nil, viewerMismatch()
		}
		// Never silently replace an existing pin, including when bosso changes a registry key.
		existing, getErr := v.store.Get(ctx, r.ViewerDeviceID)
		if getErr != nil && !errors.Is(getErr, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeInternal, getErr)
		}
		if existing != nil {
			if subtle.ConstantTimeCompare(existing.PublicKey, r.PublicKey) != 1 {
				return nil, viewerMismatch()
			}
			return connect.NewResponse(&pb.ApproveViewerDeviceResponse{}), nil
		}
		now := time.Now().UTC()
		if err := v.store.Create(ctx, &domain.ViewerDeviceApproval{ViewerDeviceID: r.ViewerDeviceID, PublicKey: append([]byte(nil), r.PublicKey...), Label: r.Label, ApprovedAt: now, CreatedAt: now}); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		return connect.NewResponse(&pb.ApproveViewerDeviceResponse{}), nil
	}
	return nil, viewerMismatch()
}
func (s *Server) RevokeViewerDevice(ctx context.Context, req *connect.Request[pb.RevokeViewerDeviceRequest]) (*connect.Response[pb.RevokeViewerDeviceResponse], error) {
	v := s.viewerApprovals
	if v == nil || v.store == nil {
		return nil, viewerUnavailable()
	}
	// Hold the notifier lock through deletion so subscriptions and revocations have one ordering.
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.store.Delete(ctx, req.Msg.ViewerDeviceId); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for ch := range v.subscribers[req.Msg.ViewerDeviceId] {
		close(ch)
	}
	delete(v.subscribers, req.Msg.ViewerDeviceId)
	return connect.NewResponse(&pb.RevokeViewerDeviceResponse{}), nil
}
func normalizeViewerFingerprint(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}
func viewerMismatch() error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New("viewer fingerprint does not match current request; do not approve"))
}

// viewerRegistryErr keeps bosso's rejection of the daemon's credentials from reaching the CLI as
// CodeUnauthenticated, which the CLI reports as a stale local socket token.
func viewerRegistryErr(err error) error {
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		return err
	}
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("viewer registry rejected this daemon's credentials; run `boss login`: %w", err))
}
func viewerUnavailable() error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New("viewer approval service unavailable"))
}
