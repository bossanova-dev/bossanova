package skillinstall

import "testing"

// BOS-1101 makes contention pay-as-you-go. Two probes that a boss-build run has already performed
// before it posts a claim — Step 1's lock outcome and Step 3's pre-post comment scan — decide
// whether the run pays for arbitration ceremony at all. These pins hold the properties that split
// apart: both fast-path conditions, the routing decision landing BEFORE the post, the re-read that
// both paths keep, the relaxed heartbeat cadence, the contended path's ceremony staying whole, and
// the malformed-evidence hard error surviving there.
//
// The pins are deliberately two-sided. A fast path that is cheap because it also skips the
// contended ceremony is a correctness regression, not a saving, so every fast-path pin is paired
// with one that fails when the contended branch loses a component.
var falsificationClaimFastPathPins = regProsePins([]falsificationProsePin{
	{
		// Both probes must be required. A fast path gated on the lock alone would take it while a
		// peer in ANOTHER worktree holds the claim (the lock is per-worktree); gated on the comment
		// scan alone it would take it after stealing a crashed run's worktree, which is exactly when
		// arbitration evidence matters.
		name:         "build-claim-fast-path-two-conditions",
		pattern:      `fresh\s+.ACQUIRED.\s+\*\*and\*\*\s+zero\s+peer\s+claim\s+comments\s+is\s+the\s+uncontended\s+fast\s+path[\s\S]{0,60}skip\s+the\s+liveness\s+snippet\s+and\s+both\s+waits`,
		live:         "fresh `ACQUIRED` **and** zero peer claim comments is the uncontended fast path — skip the liveness snippet and both waits.",
		tokenRemoved: "fresh `ACQUIRED` is the uncontended fast path — skip the liveness snippet and both waits.",
		alsoRemoved: []string{
			"zero peer claim comments is the uncontended fast path — skip the liveness snippet and both waits.",
			"fresh `ACQUIRED` **and** zero peer claim comments is the uncontended fast path — skip the liveness snippet.",
		},
	},
	{
		// The route has to be decided BEFORE the claim goes up. The contended branch's first act is
		// gathering liveness evidence, and a run that posts first has already flipped .inProgress on
		// a ticket it may not own by the time it discovers contention. A top-down executor follows
		// the order the prose is written in, so the order IS the behaviour.
		name:         "build-claim-route-before-post",
		pattern:      `Route\s+before\s+posting[\s\S]{0,400}Then\s+post\s+.\$BODY.\s+via\s+.writeComment.,\s+set\s+\.inProgress`,
		live:         "Route before posting: fresh `ACQUIRED` **and** zero peer claim comments is the fast path. Then post `$BODY` via `writeComment`, set .inProgress.",
		tokenRemoved: "After posting, route: fresh `ACQUIRED` **and** zero peer claim comments is the fast path. Then post `$BODY` via `writeComment`, set .inProgress.",
		alsoRemoved: []string{
			"Route before posting: fresh `ACQUIRED` **and** zero peer claim comments is the fast path.",
		},
	},
	{
		// The re-read is the one thing the fast path must NOT drop. `claim-verdict` over the
		// pre-post comment set contains no claims at all and answers NO_WINNER, so a fast path
		// without the re-read never reaches WON; and a run that reads its own claim into a pre-post
		// set has two concurrent runs each adjudicating a set of one and both concluding WON.
		name:         "build-claim-reread-after-post",
		pattern:      `Both\s+paths\s+re-read\s+.\$COMMENTS_JSON.\s+after\s+posting\*\*[\s\S]{0,60}drops\s+the\s+waits,\s+not\s+the\s+re-read`,
		live:         "**Both paths re-read `$COMMENTS_JSON` after posting** — the fast path drops the waits, not the re-read:",
		tokenRemoved: "**The contended path re-reads `$COMMENTS_JSON` after posting** — the fast path drops the waits, not the re-read:",
		alsoRemoved: []string{
			"**Both paths re-read `$COMMENTS_JSON` after posting** — the fast path drops the waits and the re-read:",
		},
	},
	{
		// The liveness guard is what the fast path is allowed to skip, so it has to be BRANCHED, not
		// unconditional: an unconditional `test -n "$BOSS_CLAIM_LIVENESS_JSON"` hard-stops every
		// uncontended run at BLOCKED, because the fast path never runs the snippet that sets it.
		// The branch's SENSE is the other half. `UNCONTENDED` is asserted only by the cheap path, so
		// an absent variable — a separate shell, a dropped export — selects the ceremony and then
		// hard-errors on the missing evidence, exactly as an unbranched guard did before the split.
		// The `-n CONTENDED` spelling inverts that: it makes silence mean "no contention" and lets a
		// genuinely contended run arbitrate with no liveness evidence, which is the one weakening of
		// the contended path this whole change forbids. The empty argument vector (`set --`) is the
		// third load-bearing piece: it leaves the fast path's call `--liveness`-free.
		name:         "build-claim-liveness-guard-fails-closed",
		pattern:      `set\s+--\s+if\s+\[\s+-z\s+"\$\{UNCONTENDED:-\}"\s+\];\s+then[\s\S]{0,240}BOSS_CLAIM_LIVENESS_JSON:-[\s\S]{0,200}set\s+--\s+--liveness`,
		live:         "set --\nif [ -z \"${UNCONTENDED:-}\" ]; then\n  test -n \"${BOSS_CLAIM_LIVENESS_JSON:-}\" || exit 1\n  set -- --liveness \"$BOSS_CLAIM_LIVENESS_JSON\"\nfi",
		tokenRemoved: "test -n \"${BOSS_CLAIM_LIVENESS_JSON:-}\" || exit 1\nset -- --liveness \"$BOSS_CLAIM_LIVENESS_JSON\"",
		alsoRemoved: []string{
			"set --\nif [ -z \"${UNCONTENDED:-}\" ]; then\n  set -- --liveness \"$BOSS_CLAIM_LIVENESS_JSON\"\nfi",
			"set --\nif [ -n \"${CONTENDED:-}\" ]; then\n  test -n \"${BOSS_CLAIM_LIVENESS_JSON:-}\" || exit 1\n  set -- --liveness \"$BOSS_CLAIM_LIVENESS_JSON\"\nfi",
		},
	},
	{
		// The other half of the split: contention keeps today's ceremony entire, and the resident
		// body must say so while pointing at the reference that enumerates the components. Three
		// parts of the sentence are load-bearing. `UNCONTENDED=1` names the flag the guard reads
		// and pins the direction — the cheap path is the one that must be asserted. "run the block
		// below with" gives that assertion a REFERENT: the flag has no producer anywhere in the
		// payload, so an antecedent-free "in that shell" lets an executor set it in one Bash call
		// and run the guard in the next, where the fast path — which gathered no liveness JSON —
		// hard-errors instead of claiming. And "Else" pins the ceremony as the DEFAULT rather than
		// an alternative branch, so a run that never evaluated the probes still pays for
		// arbitration. Losing "full ... unweakened" turns the fast path into the only path.
		name:         "build-claim-contended-ceremony-whole",
		pattern:      `run\s+the\s+block\s+below\s+with\s+.UNCONTENDED=1.\s+set\.\s+Else\s+the\s+full\s+ceremony\s+stays\s+unweakened`,
		live:         "skip the liveness snippet and both waits; run the block below with `UNCONTENDED=1` set. Else the full ceremony stays unweakened: same-shell inline liveness snippet, 20s inline.",
		tokenRemoved: "skip the liveness snippet and both waits; run the block below with `UNCONTENDED=1` set. Else the ceremony is relaxed: same-shell inline liveness snippet, 20s inline.",
		alsoRemoved: []string{
			"skip the liveness snippet and both waits; run the block below with `CONTENDED=1` set. Else the full ceremony stays unweakened: same-shell inline liveness snippet, 20s inline.",
			"skip the liveness snippet and both waits; set `UNCONTENDED=1` in that shell. Else the full ceremony stays unweakened: same-shell inline liveness snippet, 20s inline.",
			"skip the liveness snippet and both waits. The full ceremony stays unweakened: same-shell inline liveness snippet, 20s inline.",
		},
	},
	{
		// The resident enumeration is read top-down by a literal executor, so its ORDER is its
		// behaviour. "liveness snippet, 20s inline" alone reads as gather-then-wait-then-post, which
		// spends the wait before there is anything to settle and leaves no post-post window at all —
		// the opposite of what the contended ceremony is for, and it contradicts the reference's own
		// ordered list. The two position words are the whole content of this pin.
		name:         "build-claim-contended-wait-after-post",
		pattern:      `liveness\s+snippet\s+before\s+the\s+post,\s+20s\s+inline\s+after\s+it`,
		live:         "same-shell inline liveness snippet before the post, 20s inline after it, malformed evidence hard-errors.",
		tokenRemoved: "same-shell inline liveness snippet, 20s inline, malformed evidence hard-errors.",
		alsoRemoved: []string{
			"same-shell inline liveness snippet before the post, 20s inline before it, malformed evidence hard-errors.",
			"same-shell inline liveness snippet after the post, 20s inline after it, malformed evidence hard-errors.",
		},
	},
	{
		// The pre-post verdict is the one that decides whether this run posts at all, and BOS-1101
		// split the liveness snippet out of the resident pre-post enumeration when it made the
		// snippet contended-only. Unevidenced, `claim-verdict` forfeits nothing — `isForfeited`
		// returns false the moment `options` is null — so on a `TOOK_OVER_STALE` resume the crashed
		// prior run's older claim wins the pre-post verdict, the mapping sends the run to
		// `NO_CHANGE`, and the resume the contended path exists to serve is silently dropped. The
		// resident enumeration therefore has to say the pre-post verdict is evidenced, and say it is
		// evidenced only where evidence exists: an unqualified `--liveness` here would tell the fast
		// path to pass a variable it deliberately never gathered.
		name:         "build-claim-prepost-verdict-evidenced",
		pattern:      `Pre-post:\s+.readComments.\s+\+\s+claim-verdict\s+\(contended\s+.--liveness.;`,
		live:         "Pre-post: `readComments` + claim-verdict (contended `--liveness`; 3=NO_CHANGE, 4=cleanup+post, other=BLOCKED).",
		tokenRemoved: "Pre-post: `readComments` + claim-verdict (3=NO_CHANGE, 4=cleanup+post, other=BLOCKED).",
		alsoRemoved: []string{
			"Pre-post: `readComments` + claim-verdict (`--liveness`; 3=NO_CHANGE, 4=cleanup+post, other=BLOCKED).",
			"Pre-post: `readComments` + claim-verdict (contended liveness; 3=NO_CHANGE, 4=cleanup+post, other=BLOCKED).",
			"Pre-post: `readComments` + liveness snippet + claim-verdict (3=NO_CHANGE, 4=cleanup+post, other=BLOCKED).",
		},
	},
	{
		// The cadence relaxation is only sound because it still fires at the long phases where a
		// run can go quiet for an hour. Dropping Step 6 from the list is the plausible edit, so the
		// pattern names all three.
		name:         "build-claim-relaxed-heartbeat-cadence",
		pattern:      `long\s+phases\s+\(Steps\s+5\s+implement,\s+6\s+review,\s+8\s+repair\)[\s\S]{0,80}on\s+the\s+uncontended\s+fast\s+path,\s+at\s+long\s+phases\s+only`,
		live:         "at each step boundary and at the top of long phases (Steps 5 implement, 6 review, 8 repair) — on the uncontended fast path, at long phases only.",
		tokenRemoved: "at each step boundary and at the top of long phases (Steps 5 implement, 6 review, 8 repair).",
		alsoRemoved: []string{
			"at each step boundary and at the top of long phases (Steps 5 implement, 8 repair) — on the uncontended fast path, at long phases only.",
		},
	},
	{
		// Scoping the hard error to the contended path is the edit that could quietly delete it.
		// The pin fails both when the stop is replaced by a fallback and when the scoping sentence
		// loses the contended qualifier that keeps it addressed to a real branch.
		name:         "build-claim-malformed-evidence-hard-error",
		pattern:      `Malformed\s+evidence\s+is\s+a\s+hard\s+error\s+on\s+the\s+contended\s+path[\s\S]{0,40}stop\s+the\s+run\s+rather\s+than\s+falling\s+back\s+to\s+first-writer-wins`,
		live:         "Malformed evidence is a hard error on the contended path: stop the run rather than falling back to first-writer-wins without liveness.",
		tokenRemoved: "Malformed evidence is a hard error on the contended path: fall back to first-writer-wins without liveness.",
		alsoRemoved: []string{
			"Malformed evidence is a hard error: stop the run rather than falling back to first-writer-wins without liveness.",
		},
	},
})

