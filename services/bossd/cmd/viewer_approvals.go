package main

import (
	"context"
	"crypto/ed25519"

	"connectrpc.com/connect"
	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
)

func (w *daemonWiring) wireViewerApprovals(client connect.HTTPClient, url string, token func() string, identity ed25519.PrivateKey) {
	read := upstream.NewViewerRequestReader(client, url, token)
	w.viewerApprovals = server.NewViewerApprovals(w.viewerApprovalStore, func(ctx context.Context) ([]server.ViewerDeviceRequest, error) {
		requests, err := read(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]server.ViewerDeviceRequest, 0, len(requests))
		for _, r := range requests {
			out = append(out, server.ViewerDeviceRequest{ViewerDeviceID: r.ID, PublicKey: r.PublicKey, Label: r.Label})
		}
		return out, nil
	}, identity, func() bool { return w.streamClient != nil && w.streamClient.AuthSnapshot().Connected })
}
