package bossmcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const proofViewHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func proofViewSession(displayHead string) *pb.Session {
	pr := int32(7)
	return &pb.Session{Id: "s1", PrNumber: &pr, RepoOriginUrl: "https://github.com/acme/widgets", PrProofHeadSha: proofViewHead, PrDisplayHeadSha: displayHead}
}

func callProofViewLink(t *testing.T, backend *fakeBackend, opts Options) *mcp.CallToolResult {
	t.Helper()
	res, err := newConnectedClient(t, backend, opts).CallTool(context.Background(), &mcp.CallToolParams{Name: "get_proof_view_link", Arguments: map[string]any{"id": "s1"}})
	if err != nil {
		t.Fatalf("call get_proof_view_link: %v", err)
	}
	return res
}

// TestGetProofViewLink_McpServer_AC276901_SessionReadGatesMint proves a
// caller whose get_session fails gets that error and no mint is attempted.
func TestGetProofViewLink_McpServer_AC276901_SessionReadGatesMint(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeNotFound, connect.CodePermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			minted := false
			res := callProofViewLink(t, &fakeBackend{
				getSession: func(context.Context, string) (*pb.Session, error) {
					return nil, connect.NewError(code, errors.New("session s1 is not yours"))
				},
				mintProofViewLink: func(context.Context, *pb.MintProofViewLinkRequest) (*pb.MintProofViewLinkResponse, error) {
					minted = true
					return &pb.MintProofViewLinkResponse{ViewUrl: "https://leak.test/"}, nil
				},
			}, Options{})
			if !res.IsError || !strings.Contains(textOf(t, res), "not yours") || minted {
				t.Fatalf("isError=%v minted=%v text=%s", res.IsError, minted, textOf(t, res))
			}
		})
	}
}

func TestGetProofViewLink(t *testing.T) {
	expires := time.Date(2026, 11, 25, 12, 30, 0, 0, time.FixedZone("JST", 9*3600))
	t.Run("session without a pull request is a tool error", func(t *testing.T) {
		minted := false
		res := callProofViewLink(t, &fakeBackend{
			getSession: func(context.Context, string) (*pb.Session, error) { return &pb.Session{Id: "s1"}, nil },
			mintProofViewLink: func(context.Context, *pb.MintProofViewLinkRequest) (*pb.MintProofViewLinkResponse, error) {
				minted = true
				return &pb.MintProofViewLinkResponse{}, nil
			},
		}, Options{})
		if !res.IsError || !strings.Contains(textOf(t, res), "no pull request") || minted {
			t.Fatalf("isError=%v minted=%v text=%s", res.IsError, minted, textOf(t, res))
		}
	})
	for _, tc := range []struct {
		name        string
		displayHead string
		wantCurrent bool
	}{
		{"display head matches", strings.ToUpper(proofViewHead), true},
		{"display head moved on", strings.Repeat("b", 40), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *pb.MintProofViewLinkRequest
			res := callProofViewLink(t, &fakeBackend{
				getSession: func(_ context.Context, id string) (*pb.Session, error) {
					if id != "s1" {
						t.Errorf("id not forwarded: %q", id)
					}
					return proofViewSession(tc.displayHead), nil
				},
				mintProofViewLink: func(_ context.Context, req *pb.MintProofViewLinkRequest) (*pb.MintProofViewLinkResponse, error) {
					got = req
					return &pb.MintProofViewLinkResponse{ViewUrl: "https://acme.proofs.test/v1.1.sig/tok/", ExpiresAt: timestamppb.New(expires)}, nil
				},
			}, Options{ReadOnly: true})
			if res.IsError {
				t.Fatal(textOf(t, res))
			}
			if got.GetRepoOriginUrl() != "https://github.com/acme/widgets" || got.GetPrNumber() != 7 || got.GetHeadSha() != proofViewHead {
				t.Fatalf("mint request = %v", got)
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"view_url": "https://acme.proofs.test/v1.1.sig/tok/", "expires_at": "2026-11-25T03:30:00Z", "head_sha": proofViewHead, "is_current_head": tc.wantCurrent}
			for k, v := range want {
				if out[k] != v {
					t.Fatalf("%s = %v, want %v (result %v)", k, out[k], v, out)
				}
			}
		})
	}
	t.Run("mint NotFound is a tool error", func(t *testing.T) {
		res := callProofViewLink(t, &fakeBackend{
			getSession: func(context.Context, string) (*pb.Session, error) { return proofViewSession(proofViewHead), nil },
			mintProofViewLink: func(context.Context, *pb.MintProofViewLinkRequest) (*pb.MintProofViewLinkResponse, error) {
				return nil, connect.NewError(connect.CodeNotFound, errors.New("proof page not found"))
			},
		}, Options{})
		if !res.IsError || !strings.Contains(textOf(t, res), "not_found") {
			t.Fatalf("isError=%v text=%s", res.IsError, textOf(t, res))
		}
	})
	t.Run("backend without minting is a tool error", func(t *testing.T) {
		backend := struct{ Backend }{&fakeBackend{getSession: func(context.Context, string) (*pb.Session, error) { return proofViewSession(proofViewHead), nil }}}
		res, err := newConnectedClient(t, backend, Options{}).CallTool(context.Background(), &mcp.CallToolParams{Name: "get_proof_view_link", Arguments: map[string]any{"id": "s1"}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(textOf(t, res), "not supported") {
			t.Fatalf("isError=%v text=%s", res.IsError, textOf(t, res))
		}
	})
	t.Run("is read-only", func(t *testing.T) {
		if !slices.Contains(ReadOnlyToolNames(), "get_proof_view_link") || !listedToolNames(t, Options{ReadOnly: true})["get_proof_view_link"] {
			t.Fatal("get_proof_view_link is not in the read-only tier")
		}
	})
}
