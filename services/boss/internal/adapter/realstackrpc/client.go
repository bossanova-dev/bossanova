// Package realstackrpc owns the Connect transport used by the real-stack harness.
package realstackrpc

import (
	"net/http"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// Client is the generated daemon client, kept at the transport boundary.
type Client = bossanovav1connect.DaemonServiceClient

// NewClient authenticates daemon RPCs over the caller's isolated HTTP transport.
func NewClient(httpClient *http.Client, baseURL, token string) Client {
	return bossanovav1connect.NewDaemonServiceClient(httpClient, baseURL, connect.WithInterceptors(socketauth.NewClientInterceptor(token)))
}

// Request wraps an RPC payload at the Connect boundary.
func Request[T any](message *T) *connect.Request[T] { return connect.NewRequest(message) }
