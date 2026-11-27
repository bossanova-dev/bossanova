package proofseal

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateVectors = flag.Bool("update", false, "regenerate proofseal interoperability vectors")

type sealVector struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	FragmentKey string `json:"fragmentKey"`
	Nonce       string `json:"nonce"`
	Plaintext   string `json:"plaintext"`
	Ciphertext  string `json:"ciphertext"`
}

type vectors struct {
	Version  int          `json:"version"`
	Encoding string       `json:"encoding"`
	Format   string       `json:"format"`
	Vectors  []sealVector `json:"vectors"`
}

const vectorsPath = "testdata/vectors.json"

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func generateVectors(t *testing.T) vectors {
	t.Helper()
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	cases := []struct {
		name  string
		plain []byte
	}{
		{PageName, []byte("<!doctype html>\n<title>Proof</title>\n<img src=\"screenshots/a.png\">\n")},
		{"screenshots/a.png.enc", []byte("\x89PNG\r\n\x1a\n")},
		{"manifest.json.enc", []byte(`{"criteria":[]}`)},
		{"media/empty.webm.enc", nil},
	}
	out := vectors{Version: 1, Encoding: "hex", Format: "0x01 || nonce(12) || AES-256-GCM(key, nonce, plaintext, aad=name) ciphertext+tag(16)"}
	for i, c := range cases {
		nonce := bytes.Repeat([]byte{byte(0xa0 + i)}, NonceSize)
		sealed, err := sealWithNonce(key, nonce, c.name, c.plain)
		if err != nil {
			t.Fatal(err)
		}
		out.Vectors = append(out.Vectors, sealVector{
			Name: c.name, Key: hex.EncodeToString(key), FragmentKey: FragmentKey(key),
			Nonce: hex.EncodeToString(nonce), Plaintext: hex.EncodeToString(c.plain), Ciphertext: hex.EncodeToString(sealed),
		})
	}
	return out
}

func TestGoldenVectorsDecrypt_Sessions_AC278601(t *testing.T) {
	if *updateVectors {
		data, err := json.MarshalIndent(generateVectors(t), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorsPath, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.Version != 1 || len(v.Vectors) == 0 {
		t.Fatalf("vectors version %d with %d vectors", v.Version, len(v.Vectors))
	}
	for _, vec := range v.Vectors {
		t.Run(vec.Name, func(t *testing.T) {
			key, nonce, plain, sealed := unhex(t, vec.Key), unhex(t, vec.Nonce), unhex(t, vec.Plaintext), unhex(t, vec.Ciphertext)
			if FragmentKey(key) != vec.FragmentKey {
				t.Fatalf("fragment key = %q, want %q", FragmentKey(key), vec.FragmentKey)
			}
			got, err := Open(key, vec.Name, sealed)
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("Open = %x, %v; want %x", got, err, plain)
			}
			resealed, err := sealWithNonce(key, nonce, vec.Name, plain)
			if err != nil || !bytes.Equal(resealed, sealed) {
				t.Fatalf("seal = %x, %v; want the golden ciphertext", resealed, err)
			}
		})
	}
}
