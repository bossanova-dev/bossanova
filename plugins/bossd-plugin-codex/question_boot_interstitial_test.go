package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
)

// updateInterstitialFixtureDigest pins testdata/panes/update_interstitial.txt
// for the same reason realPaneFixtureDigest pins question.txt: this capture is
// no longer only this module's business. services/bossd/internal/session keeps
// a byte copy (testdata/panes/codex_update_interstitial.txt) and proves "the
// session-start path refuses this pane with no keystroke sent", while the tests
// below prove "the real codex grammar calls this pane a modal". BOS-894's claim
// is the composition of the two and holds only while both sides read the same
// bytes. The module boundary forbids reading across it, so each side hashes its
// own copy against its own literal — a tripwire, not a proof: re-pinning both
// literals would let them diverge green, but divergence cannot happen QUIETLY.
//
// Re-pin this only after re-running testdata/panes/capture-update-interstitial.sh;
// a rerun on another machine or a bumped codex legitimately produces different
// bytes, so the digest is a change detector, not a contract about the content.
const updateInterstitialFixtureDigest = "422ea7fabae5dc0ff1de6afc5d9d97e903301692d256b65849918c8f437e9e3f"

func readUpdateInterstitialFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/panes/update_interstitial.txt")
	if err != nil {
		t.Fatalf("read update-interstitial fixture: %v "+
			"(regenerate with testdata/panes/capture-update-interstitial.sh; "+
			"services/bossd/internal/session copies this file, do not delete it)", err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != updateInterstitialFixtureDigest {
		t.Fatalf("fixture digest = %s, want %s; "+
			"services/bossd/internal/session/testdata/panes/codex_update_interstitial.txt "+
			"must stay byte-identical and asserts against the same digest", got, updateInterstitialFixtureDigest)
	}
	return data
}

// TestBootInterstitialRealPaneFixture is the headline claim: on a REAL capture
// of codex's update screen the modal gate refuses, and — the part that explains
// why this clause had to be written at all — the notification detector does not
// fire, because hasCodexQuestionPrompt's "›" stripper deletes the only row that
// carries the menu's distinctive text.
func TestBootInterstitialRealPaneFixture(t *testing.T) {
	data := readUpdateInterstitialFixture(t)

	if !hasCodexModalPrompt(data) {
		t.Errorf("expected modal=true on the real update-interstitial fixture (%d bytes)", len(data))
	}
	if hasCodexQuestionPrompt(data) {
		t.Error("expected has_prompt=false on the update interstitial: it is not a question, " +
			"and surfacing it as one would notify on every codex release")
	}
}

// TestBootInterstitialAlternationsAreIndependent removes one half of the
// grammar at a time and requires the other half to carry the refusal alone.
// Both directions matter: the header is the most specific evidence but the most
// likely to be restyled between releases, and the footer wording is ordinary
// English that a rewrite could touch. A change that quietly collapsed the two
// alternations into one would still pass the fixture test above; it fails here.
func TestBootInterstitialAlternationsAreIndependent(t *testing.T) {
	data := readUpdateInterstitialFixture(t)
	pane := string(data)

	if !strings.Contains(pane, "Update available!") {
		t.Fatal("fixture no longer contains the header the header-alternation anchors on")
	}
	if !strings.Contains(pane, "Press enter to continue") {
		t.Fatal("fixture no longer contains the footer the structural alternation anchors on")
	}

	t.Run("header removed, structural pair still refuses", func(t *testing.T) {
		stripped := dropFixtureLinesContaining(pane, "Update available!")
		if strings.Contains(stripped, "Update available!") {
			t.Fatal("header line survived the strip")
		}
		if !hasCodexModalPrompt([]byte(stripped)) {
			t.Errorf("expected modal=true from the menu rows plus footer alone:\n%s", stripped)
		}
	})

	t.Run("footer removed, header still refuses", func(t *testing.T) {
		stripped := dropFixtureLinesContaining(pane, "Press enter to continue")
		if strings.Contains(stripped, "Press enter to continue") {
			t.Fatal("footer line survived the strip")
		}
		if !hasCodexModalPrompt([]byte(stripped)) {
			t.Errorf("expected modal=true from the header alone:\n%s", stripped)
		}
	})

	t.Run("both removed, nothing refuses", func(t *testing.T) {
		stripped := dropFixtureLinesContaining(
			dropFixtureLinesContaining(pane, "Update available!"), "Press enter to continue")
		if hasCodexModalPrompt([]byte(stripped)) {
			t.Errorf("expected modal=false once both anchors are gone; a menu shape on its own "+
				"is an ordered list, not a modal:\n%s", stripped)
		}
	})
}

// TestBootInterstitialIgnoresProse guards the fail-open side. Every case here
// is text an agent legitimately writes into a pane it is NOT blocked on, and
// treating any of them as a modal would refuse delivery into a live composer —
// a self-inflicted outage that looks exactly like the daemon hanging.
func TestBootInterstitialIgnoresProse(t *testing.T) {
	cases := []struct {
		name string
		pane string
	}{
		{
			// The acceptance case: the footer wording with no menu behind it.
			name: "footer wording in prose, no option rows",
			pane: "I have finished wiring the installer.\n\n" +
				"The setup script pauses and asks you to Press enter to continue,\n" +
				"then finishes on its own.\n\n" +
				"› \n",
		},
		{
			// Numbered lines that are prose, not a menu: separated by blank
			// lines and continuation text, which breaks the run.
			name: "ordered prose list plus footer wording",
			pane: "Here is the plan:\n\n" +
				"1. Read the capture script.\n" +
				"   It resolves the binary through nodenv.\n\n" +
				"2. Run it.\n" +
				"   Press enter to continue when tmux asks.\n\n" +
				"› \n",
		},
		{
			// A single option-shaped row from replayed user history. One row is
			// a sentence that starts with a number, not a menu.
			name: "single replayed history row plus footer wording",
			pane: "› 1. Yes, do that\n\n" +
				"Understood. Press enter to continue was the last thing it printed.\n\n" +
				"› \n",
		},
		{
			// "update available" without the version arrow: a changelog line,
			// not the interstitial header.
			name: "changelog prose mentioning an update",
			pane: "The release notes say an Update available! banner now appears at boot.\n\n" +
				"› \n",
		},
		{
			// Activity bullets are replayed status text; "•" must never be able
			// to impersonate live menu UI.
			// The outside-voice (cross-model) review of BOS-894 produced this
			// exact pane and it used to return modal=true: a solid run of
			// numbered rows plus the footer words, with a live composer
			// underneath. It is why the menu now requires the selection
			// cursor.
			name: "agent's own ordered list plus footer wording",
			pane: "1. Install dependencies\n" +
				"2. Run tests\n\n" +
				"Press enter to continue\n\n" +
				"› \n",
		},
		{
			// Same shape with codex's activity bullets above it, which is how
			// it actually shows up mid-session rather than on a bare pane.
			name: "ordered list after activity replay, plus footer wording",
			pane: "• Read services/bossd/internal/session/tmux_chat.go\n" +
				"1. Install dependencies\n" +
				"2. Run tests\n\n" +
				"Press enter to continue\n\n" +
				"› \n",
		},
		{
			// The cursor has to belong to THIS run. A composer row drawn above
			// an unrelated numbered list must not vouch for it.
			name: "cursor row above an unrelated numbered block",
			pane: "› \n" +
				"1. Install dependencies\n" +
				"2. Run tests\n\n" +
				"Press enter to continue\n",
		},
		{
			name: "bulleted numbered rows",
			pane: "• 1. Update now\n• 2. Skip\n\n  Press enter to continue\n\n› \n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if hasCodexModalPrompt([]byte(tc.pane)) {
				t.Errorf("expected modal=false; refusing this pane would block delivery into a live composer:\n%s", tc.pane)
			}
		})
	}
}

