package streamseal

import (
	"bytes"
	"math"
	"testing"
)

func TestFrames(t *testing.T) {
	for _, direction := range []Direction{DaemonToViewer, ViewerToDaemon} {
		t.Run(string(rune('0'+direction)), func(t *testing.T) {
			key := bytes.Repeat([]byte{7}, 32)
			sealer, err := NewFrameSealer(key, direction, "attach")
			if err != nil {
				t.Fatal(err)
			}
			opener, err := NewFrameOpener(key, direction, "attach")
			if err != nil {
				t.Fatal(err)
			}
			for want := uint64(1); want <= 3; want++ {
				counter, ct, err := sealer.Seal([]byte("terminal"))
				if err != nil || counter != want {
					t.Fatalf("seal: %d %v", counter, err)
				}
				plain, err := opener.Open(counter, ct)
				if err != nil || string(plain) != "terminal" {
					t.Fatalf("open: %q %v", plain, err)
				}
			}
			sealer.counter = math.MaxUint64 - 1
			if counter, _, err := sealer.Seal(nil); err != nil || counter != math.MaxUint64 {
				t.Fatalf("last counter: %d %v", counter, err)
			}
			if _, _, err := sealer.Seal(nil); err == nil {
				t.Fatal("counter wrapped")
			}
		})
	}
}

func TestFrameRejectsAndCloses(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	for _, direction := range []Direction{DaemonToViewer, ViewerToDaemon} {
		for _, failure := range []string{"zero", "missing-first", "interior-gap", "replay", "reorder", "tamper", "direction", "attach"} {
			t.Run(failure+string(rune('0'+direction)), func(t *testing.T) {
				sealer, _ := NewFrameSealer(key, direction, "attach")
				opener, _ := NewFrameOpener(key, direction, "attach")
				frames := make([][]byte, 3)
				for i := range frames {
					_, frames[i], _ = sealer.Seal([]byte("data"))
				}
				counter, ct := uint64(1), bytes.Clone(frames[0])
				switch failure {
				case "zero":
					counter = 0
				case "missing-first":
					counter, ct = 2, frames[1]
				case "interior-gap", "replay", "reorder":
					if _, err := opener.Open(1, frames[0]); err != nil {
						t.Fatal(err)
					}
					if failure == "interior-gap" {
						counter, ct = 3, frames[2]
					}
					if failure == "reorder" {
						if _, err := opener.Open(2, frames[1]); err != nil {
							t.Fatal(err)
						}
					}
				case "tamper":
					ct[0] ^= 1
				case "direction":
					opener, _ = NewFrameOpener(key, 3-direction, "attach")
				case "attach":
					opener, _ = NewFrameOpener(key, direction, "other")
				}
				if plain, err := opener.Open(counter, ct); err == nil || plain != nil {
					t.Fatal("invalid frame delivered")
				}
				if _, err := opener.Open(1, frames[0]); err == nil {
					t.Fatal("failed opener recovered")
				}
			})
		}
	}
}
