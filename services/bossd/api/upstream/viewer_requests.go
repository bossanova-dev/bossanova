package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// ViewerRequest contains public registry material, never approval state.
type ViewerRequest struct {
	ID        string `json:"id"`
	PublicKey []byte `json:"publicKey"`
	Label     string `json:"label"`
}

type viewerRequestsResponse struct {
	ViewerDevices []ViewerRequest `json:"viewerDevices"`
}

// viewerRegistryJSON bridges the registry RPC before its generated bindings are
// available on this branch. Connect still handles deadlines and structured errors.
// BOS-2190 introduces the same JSON contract; replace with generated bindings once merged.
type viewerRegistryJSON struct{}

func (viewerRegistryJSON) Name() string                           { return "json" }
func (viewerRegistryJSON) Marshal(value any) ([]byte, error)      { return json.Marshal(value) }
func (viewerRegistryJSON) Unmarshal(data []byte, value any) error { return json.Unmarshal(data, value) }

// NewViewerRequestReader authenticates every request with the current daemon token.
// Its narrow public-only result keeps approval authority out of the upstream door.
func NewViewerRequestReader(client connect.HTTPClient, baseURL string, token func() string) func(context.Context) ([]ViewerRequest, error) {
	rpc := connect.NewClient[struct{}, viewerRequestsResponse](client, strings.TrimRight(baseURL, "/")+"/bossanova.v1.OrchestratorService/ListViewerDeviceRequests", connect.WithCodec(viewerRegistryJSON{}), connect.WithReadMaxBytes(1024*1024))
	return func(ctx context.Context) ([]ViewerRequest, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		currentToken := ""
		if token != nil {
			currentToken = token()
		}
		if currentToken == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("viewer registry requires a signed-in daemon"))
		}
		request := connect.NewRequest(&struct{}{})
		request.Header().Set("Authorization", "Bearer "+currentToken)
		response, err := rpc.CallUnary(ctx, request)
		if err != nil {
			return nil, err
		}
		return response.Msg.ViewerDevices, nil
	}
}