// TestBootInterstitialAboveWindowDoesNotBlock is the stale-scrollback case. A
// user who dismissed the interstitial an hour ago leaves it in the buffer, and
// callers capture with scrollback — so a pane-wide read would refuse every
// delivery for the rest of the session. codexModalTailLines is what stops that,
// and this test is what proves the new clause respects it rather than reaching
// past it the way codexTallCardOwnsComposer deliberately does.
func TestBootInterstitialAboveWindowDoesNotBlock(t *testing.T) {
	banner := string(readUpdateInterstitialFixture(t))
	// Rendered lines, not raw ones: codexModalTail right-trims the pane's blank
	// padding before counting, so the filler has to be real content.
	filler := strings.Repeat("• Read services/bossd/internal/session/tmux_chat.go\n", codexModalTailLines+5)
	pane := strings.TrimRight(banner, " \t\r\n") + "\n" + filler + "› \n"

	if hasCodexModalPrompt([]byte(pane)) {
		t.Errorf("expected modal=false: the interstitial has scrolled more than %d rendered lines "+
			"above the composer and is no longer what the pane is showing", codexModalTailLines)
	}
}

// TestBootInterstitialWindowStopsAtDrawnContent pins the boundary this clause
// inherits from codexModalTailLines, in the direction that COSTS a chat if it
// ever moves without anyone noticing.
//
// The committed capture is recognised partly because of something the code does
// not enforce: the interstitial is the LAST thing drawn on that pane. Rows 11-50
// of the 50-row capture are blank, codexModalTail right-trims them away, and the
// banner lands inside the 30-line window as a result. Draw anything on a bottom
// row — a status line, leftover output in a reused pane, a shell prompt under a
// dead codex — and the interior blanks are no longer trailing, the banner is
// pushed out of the window, hasCodexBootInterstitial returns false, the
// readiness gate accepts the "›"-led menu row as a live composer, and the Enter
// lands on "Update now".
//
// That is SPECIFIED behaviour, not a defect to fix here. BOS-894's acceptance
// criteria require a banner above the last 30 rendered lines not to block, and
// widening the window is exactly what BOS-600 narrowed it away from: a banner
// left in scrollback would then wedge delivery to an idle chat. What was
// missing was the disclosure. This test is it — the boundary now holds because
// a test says so, not because the fixture happened to end in blank rows, and
// anyone who moves it has to move this file too.
func TestBootInterstitialWindowStopsAtDrawnContent(t *testing.T) {
	banner := strings.TrimRight(string(readUpdateInterstitialFixture(t)), " \t\r\n")
	bannerRows := len(strings.Split(banner, "\n"))
	if bannerRows >= codexModalTailLines {
		t.Fatalf("fixture is %d rendered rows, which no longer fits in the %d-line window; "+
			"this test can no longer say anything about where the boundary is", bannerRows, codexModalTailLines)
	}

	drawnBelow := func(rows int) string {
		pane := banner
		for i := 0; i < rows; i++ {
			pane += "\n• Read services/bossd/internal/session/tmux_chat.go"
		}
		return pane + "\n› \n"
	}

	t.Run("still refuses while the banner is inside the window", func(t *testing.T) {
		// One row short of the window, so every row of the banner is still
		// rendered inside it. This is the direction that must never regress.
		rows := codexModalTailLines - bannerRows - 1
		if !hasCodexModalPrompt([]byte(drawnBelow(rows))) {
			t.Errorf("expected modal=true with %d rows drawn below a %d-row banner "+
				"inside a %d-line window", rows, bannerRows, codexModalTailLines)
		}
	})

	t.Run("stops refusing once real content sits at the bottom of a full pane", func(t *testing.T) {
		// The 50-row pane the fixture was captured on, with one line drawn at
		// the very bottom: the banner is now more than 30 rendered lines up.
		// Documented limitation, asserted so it cannot drift into a surprise.
		const paneRows = 50
		pane := banner + strings.Repeat("\n", paneRows-bannerRows-1) + "\n  esc to interrupt\n"
		if hasCodexModalPrompt([]byte(pane)) {
			t.Error("expected modal=false: this asserts the KNOWN limitation. If the window " +
				"or the trim changed so the banner is seen again, that is an improvement — " +
				"update this test and the comment above it rather than reverting the change.")
		}
	})
}

