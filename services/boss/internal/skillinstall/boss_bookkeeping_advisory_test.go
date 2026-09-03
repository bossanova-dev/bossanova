package skillinstall

import (
	"strings"
	"testing"
)

// BOS-1105. Bookkeeping is what a run records about itself: route receipts, ledgers, and
// installed-skills drift. None of it is work state, and none of it is one of BLOCKED's four
// exhaustive causes (see TestBossBuildBlockedHasExactlyFourCauses). BOS-1085 nonetheless exited
// BLOCKED on a missing route-receipt stamp while its branch was pushed and green, so these pins
// hold the split open: bookkeeping warns and continues, an absent install still blocks.

// TestRouteReceiptIsAdvisoryAtTerminalTime pins the exact site that stranded BOS-1085. The receipt
// records the route; it must never be the thing that picks the terminal state.
func TestRouteReceiptIsAdvisoryAtTerminalTime(t *testing.T) {
	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			finalize := readPayloadFile(t, payload, "skills/boss-build/references/finalize-and-stop.md")

			// Step 12 is the file's last section, so window it from its heading to EOF rather
			// than widening these assertions across the whole reference.
			idx := strings.Index(finalize, "## Step 12: Stop cleanly")
			if idx < 0 {
				t.Fatalf("%s: finalize-and-stop.md has no Step 12 heading", label)
			}
			step12 := unwrapped(finalize[idx:])

			// The warning replaces the stop, and says which half of the run is unaffected. Pin the
			// composed line: "bookkeeping only, work state unaffected" appears three times in this
			// window, so two separate substring pins are satisfied by prose that is not this echo.
			assertContains(t, step12, "warning: route receipt incomplete (${RC_DETAIL:-no detail}) — bookkeeping only, work state unaffected")

			// The terminal state comes from the work, never from the receipt.
			assertContains(t, step12, "it records the route, it never picks the terminal state")
			assertContains(t, step12, "**Never finalize `BLOCKED` because a receipt was incomplete.**")
			assertContains(t, step12, "Pick the terminal state from the **work state**")

			// The old hard stop must be gone: an unsatisfied receipt no longer suppresses the
			// terminal state, which is precisely what stranded a pushed, green branch. Scope the
			// negative to the WHOLE file, not the Step 12 window — a negative costs nothing to
			// widen, and a windowed one stays green when the stop is reintroduced elsewhere.
			assertNotContains(t, unwrapped(finalize), "route-contract unsatisfied; no terminal state")

			// A satisfied receipt may still carry the legitimate BLOCKED downgrade — the change
			// makes failure advisory, it does not delete the contract.
			assertContains(t, step12, "route-contract.mjs\" assert --outcome")

			// Ledger writes are history, not a terminal-state input.
			assertContains(t, unwrapped(finalize), "warning: ledger write failed")
			assertContains(t, unwrapped(finalize), "A ledger is history, never a terminal-state input.")
		})
	}
}

// TestSkillsDriftWarnsWhileMissingInstallBlocks holds the one line the plan keeps hard. Drift means
// the installed tree is stale — it still runs. A missing install means there is no toolbox at all,
// so nothing downstream can execute; that is an absent capability, not an accounting discrepancy.
func TestSkillsDriftWarnsWhileMissingInstallBlocks(t *testing.T) {
	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			for _, tc := range []struct {
				path string
				// hasHardStop says whether this skill carries a missing-install BLOCKED at all.
				// It is a separate field on purpose: overloading `stillHard: ""` as the skip
				// sentinel lets an author whose edit reds a row make it green by deleting the
				// needle, and the run then reports a full pass with that coverage silently gone.
				hasHardStop bool
				stillHard   string
				driftWarns  string
			}{
				{
					path:        "skills/boss-build/SKILL.md",
					hasHardStop: true,
					stillHard:   `BLOCKED: installed boss skills not found`,
					driftWarns:  "warning: installed boss skills drift from checkout source",
				},
				{
					path:        "skills/boss-plan/SKILL.md",
					hasHardStop: true,
					stillHard:   `BLOCKED: installed boss skills missing or stale`,
					driftWarns:  "warning: installed boss skills drift from checkout source",
				},
				{
					// boss-repair shares the same drift gate. It carries no missing-install
					// hard stop of its own (it resolves the toolbox by probing both skill
					// homes), so there is no stillHard string to assert here. Its only
					// `BLOCKED:` is the transport stop, which this table does not cover.
					path:        "skills/boss-repair/SKILL.md",
					hasHardStop: false,
					stillHard:   "",
					driftWarns:  "warning: installed boss skills drift from checkout source",
				},
			} {
				tc := tc
				// Subtest per row: the drift needles below are byte-identical across all three
				// rows, and assertContains reports only the needle, so without this a regression
				// in one skill prints a message that cannot say which skill regressed.
				t.Run(tc.path, func(t *testing.T) {
					// The two hard-stop fields must agree, so an emptied needle fails loudly
					// rather than quietly downgrading this row to the no-hard-stop case.
					if tc.hasHardStop && tc.stillHard == "" {
						t.Fatalf("%s: row claims a missing-install hard stop but carries no stillHard needle", tc.path)
					}
					if !tc.hasHardStop && tc.stillHard != "" {
						t.Fatalf("%s: row carries stillHard %q but is marked as having no hard stop", tc.path, tc.stillHard)
					}

					skill := unwrapped(readPayloadFile(t, payload, tc.path))

					// Drift warns and continues...
					assertContains(t, skill, tc.driftWarns)
					assertContains(t, skill, "bookkeeping only, work state unaffected")
					// ...and the old BLOCKED arm is gone from both drift gates...
					assertNotContains(t, skill, "BLOCKED: installed boss skills differ from checkout source")
					// ...including its `exit 1`: warning wording alone would still pass with the
					// hard stop restored underneath it, which is the regression this ticket
					// exists to prevent. The control-flow change IS the ticket's claim.
					assertContains(t, skill, `work state unaffected" >&2 fi ;; esac`)

					// ...but a missing install is still a hard stop, where the skill has one.
					if tc.hasHardStop {
						assertContains(t, skill, tc.stillHard)
					}
				})
			}

			// The toolbox-directory guards stay BLOCKING for the same reason.
			build := unwrapped(readPayloadFile(t, payload, "skills/boss-build/SKILL.md"))
			assertContains(t, build, "BLOCKED: boss-build toolbox not found")
			assertContains(t, build, "**Bookkeeping warns; a missing install blocks.**")

			// A missing helper script is a missing install, not bookkeeping.
			assertContains(t, build, "BLOCKED: bs-run-sentinel.mjs missing")
			plan := unwrapped(readPayloadFile(t, payload, "skills/boss-plan/SKILL.md"))
			assertContains(t, plan, "BLOCKED: bs-run-sentinel.mjs missing")
		})
	}
}