// falsificationClaimContractPins hold the reviewable part of the change. Skipping the waits is safe
// only because of a specific ordering argument with a stated residual; if the prose keeps the fast
// path but loses the argument, a later reader has a cheap rule with no way to evaluate it, which is
// how a correct optimisation degrades into a cargo-culted one.
//
// The argument these pins hold is deliberately NOT "the race cannot happen". An earlier draft
// argued that a run which read the comment set as empty "cannot lose a race it has not entered" and
// that "the peer's own verdict pass reads our comment" resolves ownership. Both are false: a peer
// can post between our scan and our post, and a peer resolving ownership on ITS pass does nothing
// about the verdict WE reach on ours — without a re-read both runs conclude WON. What is true is an
// ordering property plus a bounded residual, and that is what is pinned.
var falsificationClaimContractPins = regProsePins([]falsificationProsePin{
	{
		name:         "build-claim-probe-first-two-probes",
		pattern:      `Lock\s+probe[\s\S]{0,220}TOOK_OVER_STALE[\s\S]{0,80}HELD_BY_PEER[\s\S]{0,200}Claim\s+probe[\s\S]{0,200}zero\s+peer\s+claim\s+comments`,
		live:         "1. **Lock probe** — Step 1's `worktree-lock.sh acquire` returned a fresh `ACQUIRED`, not `TOOK_OVER_STALE` and not `HELD_BY_PEER`. 2. **Claim probe** — Step 3's pre-post `readComments` scan found zero peer claim comments on this ticket.",
		tokenRemoved: "1. **Lock probe** — Step 1's `worktree-lock.sh acquire` returned a fresh `ACQUIRED`, not `TOOK_OVER_STALE` and not `HELD_BY_PEER`.",
		alsoRemoved: []string{
			"2. **Claim probe** — Step 3's pre-post `readComments` scan found zero peer claim comments on this ticket.",
		},
	},
	{
		// The contract has to state WHY the route is decided before the post, or the ordering reads
		// as an accident of how the sentence was written and the next editor moves it.
		name:         "build-claim-contract-route-before-post",
		pattern:      `Route\s+on\s+the\s+two\s+probes\s+\*\*before\*\*\s+posting[\s\S]{0,300}has\s+to\s+happen\s+before\s+the\s+claim\s+goes\s+up`,
		live:         "Route on the two probes **before** posting, because the contended path's first act — gathering liveness evidence — has to happen before the claim goes up, not after.",
		tokenRemoved: "Route on the two probes after posting, because the contended path's first act — gathering liveness evidence — has to happen before the claim goes up, not after.",
		alsoRemoved: []string{
			"Route on the two probes **before** posting, because the contended path is more expensive.",
		},
	},
	{
		// Why the re-read survives the fast path, stated as the two concrete failures it prevents.
		name:         "build-claim-reread-not-skippable",
		pattern:      `re-read\s+is\s+not\s+ceremony\s+and\s+is\s+not\s+skippable[\s\S]{0,200}answers\s+.NO_WINNER.\s+\(exit\s+4\)[\s\S]{0,300}both\s+conclude\s+.WON.`,
		live:         "The re-read is not ceremony and is not skippable. `claim-verdict` over the pre-post set has no claims in it at all and answers `NO_WINNER` (exit 4) — the fast path would never reach `WON`. Worse, a run that reads its own claim into the pre-post set has each of two concurrent runs adjudicating a set of exactly one comment, its own, and both conclude `WON`.",
		tokenRemoved: "The re-read is an optimisation. `claim-verdict` over the pre-post set has no claims in it at all and answers `NO_WINNER` (exit 4) — the fast path would never reach `WON`. Worse, a run that reads its own claim into the pre-post set has each of two concurrent runs adjudicating a set of exactly one comment, its own, and both conclude `WON`.",
		alsoRemoved: []string{
			"The re-read is not ceremony and is not skippable. `claim-verdict` over the pre-post set has no claims in it at all and answers `NO_WINNER` (exit 4) — the fast path would never reach `WON`.",
		},
	},
	{
		// The ordering property that replaces the false race-freedom claim.
		name:         "build-claim-ordering-argument",
		pattern:      `Every\s+run\s+posts\s+before\s+it\s+re-reads[\s\S]{0,400}later\s+poster\s+sees\s+the\s+earlier\s+claim\s+and\s+loses`,
		live:         "Every run posts before it re-reads. So each run's post-post set contains every claim posted before its own post, and arbitration is first-writer-wins by `createdAt`: the later poster sees the earlier claim and loses; the earlier poster wins either way.",
		tokenRemoved: "Every run re-reads before it posts. So each run's post-post set contains every claim posted before its own post, and arbitration is first-writer-wins by `createdAt`: the later poster sees the earlier claim and loses; the earlier poster wins either way.",
		alsoRemoved: []string{
			"Every run posts before it re-reads, and arbitration is first-writer-wins by `createdAt`.",
		},
	},
	{
		// The residual has to stay stated. A fast path whose prose claims safety without naming the
		// window it trades away is the cargo-cult outcome these pins exist to prevent.
		name:         "build-claim-residual-stated",
		pattern:      `unless\s+\*\*both\*\*\s+re-reads\s+land\s+inside\s+the[\s\S]{0,120}read-after-write\s+lag[\s\S]{0,300}bounded\s+window,\s+not\s+a\s+proof\s+of\s+absence`,
		live:         "Two concurrent first claims therefore agree on the winner unless **both** re-reads land inside the tracker's read-after-write lag for the other's comment. The timed waits bought tolerance for exactly that lag — that is the whole trade, and it is a bounded window, not a proof of absence.",
		tokenRemoved: "Two concurrent first claims therefore always agree on the winner. The timed waits bought tolerance for read-after-write lag — that is the whole trade, and it is a bounded window, not a proof of absence.",
		alsoRemoved: []string{
			"Two concurrent first claims therefore agree on the winner unless **both** re-reads land inside the tracker's read-after-write lag for the other's comment.",
		},
	},
	{
		// What bounds the residual: exit 4's delete-and-retry-once is the fail-closed direction, and
		// the worktree mutex has already excluded a same-worktree peer.
		name:         "build-claim-residual-bounds",
		pattern:      `exit\s+4,\s+whose\s+handling\s+is\s+delete-and-retry-once[\s\S]{0,200}fails\s+closed[\s\S]{0,300}clean\s+.ACQUIRED.\s+on\s+the\s+worktree\s+mutex`,
		live:         "A re-read that misses even our own claim yields exit 4, whose handling is delete-and-retry-once, then stop `NO_CHANGE` — the visibility-lag case that touches our own comment fails closed. And the fast path is only ever entered from a clean `ACQUIRED` on the worktree mutex.",
		tokenRemoved: "A re-read that misses even our own claim yields exit 4, whose handling is delete-and-retry-once, then stop `NO_CHANGE` — the visibility-lag case that touches our own comment fails closed.",
		alsoRemoved: []string{
			"A re-read that misses even our own claim yields exit 0 — the visibility-lag case that touches our own comment fails closed. And the fast path is only ever entered from a clean `ACQUIRED` on the worktree mutex.",
		},
	},
	{
		// The contended path's components, in the order a run executes them. The resident body
		// still names the pre-post verdict, the liveness snippet and the 20s wait, but only this
		// list puts every component in execution order, so losing one here is a silent weakening.
		// Two orderings matter and both are pinned. The pre-post `claim-verdict` has to be in the
		// list at all -- it is what stops a run that would LOSE from posting a claim and flipping
		// .inProgress. And it has to come AFTER the liveness gather: unevidenced it forfeits
		// nothing, so a crashed prior run's older claim wins it and the run stops NO_CHANGE,
		// which silently deletes the TOOK_OVER_STALE resume the contended path exists to serve.
		name:         "build-claim-contended-order",
		pattern:      `the\s+pre-post\s+.readComments.,\s+liveness\s+evidence\s+gathered\s+\*\*before\*\*\s+the\s+post,\s+the\s+pre-post\s+.claim-verdict\s+--liveness.[\s\S]{0,120}20s\s+inline\s+wait\s+after\s+the\s+post[\s\S]{0,120}re-read[\s\S]{0,200}~10s\s+timed\s+confirm[\s\S]{0,200}malformed\s+evidence\s+a\s+hard\s+error`,
		live:         "Run the full ceremony unchanged, in this order: the pre-post `readComments`, liveness evidence gathered **before** the post, the pre-post `claim-verdict --liveness`, the post, the 20s inline wait after the post, the re-read, `claim-verdict --liveness`, the post-`WON` ~10s timed confirm with a fresh liveness snapshot, and stale-claim cleanup — with malformed evidence a hard error throughout.",
		tokenRemoved: "Run the full ceremony unchanged, in this order: the pre-post `readComments`, liveness evidence gathered **before** the post, the pre-post `claim-verdict --liveness`, the post, the re-read, `claim-verdict --liveness`, the post-`WON` ~10s timed confirm with a fresh liveness snapshot, and stale-claim cleanup — with malformed evidence a hard error throughout.",
		alsoRemoved: []string{
			"Run the full ceremony unchanged, in this order: the pre-post `readComments`, liveness evidence gathered **before** the post, the pre-post `claim-verdict --liveness`, the post, the 20s inline wait after the post, the re-read, `claim-verdict --liveness`, and stale-claim cleanup.",
			"Run the full ceremony unchanged, in this order: the pre-post `readComments`, liveness evidence gathered **before** the post, the pre-post `claim-verdict`, the post, the 20s inline wait after the post, the re-read, `claim-verdict --liveness`, the post-`WON` ~10s timed confirm with a fresh liveness snapshot, and stale-claim cleanup — with malformed evidence a hard error throughout.",
			"Run the full ceremony unchanged, in this order: the pre-post `readComments` + `claim-verdict`, liveness evidence gathered **before** the post, the post, the 20s inline wait after the post, the re-read, `claim-verdict --liveness`, the post-`WON` ~10s timed confirm with a fresh liveness snapshot, and stale-claim cleanup — with malformed evidence a hard error throughout.",
			"Run the full ceremony unchanged, in this order: liveness evidence gathered **before** the post, the post, the 20s inline wait after the post, the re-read, `claim-verdict --liveness`, the post-`WON` ~10s timed confirm with a fresh liveness snapshot, and stale-claim cleanup — with malformed evidence a hard error throughout.",
		},
	},
	{
		// The cadence rule is stated where the contract is, not only in the resident body. Written
		// as a pattern rather than a literal so a reflow of the paragraph cannot red it: the earlier
		// spelling hard-coded the line wrap between "Step" and "5 implement".
		name:         "build-claim-contract-cadence",
		pattern:      `uncontended\s+fast\s+path,\s+refresh\s+at\s+long-phase\s+boundaries\s+only\s+.\s+Step\s+5\s+implement,\s+Step\s+6\s+review,\s+Step\s+8\s+repair`,
		live:         "On the uncontended fast path, refresh at long-phase boundaries only — Step 5 implement, Step 6 review, Step 8 repair.",
		tokenRemoved: "On the uncontended fast path, refresh at long-phase boundaries only — Step 5 implement, Step 8 repair.",
		alsoRemoved: []string{
			"On the uncontended fast path, refresh at every step boundary — Step 5 implement, Step 6 review, Step 8 repair.",
		},
	},
	{
		// AC 4 is a verify-only audit whose whole value is the recorded arithmetic. A contract that
		// asserts the fit without saying where the numbers are cannot be re-checked when either the
		// cadence or `STALE_SECS` moves.
		name:         "build-claim-cadence-arithmetic",
		pattern:      `takeover-eligible\s+once\s+.now\s+-\s+eff_heartbeat.\s+reaches\s+.STALE_SECS.\s+\(18000\s+s[\s\S]{0,200}worst\s+gap\s+is\s+14400\s+s\.\s+14400\s+<\s+18000`,
		live:         "A lock becomes takeover-eligible once `now - eff_heartbeat` reaches `STALE_SECS` (18000 s — 5 h — compared strictly with `-lt`), while the phase cap bounds any single long-phase interval at ~4 h, so the worst gap is 14400 s. 14400 < 18000 leaves a margin of 3600 s.",
		tokenRemoved: "A lock becomes takeover-eligible once `now - eff_heartbeat` reaches `STALE_SECS`, and the relaxed cadence stays inside that tolerance.",
		alsoRemoved: []string{
			"A lock becomes takeover-eligible once `now - eff_heartbeat` reaches `STALE_SECS` (18000 s — 5 h — compared strictly with `-lt`), and the relaxed cadence fits.",
		},
	},
	{
		// TOOK_OVER_STALE staying contended is the non-obvious half of the rule, and "do not weaken"
		// is the instruction the rest of the contract leans on. Both are matched as patterns rather
		// than literal substrings: hard-coding a line wrap makes a reflow of this paragraph red the
		// gate for a reason that has nothing to do with the property.
		name:         "build-claim-contended-not-weakened",
		pattern:      `Do\s+not\s+weaken\s+the\s+contended\s+path\s+in\s+any\s+way\.[\s\S]{0,80}.TOOK_OVER_STALE.\s+stays\s+on\s+it\s+deliberately`,
		live:         "Do not weaken the contended path in any way. `TOOK_OVER_STALE` stays on it deliberately — a crashed prior run is exactly when arbitration evidence matters most.",
		tokenRemoved: "Do not weaken the contended path in any way. `TOOK_OVER_STALE` may take the fast path — a crashed prior run leaves nothing to arbitrate.",
		alsoRemoved: []string{
			"`TOOK_OVER_STALE` stays on it deliberately — a crashed prior run is exactly when arbitration evidence matters most.",
		},
	},
})