// TestBootInterstitialRefusesItsOwnBannerQuoted pins the accepted false-refusal
// trade, so the first person to hit it reads a test instead of filing a bug.
//
// The header alternation fires on its own, by design, and it does not ask what
// drew the line. So a pane that merely SHOWS the banner text — an agent working
// on this very file, a cat of the fixture, a pasted bug report — refuses its
// next delivery until 30 rendered lines push it out of the window.
//
// That is the deliberate direction. Requiring the menu or the footer to
// corroborate the header would make the primary anchor die to exactly the
// restyle the second alternation exists to survive, and the failure it would
// trade into is the silent one: delivery proceeding into an Enter that
// reinstalls codex over itself. A refusal announces itself, self-clears as the
// pane redraws, and costs a retry.
//
// Asserted rather than merely written down because it is behaviour a reader
// would otherwise call a bug, and because anyone who decides the trade is wrong
// should have to delete a test that explains why it was made.
func TestBootInterstitialRefusesItsOwnBannerQuoted(t *testing.T) {
	for _, tc := range []struct {
		name string
		pane string
	}{
		{
			name: "the header quoted in agent prose, composer live",
			pane: "I reproduced it. The pane draws:\n\n" +
				"  ✨ Update available! 0.147.0 -> 9.99.0\n\n" +
				"which is what the header anchor matches.\n\n› \n",
		},
		{
			name: "the whole fixture displayed in a pane",
			pane: string(readUpdateInterstitialFixture(t)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !hasCodexModalPrompt([]byte(tc.pane)) {
				t.Error("expected modal=true. This test asserts an ACCEPTED false refusal, " +
					"not a requirement that quoted text be refused. If the grammar gained a " +
					"way to tell a drawn banner from a quoted one, that is strictly better — " +
					"update this test and its comment rather than reverting the change.")
			}
		})
	}
}

// dropFixtureLinesContaining removes whole lines carrying needle, which is how
// these tests simulate a codex release that restyled one half of the screen.
func dropFixtureLinesContaining(pane, needle string) string {
	lines := strings.Split(pane, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.Contains(line, needle) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
