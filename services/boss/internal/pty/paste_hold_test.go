package pty

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestEnterHoldPassesEverythingThroughWhenIdle: with no upload in flight the
// filter must be the identity function. This is the local-mode guarantee — the
// keystroke path of a non---host attach must not change because this machinery
// exists.
func TestEnterHoldPassesEverythingThroughWhenIdle(t *testing.T) {
	var h pasteEnterHold
	in := []byte("what is wrong in\rthis image\r")
	if got := h.filter(in); !bytes.Equal(got, in) {
		t.Fatalf("filter(%q) = %q with no upload pending, want it unchanged", in, got)
	}
}

// TestEnterHoldWithholdsSubmitWhileUploading is the core of the fix: the turn
// must not be sent while the path is still in flight.
func TestEnterHoldWithholdsSubmitWhileUploading(t *testing.T) {
	var h pasteEnterHold
	h.begin()

	got := h.filter([]byte("describe this\r"))
	if want := []byte("describe this"); !bytes.Equal(got, want) {
		t.Fatalf("filter = %q, want %q: the typed text must still reach the "+
			"composer, only the submit key is withheld", got, want)
	}

	if replay := h.release(); !replay {
		t.Fatal("release() = false after withholding a submit, want true: " +
			"the user's Enter would be swallowed entirely")
	}
}

// TestEnterHoldReplaysOnlyOnce: a replayed submit is consumed as it is
// reported, so a second release cannot submit the turn a second time.
func TestEnterHoldReplaysOnlyOnce(t *testing.T) {
	var h pasteEnterHold
	h.begin()
	h.filter([]byte{pasteSubmitCR})

	if replay := h.release(); !replay {
		t.Fatal("first release() = false, want true")
	}
	if replay := h.release(); replay {
		t.Fatal("second release() = true, want false: the held submit was already replayed")
	}
}

// TestEnterHoldWaitsForTheLastOfTwoUploads: two images pasted in quick
// succession share one composer, and the single Enter belongs to the whole
// composer. Replaying it when the FIRST upload landed would submit a turn still
// missing the second path — the same defect this type exists to fix, just
// narrower.
func TestEnterHoldWaitsForTheLastOfTwoUploads(t *testing.T) {
	var h pasteEnterHold
	h.begin()
	h.begin()
	h.filter([]byte{pasteSubmitCR})

	if replay := h.release(); replay {
		t.Fatal("release() = true with one upload still in flight, want false: " +
			"the turn would be sent missing the second image's path")
	}
	if replay := h.release(); !replay {
		t.Fatal("release() = false once the last upload finished, want true")
	}
}

// TestEnterHoldCancelDropsTheHeldSubmit: Ctrl+C abandons the composer, so the
// Enter pressed before it must never be replayed afterwards — that would submit
// a turn the user had just cancelled. The cancel byte itself is still forwarded
// so the agent sees the interrupt immediately.
func TestEnterHoldCancelDropsTheHeldSubmit(t *testing.T) {
	var h pasteEnterHold
	h.begin()
	h.filter([]byte{pasteSubmitCR})

	got := h.filter([]byte{pasteCancelETX})
	if want := []byte{pasteCancelETX}; !bytes.Equal(got, want) {
		t.Fatalf("filter(Ctrl+C) = %q, want %q forwarded", got, want)
	}
	if replay := h.release(); replay {
		t.Fatal("release() = true after Ctrl+C, want false: the cancelled turn would be submitted")
	}
}

// TestEnterHoldDiscardDropsTheHeldSubmit covers the detach path, which abandons
// the composer for good.
func TestEnterHoldDiscardDropsTheHeldSubmit(t *testing.T) {
	var h pasteEnterHold
	h.begin()
	h.filter([]byte{pasteSubmitCR})

	h.discard()
	if replay := h.release(); replay {
		t.Fatal("release() = true after discard, want false")
	}
}

// TestEnterHoldAcceptsLF: a terminal configured for LF, or an automated driver
// writing to the same fd, submits with LF rather than CR. Holding only CR would
// leave that path racing exactly as before the fix.
func TestEnterHoldAcceptsLF(t *testing.T) {
	var h pasteEnterHold
	h.begin()

	if got := h.filter([]byte("hi\n")); !bytes.Equal(got, []byte("hi")) {
		t.Fatalf("filter = %q, want %q with LF withheld", got, []byte("hi"))
	}
	if replay := h.release(); !replay {
		t.Fatal("release() = false after withholding an LF submit, want true")
	}
}