// TestPreflightReportsCliOnlyModeNotDegraded pins honest capability reporting across the three
// skills that share bossEpicTransportPreflight. The helper's `degraded` field name is pinned
// elsewhere by skills-toolbox/session/boss.test.mjs and must survive unchanged; only what the run
// *prints* changes, because three permanently-CLI-less capabilities reported as "degraded" on every
// single run is noise that reads as failure.
func TestPreflightReportsCliOnlyModeNotDegraded(t *testing.T) {
	const cliOnly = "cli-only mode (expected): resolveContext, getSessionStatuses, createPlanningChat"

	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			for _, path := range []string{
				"skills/boss-build/SKILL.md",
				"skills/boss-epic/SKILL.md",
				"skills/boss-repair/SKILL.md",
			} {
				path := path
				// Subtest per path: all three needles below are identical across the three
				// files, so a bare failure could not name the skill that regressed.
				t.Run(path, func(t *testing.T) {
					skill := unwrapped(readPayloadFile(t, payload, path))

					// The expected steady state of a CLI run is reported as expected.
					assertContains(t, skill, cliOnly)

					// ...and the wording it replaced is gone, not merely joined. Without these
					// negatives an edit that re-adds the old line ALONGSIDE the new one passes.
					assertNotContains(t, skill, "degraded: <capabilities>")
					assertNotContains(t, skill, "degraded: <comma-separated capabilities>")

					// "degraded" is reserved for a capability absent on BOTH transports.
					assertContains(t, skill, "for a capability missing from **both** transports")

					// The helper's return shape is untouched — this is a prose-only rename, and
					// renaming the field would break skills-toolbox/session/boss.test.mjs.
					assertContains(t, skill, "degraded, partial, inventoryHint }")
				})
			}
		})
	}
}

// TestReviewTierWordingIsDistinctFromTheCapabilitySense guards the collision the plan's Risks
// section warns about: "degraded" is two different words in boss-build. The capability sense was
// renamed above and still uses it. The REVIEW TIER sense was the separate ticket the original
// version of this test was holding the door open for — BOS-1103 has now landed it, renaming that
// sense "quick" and keying it to the diff instead of a clock. So the guard inverts rather than
// disappears: the tier vocabulary is pinned to its NEW spelling, and the tier's own reference must
// no longer carry the capability sense's word at all. That last negative is what keeps the two
// senses from silently re-merging — a future edit that reaches for "degraded" to mean a reduced
// review reds here.
func TestReviewTierWordingIsDistinctFromTheCapabilitySense(t *testing.T) {
	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			build := unwrapped(readPayloadFile(t, payload, "skills/boss-build/SKILL.md"))
			for _, tier := range []string{
				"The review tier is picked from the diff, not from a clock.",
				"The one pre-dispatch decline is the off switch.",
				"quick: <reason> (skipped: <round list>)",
			} {
				assertContains(t, build, tier)
			}

			// The review-stack reference owns the tier rule. Pin the section anchor, not the bare
			// phrase: "quick tier" occurs many times in this file, so a two-word substring
			// survives renaming all but one of them.
			stack := unwrapped(readPayloadFile(t, payload, "skills/boss-build/references/review-stack.md"))
			assertContains(t, stack, "[**quick** tier](#quick-tier-minimal)")
			assertContains(t, stack, "### Quick tier (minimal)")

			// The capability sense lives in SKILL.md and finalize-and-stop.md, never here. A
			// "degraded" anywhere in the tier reference means the two senses have re-merged.
			assertNotContains(t, stack, "degraded")
		})
	}
}
