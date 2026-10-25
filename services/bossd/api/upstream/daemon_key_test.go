package upstream

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/99designs/keyring"

	"github.com/recurser/bossalib/contract/cloud/daemonproof"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func TestLoadOrCreateDaemonKey_StableAcrossCalls(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)

	first, err := LoadOrCreateDaemonKey(ring, "daemon-1")
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(first) != ed25519.PrivateKeySize {
		t.Fatalf("key length = %d, want %d", len(first), ed25519.PrivateKeySize)
	}
	second, err := LoadOrCreateDaemonKey(ring, "daemon-1")
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("a second load must return the stored key, not mint a new one")
	}

	item, err := ring.Get(daemonIdentityKeyPrefix + "daemon-1")
	if err != nil {
		t.Fatalf("key not stored under the per-daemon item: %v", err)
	}
	if !bytes.Equal(item.Data, first) {
		t.Fatal("stored item does not hold the returned key")
	}

	other, err := LoadOrCreateDaemonKey(ring, "daemon-2")
	if err != nil {
		t.Fatalf("load for another daemon id: %v", err)
	}
	if bytes.Equal(other, first) {
		t.Fatal("a different daemon id must get its own key")
	}
}

func TestLoadOrCreateDaemonKey_CorruptItemIsAnErrorNotOverwritten(t *testing.T) {
	ring := keyring.NewArrayKeyring([]keyring.Item{{Key: daemonIdentityKeyPrefix + "daemon-1", Data: []byte("short")}})
	if _, err := LoadOrCreateDaemonKey(ring, "daemon-1"); err == nil {
		t.Fatal("a stored item of the wrong size must be an error")
	}
	item, err := ring.Get(daemonIdentityKeyPrefix + "daemon-1")
	if err != nil || string(item.Data) != "short" {
		t.Fatalf("the corrupt item must be left as it was, got %q, %v", item.Data, err)
	}
}

type failingKeyring struct{ keyring.Keyring }

func (failingKeyring) Get(string) (keyring.Item, error) {
	return keyring.Item{}, errors.New("keychain locked")
}

func TestLoadOrCreateDaemonKey_KeyringFailureIsReturned(t *testing.T) {
	if _, err := LoadOrCreateDaemonKey(failingKeyring{}, "daemon-1"); err == nil {
		t.Fatal("a keyring read failure must be returned")
	}
}

func TestLoadDaemonIdentityKey_UsesSharedKeyringSeam(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	originalOpen := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = originalOpen })

	key, err := LoadDaemonIdentityKey("daemon-1")
	if err != nil {
		t.Fatalf("LoadDaemonIdentityKey: %v", err)
	}
	again, err := LoadOrCreateDaemonKey(ring, "daemon-1")
	if err != nil || !bytes.Equal(key, again) {
		t.Fatalf("LoadDaemonIdentityKey did not use the shared keyring: %v", err)
	}
}

// capturingRegisterHandler records the RegisterDaemon request it receives.
type capturingRegisterHandler struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler
	mu     sync.Mutex
	msg    *pb.RegisterDaemonRequest
	header http.Header
}

func (h *capturingRegisterHandler) RegisterDaemon(
	_ context.Context,
	req *connect.Request[pb.RegisterDaemonRequest],
) (*connect.Response[pb.RegisterDaemonResponse], error) {
	h.mu.Lock()
	h.msg = req.Msg
	h.header = req.Header().Clone()
	h.mu.Unlock()
	return connect.NewResponse(&pb.RegisterDaemonResponse{DaemonId: req.Msg.DaemonId, SessionToken: "tok"}), nil
}

func newCapturingRegisterClient(t *testing.T) (*capturingRegisterHandler, bossanovav1connect.OrchestratorServiceClient) {
	t.Helper()
	handler := &capturingRegisterHandler{}
	return handler, newRegisterTestClient(t, handler)
}