// TestEnterHoldSubmitAfterUploadIsNotHeld: once the upload has finished there is
// nothing to wait for, so Enter must pass straight through. Without this the
// hold could latch and make every later turn need a spurious release.
func TestEnterHoldSubmitAfterUploadIsNotHeld(t *testing.T) {
	var h pasteEnterHold
	h.begin()
	h.release()

	in := []byte("second turn\r")
	if got := h.filter(in); !bytes.Equal(got, in) {
		t.Fatalf("filter(%q) = %q after the upload finished, want it unchanged", in, got)
	}
}

// TestFinishPasteUploadReplaysSubmitAfterInjectingPath proves the two halves are
// wired together in the right ORDER: the remote path must reach the composer
// before the submit key, or the turn is sent without it — which is the whole
// bug.
func TestFinishPasteUploadReplaysSubmitAfterInjectingPath(t *testing.T) {
	c := &PTYCommand{}
	input := &fakeInput{}
	upload := func(context.Context, string) (string, error) { return "/remote/cache/img.png", nil }

	c.enterHold.begin()
	c.enterHold.filter([]byte{pasteSubmitCR})
	c.finishPasteUpload(context.Background(), input, upload, "/local/img.png")

	got := input.bytes()
	pathAt := bytes.Index(got, []byte("/remote/cache/img.png"))
	submitAt := bytes.IndexByte(got, pasteSubmitCR)
	if pathAt < 0 {
		t.Fatalf("written = %q, want the remote path injected", got)
	}
	if submitAt < 0 {
		t.Fatalf("written = %q, want the withheld submit replayed", got)
	}
	if submitAt < pathAt {
		t.Fatalf("written = %q submits at %d before the path at %d: the turn "+
			"would be sent without the image", got, submitAt, pathAt)
	}
}

// TestFinishPasteUploadSubmitsAnywayOnFailure pins the decision taken for the
// failure path: the error is injected AND the turn is still submitted. A
// swallowed Enter is a worse surprise than a turn carrying boss's own error
// text, which the user can see and act on.
func TestFinishPasteUploadSubmitsAnywayOnFailure(t *testing.T) {
	c := &PTYCommand{}
	input := &fakeInput{}
	upload := func(context.Context, string) (string, error) {
		return "", errors.New("permission denied")
	}

	c.enterHold.begin()
	c.enterHold.filter([]byte{pasteSubmitCR})
	c.finishPasteUpload(context.Background(), input, upload, "/local/img.png")

	got := input.bytes()
	if !bytes.Contains(got, []byte("image upload failed")) {
		t.Fatalf("written = %q, want the failure message injected", got)
	}
	if bytes.IndexByte(got, pasteSubmitCR) < 0 {
		t.Fatalf("written = %q, want the withheld submit replayed anyway: "+
			"a swallowed Enter is a worse surprise than a late error", got)
	}
}

// TestFinishPasteUploadDoesNotReplayAfterTeardown: once the context is
// cancelled the composer is being torn down, so replaying the key would push a
// keystroke into a process the user can no longer see.
func TestFinishPasteUploadDoesNotReplayAfterTeardown(t *testing.T) {
	c := &PTYCommand{}
	input := &fakeInput{}
	upload := func(context.Context, string) (string, error) { return "/remote/img.png", nil }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c.enterHold.begin()
	c.enterHold.filter([]byte{pasteSubmitCR})
	c.finishPasteUpload(ctx, input, upload, "/local/img.png")

	if got := input.bytes(); bytes.IndexByte(got, pasteSubmitCR) >= 0 {
		t.Fatalf("written = %q, want no replayed submit after teardown", got)
	}
}

// TestFinishPasteUploadReleasesHoldOnEveryPath: the hold must not outlive the
// upload whatever happened, or the user's Enter is withheld for the rest of the
// attach with nothing left to release it.
func TestFinishPasteUploadReleasesHoldOnEveryPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		upload PasteUpload
	}{
		{"success", func(context.Context, string) (string, error) { return "/remote/img.png", nil }},
		{"failure", func(context.Context, string) (string, error) { return "", errors.New("boom") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &PTYCommand{}
			c.enterHold.begin()
			c.finishPasteUpload(context.Background(), &fakeInput{}, tc.upload, "/local/img.png")

			in := []byte("next turn\r")
			if got := c.enterHold.filter(in); !bytes.Equal(got, in) {
				t.Fatalf("filter(%q) = %q after the upload finished, want it "+
					"unchanged: the hold outlived its upload", in, got)
			}
		})
	}
}
