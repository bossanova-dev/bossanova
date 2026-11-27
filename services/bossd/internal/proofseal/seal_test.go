package proofseal

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func pageKey(t *testing.T) []byte {
	t.Helper()
	key, err := NewPageKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSealOpenRoundTrip_Sessions_AC278601(t *testing.T) {
	key := pageKey(t)
	for _, plain := range [][]byte{nil, []byte("<!doctype html><p>proof</p>"), bytes.Repeat([]byte{0xff}, 1<<16)} {
		sealed, err := Seal(key, PageName, plain)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(sealed)) != SealedSize(int64(len(plain))) || sealed[0] != Version {
			t.Fatalf("sealed %d bytes, version %#x; want %d bytes, version %#x", len(sealed), sealed[0], SealedSize(int64(len(plain))), Version)
		}
		if len(plain) > 0 && bytes.Contains(sealed, plain) {
			t.Fatal("ciphertext contains the plaintext")
		}
		got, err := Open(key, PageName, sealed)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("Open = %q, %v; want the plaintext", got, err)
		}
	}
}

func TestSealDrawsAFreshNonceEachTime(t *testing.T) {
	key := pageKey(t)
	a, err := Seal(key, PageName, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Seal(key, PageName, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a[1:1+NonceSize], b[1:1+NonceSize]) || bytes.Equal(a, b) {
		t.Fatal("two seals of one plaintext reused a nonce")
	}
	if bytes.Equal(pageKey(t), pageKey(t)) {
		t.Fatal("two page keys are equal")
	}
}

func TestOpenRefusesAMovedOrTamperedObject_Sessions_AC278601(t *testing.T) {
	key := pageKey(t)
	sealed, err := Seal(key, "screenshots/a.png.enc", []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(sealed)
	flipped[len(flipped)-1] ^= 1
	otherVersion := bytes.Clone(sealed)
	otherVersion[0] = 0x02
	for name, tc := range map[string]struct {
		key    []byte
		object string
		sealed []byte
	}{
		"moved to another name": {key, "screenshots/b.png.enc", sealed},
		"opened as the page":    {key, PageName, sealed},
		"another page's key":    {pageKey(t), "screenshots/a.png.enc", sealed},
		"tampered tag":          {key, "screenshots/a.png.enc", flipped},
		"another version":       {key, "screenshots/a.png.enc", otherVersion},
		"truncated":             {key, "screenshots/a.png.enc", sealed[:Overhead-1]},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := Open(tc.key, tc.object, tc.sealed); !errors.Is(err, ErrOpen) || got != nil {
				t.Fatalf("Open = %q, %v; want ErrOpen", got, err)
			}
		})
	}
}

func TestSealRefusesAKeyOfTheWrongLength(t *testing.T) {
	for _, key := range [][]byte{nil, make([]byte, 16), make([]byte, 33)} {
		if _, err := Seal(key, PageName, []byte("x")); err == nil {
			t.Fatalf("Seal accepted a %d-byte key", len(key))
		}
		if _, err := Open(key, PageName, make([]byte, Overhead)); err == nil {
			t.Fatalf("Open accepted a %d-byte key", len(key))
		}
	}
}

func TestFragmentKeyIsUnpaddedBase64URL(t *testing.T) {
	key := bytes.Repeat([]byte{0xfb, 0xff}, KeySize/2)
	got := FragmentKey(key)
	if strings.ContainsAny(got, "=+/") || len(got) != 43 {
		t.Fatalf("FragmentKey = %q, want 43 unpadded base64url characters", got)
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(got); err != nil || !bytes.Equal(decoded, key) {
		t.Fatalf("FragmentKey does not decode back to the key: %v", err)
	}
}

func TestSealedName(t *testing.T) {
	for name, want := range map[string]string{
		"index.html":         PageName,
		"manifest.json":      "manifest.json.enc",
		"screenshots/a.png":  "screenshots/a.png.enc",
		"request-log.ndjson": "request-log.ndjson.enc",
	} {
		if got := SealedName(name); got != want {
			t.Errorf("SealedName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestLoaderAndShimAreGenericAndIndependentCopies(t *testing.T) {
	loader := Loader()
	if !bytes.Contains(loader, []byte(`src="`+ShimName+`"`)) {
		t.Fatalf("the loader does not run %s", ShimName)
	}
	loader[0] ^= 1
	if bytes.Equal(loader, Loader()) {
		t.Fatal("Loader returned the embedded bytes, not a copy")
	}
	if len(Shim()) == 0 {
		t.Fatal("empty shim")
	}
}