// newRegisterTestClient serves handler over a test HTTP server and returns a
// client for it.
func newRegisterTestClient(t *testing.T, handler bossanovav1connect.OrchestratorServiceHandler) bossanovav1connect.OrchestratorServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	path, h := bossanovav1connect.NewOrchestratorServiceHandler(handler)
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL)
}

func TestRegister_SignsWithIdentityKey(t *testing.T) {
	handler, client := newCapturingRegisterClient(t)
	key, err := LoadOrCreateDaemonKey(keyring.NewArrayKeyring(nil), "daemon-signed")
	if err != nil {
		t.Fatalf("load key: %v", err)
	}

	if _, err := Register(context.Background(), client, "daemon-signed", "host", "jwt-abc", nil, key); err != nil {
		t.Fatalf("Register: %v", err)
	}

	msg := handler.msg
	if pub, ok := key.Public().(ed25519.PublicKey); !ok || !bytes.Equal(msg.GetDaemonPublicKey(), pub) {
		t.Fatal("daemon_public_key is not the identity key's public half")
	}
	if len(msg.GetPossessionSignature()) == 0 || msg.GetSignedAtUnix() == 0 {
		t.Fatal("possession_signature and signed_at_unix must be set")
	}
	// The server recomputes the proof from the bearer the request carried.
	bearer := strings.TrimPrefix(handler.header.Get("Authorization"), "Bearer ")
	if bearer != "jwt-abc" {
		t.Fatalf("Authorization bearer = %q, want jwt-abc", bearer)
	}
	if err := daemonproof.Verify(msg.GetDaemonPublicKey(), msg.GetDaemonId(), bearer, msg.GetSignedAtUnix(), msg.GetPossessionSignature(), time.Now()); err != nil {
		t.Fatalf("signature does not verify with daemonproof.Verify: %v", err)
	}
}

func TestRegister_WithoutKeySendsNoProof(t *testing.T) {
	handler, client := newCapturingRegisterClient(t)
	if _, err := Register(context.Background(), client, "daemon-plain", "host", "jwt-abc", nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	msg := handler.msg
	if len(msg.GetDaemonPublicKey()) != 0 || len(msg.GetPossessionSignature()) != 0 || msg.GetSignedAtUnix() != 0 {
		t.Fatalf("a keyless Register must send no proof fields, got %+v", msg)
	}
}

// pinnedElsewhereHandler refuses a keyed registration with keyedCode (by
// default the way bosso refuses a key that does not match the id's pin), and
// admits a keyless one the way it admits the pin's own account, unless
// keylessErr refuses that too.
type pinnedElsewhereHandler struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler
	keyedCode  connect.Code
	keyedMsg   string
	keylessErr *connect.Error
	mu         sync.Mutex
	msgs       []*pb.RegisterDaemonRequest
}

func (h *pinnedElsewhereHandler) RegisterDaemon(
	_ context.Context,
	req *connect.Request[pb.RegisterDaemonRequest],
) (*connect.Response[pb.RegisterDaemonResponse], error) {
	h.mu.Lock()
	h.msgs = append(h.msgs, req.Msg)
	h.mu.Unlock()
	if len(req.Msg.GetDaemonPublicKey()) != 0 {
		code := h.keyedCode
		if code == 0 {
			code = connect.CodePermissionDenied
		}
		msg := h.keyedMsg
		if msg == "" {
			msg = "daemon possession proof refused"
		}
		return nil, connect.NewError(code, errors.New(msg))
	}
	if h.keylessErr != nil {
		return nil, h.keylessErr
	}
	return connect.NewResponse(&pb.RegisterDaemonResponse{DaemonId: req.Msg.DaemonId, SessionToken: "keyless-tok"}), nil
}