// claimPin looks a pin up by name. Index-based selection out of the slices above breaks silently
// when a pin is inserted: the wrong pin is asserted against the wrong prose window, and because
// every pin here matches SOMETHING in the payload, the mistake can stay green.
func claimPin(t *testing.T, name string) falsificationProsePin {
	t.Helper()
	for _, pins := range [][]falsificationProsePin{falsificationClaimFastPathPins, falsificationClaimContractPins} {
		for _, pin := range pins {
			if pin.name == name {
				return pin
			}
		}
	}
	t.Fatalf("no claim fast-path pin named %q", name)
	return falsificationProsePin{}
}

func claimPins(t *testing.T, names ...string) []falsificationProsePin {
	t.Helper()
	pins := make([]falsificationProsePin, 0, len(names))
	for _, name := range names {
		pins = append(pins, claimPin(t, name))
	}
	return pins
}

// TestBossBuildUncontendedClaimFastPath asserts the split in both shipped payloads, in the windows
// a run actually reads: Step 1 for the cadence, Step 3 for the routing decision, and the claim
// reference for the contract, the argument, and the contended requirements.
func TestBossBuildUncontendedClaimFastPath(t *testing.T) {
	const skillPath = "skills/boss-build/SKILL.md"
	const refPath = "skills/boss-build/references/claim-and-eligibility.md"

	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			skill := readPayloadFile(t, payload, skillPath)
			ref := readPayloadFile(t, payload, refPath)

			step1 := sectionBetween(t, skill, "## Step 1: Acquire the worktree lock (simplified)", "## Step 2: Select one ticket")
			step3 := sectionBetween(t, skill, "## Step 3: Claim (cross-worktree arbitration via the tracker claim capability)", "## Step 4: Fetch + validate plan, copy to docs/plans/")

			t.Run("step1-cadence", func(t *testing.T) {
				assertFalsificationPins(t, step1, claimPins(t, "build-claim-relaxed-heartbeat-cadence"))
				// The contended cadence is the thing being relaxed FROM; if it disappears, the
				// relaxation has silently become the only rule.
				assertContains(t, step1, "at each step boundary and at the top of long phases")
			})

			t.Run("step3-routing", func(t *testing.T) {
				assertFalsificationPins(t, step3, claimPins(t,
					"build-claim-fast-path-two-conditions",
					"build-claim-route-before-post",
					"build-claim-reread-after-post",
					"build-claim-liveness-guard-fails-closed",
					"build-claim-contended-ceremony-whole",
					"build-claim-contended-wait-after-post",
					"build-claim-prepost-verdict-evidenced",
				))
				// The lock probe is only meaningful against the Step 1 outcomes it names, and the
				// executable verdict call has to stay resident on the contended branch.
				assertContains(t, step3, "claim-verdict")
			})

			t.Run("reference-contract", func(t *testing.T) {
				contract := sectionBetween(t, ref, "## Probe-First Contract", "## Selection-Time Eligibility")
				assertFalsificationPins(t, contract, falsificationClaimContractPins)
				// Both branches must be named in the contract itself, so a reader who lands here
				// from Step 3 can tell which one they are on.
				assertContains(t, contract, "**Uncontended fast path**")
				assertContains(t, contract, "**Contended path**")
			})

			t.Run("reference-contended-requirements", func(t *testing.T) {
				liveness := sectionBetween(t, ref, "## Liveness Evidence Before Claim Verdict", "## After WON: Link The Session")
				assertFalsificationPins(t, liveness, claimPins(t, "build-claim-malformed-evidence-hard-error"))
				// The section has to say which branch it belongs to; an ungated snippet is what the
				// fast path was supposed to stop paying for.
				assertContains(t, liveness, "skip it in full on the uncontended fast path")
				// Everything the contended path still owes stays here.
				assertContains(t, liveness, "BOSS_CLAIM_LIVENESS_JSON")
				assertContains(t, liveness, "--liveness \"$BOSS_CLAIM_LIVENESS_JSON\"")
			})

			t.Run("lock-signal-stays-non-decisive", func(t *testing.T) {
				lock := sectionBetween(t, ref, "## Lock Signal", "## Liveness Evidence Before Claim Verdict")
				// A fresh ACQUIRED is now load-bearing as a probe, which is exactly the pressure
				// that could promote it to a verdict about a dead prior run. Both halves stay.
				assertContains(t, lock, "never as decisive")
				assertContains(t, lock, "is the first probe of the Probe-First Contract")
				assertContains(t, lock, "still not evidence about whether a prior run in this worktree died or finished")
			})
		})
	}
}
