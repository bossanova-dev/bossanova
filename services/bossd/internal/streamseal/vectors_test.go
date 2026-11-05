package streamseal

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"testing"
)

var updateVectors = flag.Bool("update", false, "regenerate streamseal interoperability vectors")

type attachVector struct {
	Context                 AttachContext `json:"context"`
	ViewerPrivateKey        string        `json:"viewerPrivateKey"`
	ViewerPublicKey         string        `json:"viewerPublicKey"`
	DaemonIdentityPublicKey string        `json:"daemonIdentityPublicKey"`
	AttachKey               string        `json:"attachKey"`
	Info                    string        `json:"info"`
	Statement               string        `json:"statement"`
	Enc                     string        `json:"enc"`
	Ciphertext              string        `json:"ciphertext"`
	Box                     string        `json:"box"`
	Signature               string        `json:"signature"`
}
type frameVector struct {
	Direction  Direction `json:"direction"`
	Counter    uint64    `json:"counter"`
	Plaintext  string    `json:"plaintext"`
	Key        string    `json:"key"`
	Nonce      string    `json:"nonce"`
	AAD        string    `json:"aad"`
	Ciphertext string    `json:"ciphertext"`
	IsValid    bool      `json:"isValid"`
}
type sequenceVector struct {
	Direction Direction `json:"direction"`
	Counters  []uint64  `json:"counters"`
	IsValid   []bool    `json:"isValid"`
}
type fingerprintVector struct {
	ViewerDeviceID string `json:"viewerDeviceID"`
	PublicKey      string `json:"publicKey"`
	Viewer         string `json:"viewer"`
	Daemon         string `json:"daemon"`
}
type vectors struct {
	Version      int               `json:"version"`
	Encoding     string            `json:"encoding"`
	Attach       attachVector      `json:"attach"`
	Frames       []frameVector     `json:"frames"`
	Sequences    []sequenceVector  `json:"sequences"`
	Fingerprints fingerprintVector `json:"fingerprints"`
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVectors(t *testing.T) {
	const path = "testdata/vectors.json"
	if *updateVectors {
		v := generateVectors(t)
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v.Version != 1 {
		t.Fatalf("unsupported vector version %d", v.Version)
	}
	a := v.Attach
	viewer, err := ecdh.X25519().NewPrivateKey(unhex(t, a.ViewerPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viewer.PublicKey().Bytes(), unhex(t, a.ViewerPublicKey)) {
		t.Fatal("viewer public key differs")
	}
	box := unhex(t, a.Box)
	if !bytes.Equal(box, append(unhex(t, a.Enc), unhex(t, a.Ciphertext)...)) {
		t.Fatal("HPKE enc/ciphertext split differs")
	}
	sealed := SealedAttachKey{box, unhex(t, a.Signature)}
	key, err := OpenAttachKey(viewer, unhex(t, a.DaemonIdentityPublicKey), a.Context, sealed)
	if err != nil || !bytes.Equal(key, unhex(t, a.AttachKey)) {
		t.Fatalf("attach vector: %x %v", key, err)
	}
	if !bytes.Equal(a.Context.info(), unhex(t, a.Info)) || !bytes.Equal(a.Context.statement(viewer.PublicKey().Bytes(), box), unhex(t, a.Statement)) {
		t.Fatal("context encoding differs")
	}
	wantFrames := generateFrames(t, key, a.Context.AttachID)
	if !reflect.DeepEqual(v.Frames, wantFrames) {
		t.Fatal("deterministic frame vectors differ")
	}
	for _, f := range v.Frames {
		opener, err := NewFrameOpener(key, f.Direction, a.Context.AttachID)
		if err != nil {
			t.Fatal(err)
		}
		opener.counter = f.Counter - 1 // Isolated AEAD vector; sequences below test the receive policy.
		plain, err := opener.Open(f.Counter, unhex(t, f.Ciphertext))
		if (err == nil) != f.IsValid || (f.IsValid && !bytes.Equal(plain, unhex(t, f.Plaintext))) {
			t.Fatalf("frame vector direction %d counter %d: %v", f.Direction, f.Counter, err)
		}
	}
	checkSequences(t, v, key)
	fp := v.Fingerprints
	if ViewerFingerprint(fp.ViewerDeviceID, unhex(t, fp.PublicKey)) != fp.Viewer || DaemonFingerprint(unhex(t, fp.PublicKey)) != fp.Daemon || fp.Viewer == fp.Daemon {
		t.Fatal("fingerprint vectors differ")
	}
}

func checkSequences(t *testing.T, v vectors, key []byte) {
	t.Helper()
	if len(v.Sequences) != 6 {
		t.Fatal("missing counter sequence vectors")
	}
	for _, s := range v.Sequences {
		opener, err := NewFrameOpener(key, s.Direction, v.Attach.Context.AttachID)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Counters) != len(s.IsValid) {
			t.Fatal("sequence result count differs")
		}
		for i, counter := range s.Counters {
			var ct []byte
			for _, f := range v.Frames {
				if f.IsValid && f.Direction == s.Direction && f.Counter == counter {
					ct = unhex(t, f.Ciphertext)
				}
			}
			if ct == nil {
				t.Fatalf("missing frame %d", counter)
			}
			_, err := opener.Open(counter, ct)
			if (err == nil) != s.IsValid[i] {
				t.Fatalf("sequence %v direction %d at %d: %v", s.Counters, s.Direction, counter, err)
			}
		}
	}
}

func generateVectors(t *testing.T) vectors {
	t.Helper()
	viewer, err := ecdh.X25519().NewPrivateKey(bytes.Repeat([]byte{0x21}, 32))
	if err != nil {
		t.Fatal(err)
	}
	identity := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, 32))
	ac := AttachContext{"daemon-vector", "viewer-vector", "attach-vector", "chat-vector"}
	key := bytes.Repeat([]byte{0x63}, 32)
	sealed, err := SealAttachKey(viewer.PublicKey().Bytes(), key, ac, identity)
	if err != nil {
		t.Fatal(err)
	}
	h := hex.EncodeToString
	v := vectors{Version: 1, Encoding: "All byte fields are lowercase hex. Lengths are uint32 big-endian; counters are uint64 big-endian. Direction 1 is daemon-to-viewer, 2 is viewer-to-daemon. HPKE box = enc (32 bytes) || ciphertext. HKDF-SHA256 salt is empty; info = UTF-8 bossanova terminal frame v1 followed by NUL and direction byte."}
	v.Attach = attachVector{ac, h(viewer.Bytes()), h(viewer.PublicKey().Bytes()), h(identity.Public().(ed25519.PublicKey)), h(key), h(ac.info()), h(ac.statement(viewer.PublicKey().Bytes(), sealed.Box)), h(sealed.Box[:32]), h(sealed.Box[32:]), h(sealed.Box), h(sealed.Signature)}
	v.Frames = generateFrames(t, key, ac.AttachID)
	for _, d := range []Direction{DaemonToViewer, ViewerToDaemon} {
		v.Sequences = append(v.Sequences,
			sequenceVector{d, []uint64{1, 2, 3}, []bool{true, true, true}},
			sequenceVector{d, []uint64{2, 1}, []bool{false, false}},
			sequenceVector{d, []uint64{1, 3, 2}, []bool{true, false, false}})
	}
	pub := viewer.PublicKey().Bytes()
	v.Fingerprints = fingerprintVector{ac.ViewerDeviceID, h(pub), ViewerFingerprint(ac.ViewerDeviceID, pub), DaemonFingerprint(pub)}
	return v
}

func generateFrames(t *testing.T, key []byte, attachID string) []frameVector {
	t.Helper()
	var frames []frameVector
	h := hex.EncodeToString
	for _, d := range []Direction{DaemonToViewer, ViewerToDaemon} {
		sealer, err := NewFrameSealer(key, d, attachID)
		if err != nil {
			t.Fatal(err)
		}
		derived, err := frameKey(key, d)
		if err != nil {
			t.Fatal(err)
		}
		for _, plain := range [][]byte{[]byte("terminal frame one\r\n"), {0, 1, 127, 128, 255}, {}} {
			counter, ct, err := sealer.Seal(plain)
			if err != nil {
				t.Fatal(err)
			}
			f := frameVector{d, counter, h(plain), h(derived), h(frameNonce(counter)), h(frameAAD(attachID, d, counter)), h(ct), true}
			frames = append(frames, f)
			if counter == 3 {
				ct[0] ^= 1
				f.Ciphertext = h(ct)
				f.IsValid = false
				frames = append(frames, f)
			}
		}
	}
	return frames
}