// A machine whose clock is outside the server's window signs a proof the
// server refuses as stale (Unauthenticated). The daemon must still register
// keylessly, as it did before it had a key, rather than lock itself out.
func TestRegister_RefusedProofFallsBackToKeyless(t *testing.T) {
	for _, code := range []connect.Code{connect.CodePermissionDenied, connect.CodeUnauthenticated} {
		t.Run(code.String(), func(t *testing.T) {
			handler := &pinnedElsewhereHandler{keyedCode: code}
			client := newRegisterTestClient(t, handler)

			key, err := LoadOrCreateDaemonKey(keyring.NewArrayKeyring(nil), "daemon-refused")
			if err != nil {
				t.Fatalf("load key: %v", err)
			}
			tok, err := Register(context.Background(), client, "daemon-refused", "host", "jwt-abc", nil, key)
			if err != nil {
				t.Fatalf("Register after a %v refusal: %v", code, err)
			}
			if tok != "keyless-tok" {
				t.Fatalf("session token = %q, want the keyless registration's token", tok)
			}
			if len(handler.msgs) != 2 || len(handler.msgs[0].GetDaemonPublicKey()) == 0 || len(handler.msgs[1].GetDaemonPublicKey()) != 0 {
				t.Fatalf("want one keyed attempt then one keyless retry, got %d calls", len(handler.msgs))
			}
		})
	}
}

// When the keyless retry is refused too, the error keeps the retry's code (the
// stream loop routes on it) but still names the keyed refusal, which is often
// the real cause: a skewed clock looks like a key mismatch otherwise.
func TestRegister_RefusedRetryStillNamesTheKeyedRefusal(t *testing.T) {
	handler := &pinnedElsewhereHandler{
		keyedCode:  connect.CodeUnauthenticated,
		keyedMsg:   "daemon possession proof invalid: check the daemon machine's clock",
		keylessErr: connect.NewError(connect.CodePermissionDenied, errors.New("daemon id is bound to a different machine key")),
	}
	client := newRegisterTestClient(t, handler)

	key, err := LoadOrCreateDaemonKey(keyring.NewArrayKeyring(nil), "daemon-skewed")
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	_, err = Register(context.Background(), client, "daemon-skewed", "host", "jwt-abc", nil, key)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Register: code = %v (err %v), want the retry's PermissionDenied", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "check the daemon machine's clock") {
		t.Fatalf("Register error %q must name the keyed refusal", err)
	}
}

// Other refusals are not retried: a keyless retry cannot change them.
func TestRegister_OtherRefusalsAreNotRetriedKeyless(t *testing.T) {
	handler := &pinnedElsewhereHandler{keyedCode: connect.CodeFailedPrecondition}
	client := newRegisterTestClient(t, handler)

	key, err := LoadOrCreateDaemonKey(keyring.NewArrayKeyring(nil), "daemon-precondition")
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	if _, err := Register(context.Background(), client, "daemon-precondition", "host", "jwt-abc", nil, key); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Register: err = %v, want the FailedPrecondition refusal", err)
	}
	if len(handler.msgs) != 1 {
		t.Fatalf("RegisterDaemon calls = %d, want no keyless retry", len(handler.msgs))
	}
}

func TestRegister_LostKeyFallsBackToKeylessForTheOwner(t *testing.T) {
	handler := &pinnedElsewhereHandler{}
	client := newRegisterTestClient(t, handler)

	// A keychain reset: the keyring mints a fresh key that is not the pin.
	key, err := LoadOrCreateDaemonKey(keyring.NewArrayKeyring(nil), "daemon-reset")
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	tok, err := Register(context.Background(), client, "daemon-reset", "host", "jwt-abc", nil, key)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if tok != "keyless-tok" {
		t.Fatalf("session token = %q, want the keyless registration's token", tok)
	}
	if len(handler.msgs) != 2 {
		t.Fatalf("RegisterDaemon calls = %d, want a keyed attempt then one keyless retry", len(handler.msgs))
	}
	if len(handler.msgs[0].GetDaemonPublicKey()) == 0 || len(handler.msgs[1].GetDaemonPublicKey()) != 0 {
		t.Fatal("want the first attempt keyed and the retry keyless")
	}
}
