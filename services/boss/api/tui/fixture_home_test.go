package tui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"

	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (f *fakeHomeCloudAccessClient) GetCloudAccessStatus(context.Context) (*pb.CloudAccessStatus, error) {
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.status, nil
}
func (f *fakeHomeCloudAccessClient) CreateCheckoutSession(_ context.Context, returnURL, cancelURL string) (string, error) {
	f.checkouts++
	f.returnURLs = append(f.returnURLs, returnURL)
	f.cancelURLs = append(f.cancelURLs, cancelURL)
	if f.checkoutErr != nil {
		return "", f.checkoutErr
	}
	return f.checkoutURL, nil
}
func (f *fakeHomeCloudAccessClient) CreateBillingPortalSession(_ context.Context, returnURL string) (string, error) {
	f.portals++
	f.portalReturnURLs = append(f.portalReturnURLs, returnURL)
	if f.portalErr != nil {
		return "", f.portalErr
	}
	return f.portalURL, nil
}
func (f *fakeHomeCloudAccessClient) RefreshCloudEntitlements(context.Context) (*pb.CloudAccessStatus, error) {
	f.refreshes++
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	if f.refresh != nil {
		return f.refresh, nil
	}
	return f.status, nil
}
func strPtr(s string) *string { return &s }

// runCmd executes a tea.Cmd and returns the resulting message, or nil if cmd is nil.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

func endpointSession(id string, ports ...uint32) *pb.Session {
	sess := &pb.Session{Id: id, Title: "Session " + id}
	for _, p := range ports {
		sess.HttpEndpoints = append(sess.HttpEndpoints, &pb.HTTPEndpoint{
			Port: p,
			Url:  fmt.Sprintf("http://localhost:%d", p),
		})
	}
	return sess
}

type countingLogoutTokenStore struct {
	deletes int
}

func (s *countingLogoutTokenStore) Save(*auth.Tokens) error { return nil }
func (s *countingLogoutTokenStore) Load() (*auth.Tokens, error) {
	return &auth.Tokens{AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (s *countingLogoutTokenStore) Delete() error {
	s.deletes++
	return nil
}

// archivePendingSession builds a row the way the daemon serves it: the label
// comes from displaystatus.Compute, so ArchivePending=true renders "archiving"
// and a cleared flag falls back to the chat's own status.
func archivePendingSession(id string, pending bool) *pb.Session {
	sess := &pb.Session{Id: id, Title: "t", ArchivePending: pending}
	out := displaystatus.Compute(displaystatus.Input{Session: sess, ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING})
	sess.DisplayLabel, sess.DisplayIntent, sess.DisplaySpinner = out.Label, out.Intent, out.Spinner
	return sess
}
