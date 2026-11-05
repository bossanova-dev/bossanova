package streamseal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
)

// Direction is the single byte used in frame key derivation and AAD.
type Direction byte

const (
	// DaemonToViewer encrypts terminal output (wire value 1).
	DaemonToViewer Direction = 1
	// ViewerToDaemon encrypts terminal input (wire value 2).
	ViewerToDaemon Direction = 2
)

func frameKey(key []byte, direction Direction) ([]byte, error) {
	if len(key) != 32 || (direction != DaemonToViewer && direction != ViewerToDaemon) {
		return nil, errors.New("terminal frame: invalid key or direction")
	}
	return hkdf.Key(sha256.New, key, nil, "bossanova terminal frame v1\x00"+string([]byte{byte(direction)}), 32)
}
func frameCipher(key []byte, direction Direction) (cipher.AEAD, error) {
	derived, err := frameKey(key, direction)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func frameNonce(counter uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}
func frameAAD(attachID string, direction Direction, counter uint64) []byte {
	aad := appendField(nil, []byte(attachID))
	aad = append(aad, byte(direction))
	return binary.BigEndian.AppendUint64(aad, counter)
}

// FrameSealer owns one direction's nonce counter. Use it serially; never copy it.
type FrameSealer struct {
	aead      cipher.AEAD
	direction Direction
	attachID  string
	counter   uint64
}

// NewFrameSealer derives a direction key and starts its counter at one.
// The attach key must be fresh for every attach; create only one sealer per direction.
func NewFrameSealer(key []byte, direction Direction, attachID string) (*FrameSealer, error) {
	if attachID == "" {
		return nil, errors.New("terminal frame: empty attach id")
	}
	aead, err := frameCipher(key, direction)
	if err != nil {
		return nil, err
	}
	return &FrameSealer{aead: aead, direction: direction, attachID: attachID}, nil
}

// Seal authenticates the next frame, failing before its counter can wrap.
func (s *FrameSealer) Seal(plain []byte) (uint64, []byte, error) {
	if s.counter == math.MaxUint64 {
		return 0, nil, errors.New("terminal frame: counter exhausted")
	}
	s.counter++
	return s.counter, s.aead.Seal(nil, frameNonce(s.counter), plain, frameAAD(s.attachID, s.direction, s.counter)), nil
}

// FrameOpener accepts contiguous authenticated frames. Use it serially; never copy it.
// Any failure permanently closes it, so callers cannot resume after a missing frame.
type FrameOpener struct {
	aead      cipher.AEAD
	direction Direction
	attachID  string
	counter   uint64
	closed    bool
}

// NewFrameOpener derives a direction key and expects counter one first.
func NewFrameOpener(key []byte, direction Direction, attachID string) (*FrameOpener, error) {
	if attachID == "" {
		return nil, errors.New("terminal frame: empty attach id")
	}
	aead, err := frameCipher(key, direction)
	if err != nil {
		return nil, err
	}
	return &FrameOpener{aead: aead, direction: direction, attachID: attachID}, nil
}

// Open authenticates exactly the next frame before advancing the counter.
func (o *FrameOpener) Open(counter uint64, ciphertext []byte) ([]byte, error) {
	if o.closed || o.counter == math.MaxUint64 || counter != o.counter+1 {
		o.closed = true
		return nil, errors.New("terminal frame: closed or unexpected counter")
	}
	plain, err := o.aead.Open(nil, frameNonce(counter), ciphertext, frameAAD(o.attachID, o.direction, counter))
	if err != nil {
		o.closed = true
		return nil, err
	}
	o.counter = counter
	return plain, nil
}
