package skillinstall

import (
	"io/fs"
	"strings"
	"testing"
)

// TestBossBuildStep7UsesVerifiedPushProcedure keeps the normal PR-gate push on
// the same durable path as the pre-dispatch decline route. A one-shot push can fail and
// still let later terminal cleanup write BLOCKED, stranding completed commits
// in the worktree.
func TestBossBuildStep7UsesVerifiedPushProcedure(t *testing.T) {
	var canonical string
	for label, payload := range shippedPayloads(t) {
		data, err := fs.ReadFile(payload, "skills/boss-build/SKILL.md")
		if err != nil {
			t.Fatalf("%s: read boss-build skill: %v", label, err)
		}

		skill := string(data)
		if canonical == "" {
			canonical = skill
		} else if skill != canonical {
			t.Errorf("%s: boss-build skill differs from embedded payload", label)
		}

		step7 := sectionBetween(t, skill, "## Step 7: PR gate (create/reuse)", "## Steps 8-12:")
		assertContains(t, step7, "retry/rebase/rescue procedure")
		assertContains(t, step7, "references/review-stack.md")
		assertContains(t, step7, "Continue only when it sets `PUSHED=yes`")
		assertContains(t, step7, "`PUSHED=rescue` or `PUSHED=no` means the session")
		assertContains(t, step7, "Stop cleanly** `BLOCKED`")
		assertNotContains(t, step7, "git push -u origin \"$SESSION_BRANCH\"")
	}
}

// falsificationBotReviewPins are the source-partition sentences that must appear wherever a run
// decides whether an automated reviewer's feedback opens a fix cycle. They are asserted against
// three separate files (boss-build's receiving-code-review.md and finalize-and-stop.md, and
// boss-repair's SKILL.md) so the rule cannot survive in one place while quietly rotting in another.
//
// Every pattern is whitespace-tolerant (`\s+`, never a literal space) so a prettier rewrap cannot
// silently unpin the sentence, and every pin carries independent single-anchor mutations that must
// NOT match — a pin that has never been red proves nothing.
var falsificationBotReviewPins = regProsePins([]falsificationProsePin{
	{
		name:    "bot-detection-is-generic",
		pattern: "Identify\\s+an\\s+automated\\s+reviewer\\s+generically\\s+from\\s+the\\s+review\\s+author,\\s+never\\s+from\\s+a\\s+list\\s+of\\s+product\\s+names:\\s+the\\s+GraphQL\\s+author's\\s+`isBot`\\s+field,\\s+the\\s+REST\\s+author's\\s+`\"type\":\\s+\"Bot\"`\\s+value,\\s+or\\s+a\\s+login\\s+ending\\s+in\\s+the\\s+`\\[bot\\]`\\s+suffix\\.\\s+Any\\s+one\\s+signal\\s+is\\s+sufficient\\.",
		live:    "Identify an automated reviewer generically from the review author, never from a list of product names: the GraphQL author's `isBot` field, the REST author's `\"type\": \"Bot\"` value, or a login ending in the `[bot]` suffix. Any one signal is sufficient.",
		// The REST signal is dropped; the other two anchors survive.
		tokenRemoved: "Identify an automated reviewer generically from the review author, never from a list of product names: the GraphQL author's `isBot` field, the REST author's account kind, or a login ending in the `[bot]` suffix. Any one signal is sufficient.",
		alsoRemoved: []string{
			// GraphQL signal dropped.
			"Identify an automated reviewer generically from the review author, never from a list of product names: the GraphQL author's bot flag, the REST author's `\"type\": \"Bot\"` value, or a login ending in the `[bot]` suffix. Any one signal is sufficient.",
			// Login-suffix signal dropped.
			"Identify an automated reviewer generically from the review author, never from a list of product names: the GraphQL author's `isBot` field, the REST author's `\"type\": \"Bot\"` value, or a login on the automation roster. Any one signal is sufficient.",
			// The "never from a list of product names" prohibition dropped — a named-bot allowlist.
			"Identify an automated reviewer generically from the review author or from a list of product names: the GraphQL author's `isBot` field, the REST author's `\"type\": \"Bot\"` value, or a login ending in the `[bot]` suffix. Any one signal is sufficient.",
			// "Any one signal is sufficient" dropped — three signals become conjunctive.
			"Identify an automated reviewer generically from the review author, never from a list of product names: the GraphQL author's `isBot` field, the REST author's `\"type\": \"Bot\"` value, or a login ending in the `[bot]` suffix. All three signals are required.",
		},
	},
	{
		name:         "bot-acknowledge-once-contract",
		pattern:      "It\\s+gets\\s+exactly\\s+one\\s+grouped\\s+response\\s+comment\\s+per\\s+bot\\s+review,\\s+posted\\s+within\\s+the\\s+bot's\\s+own\\s+threads,\\s+carrying\\s+a\\s+per-finding\\s+reason\\s+for\\s+every\\s+finding\\s+it\\s+raised\\s+—\\s+never\\s+a\\s+blanket\\s+dismissal,\\s+and\\s+never\\s+silence\\.",
		live:         "It gets exactly one grouped response comment per bot review, posted within the bot's own threads, carrying a per-finding reason for every finding it raised — never a blanket dismissal, and never silence.",
		tokenRemoved: "It gets exactly one grouped response comment per bot review, posted within the bot's own threads, carrying a summary of the outcome — never a blanket dismissal, and never silence.",
		alsoRemoved: []string{
			// "exactly one grouped" dropped — the once-per-review bound goes with it.
			"It gets a response comment per bot review, posted within the bot's own threads, carrying a per-finding reason for every finding it raised — never a blanket dismissal, and never silence.",
			// Thread placement dropped — the reply escapes to a top-level PR comment.
			"It gets exactly one grouped response comment per bot review, posted as a top-level PR comment, carrying a per-finding reason for every finding it raised — never a blanket dismissal, and never silence.",
			// The blanket-dismissal prohibition dropped.
			"It gets exactly one grouped response comment per bot review, posted within the bot's own threads, carrying a per-finding reason for every finding it raised — never silence.",
		},
	},
	{
		name:         "bot-shortcut-is-verdict-gated",
		pattern:      "The\\s+shortcut\\s+is\\s+verdict-gated:\\s+only\\s+a\\s+verdict\\s+positively\\s+recorded\\s+as\\s+`clean`\\s+unlocks\\s+it;\\s+`capped`,\\s+`none`,\\s+or\\s+an\\s+absent\\s+record\\s+means\\s+bot\\s+feedback\\s+is\\s+triaged\\s+exactly\\s+as\\s+today\\.",
		live:         "The shortcut is verdict-gated: only a verdict positively recorded as `clean` unlocks it; `capped`, `none`, or an absent record means bot feedback is triaged exactly as today.",
		tokenRemoved: "The shortcut is verdict-gated: only a verdict positively recorded as `clean` unlocks it; a later verdict means bot feedback is triaged exactly as today.",
		alsoRemoved: []string{
			// The `clean` requirement dropped — any recorded verdict would unlock the shortcut.
			"The shortcut is verdict-gated: only a recorded verdict unlocks it; `capped`, `none`, or an absent record means bot feedback is triaged exactly as today.",
			// The absent-record case dropped — a missing run note would fall through unclassified.
			"The shortcut is verdict-gated: only a verdict positively recorded as `clean` unlocks it; `capped` or `none` means bot feedback is triaged exactly as today.",
			// The fallback behaviour inverted.
			"The shortcut is verdict-gated: only a verdict positively recorded as `clean` unlocks it; `capped`, `none`, or an absent record means bot feedback is advisory anyway.",
		},
	},
	{
		name:         "bot-advisory-still-fixes-real-defects",
		pattern:      "Advisory\\s+is\\s+not\\s+ignored:\\s+a\\s+bot\\s+finding\\s+that\\s+names\\s+a\\s+real\\s+defect\\s+is\\s+still\\s+fixed\\s+—\\s+advisory\\s+means\\s+it\\s+does\\s+not\\s+mechanically\\s+open\\s+a\\s+fix\\s+cycle,\\s+not\\s+that\\s+the\\s+finding\\s+is\\s+dropped\\.",
		live:         "Advisory is not ignored: a bot finding that names a real defect is still fixed — advisory means it does not mechanically open a fix cycle, not that the finding is dropped.",
		tokenRemoved: "Advisory is not ignored: a bot finding that names a real defect is deferred to a later run — advisory means it does not mechanically open a fix cycle, not that the finding is dropped.",
		alsoRemoved: []string{
			// The "not that the finding is dropped" clause removed — advisory collapses to ignored.
			"Advisory is not ignored: a bot finding that names a real defect is still fixed — advisory means it does not mechanically open a fix cycle.",
			// The mechanical-fix-cycle clause removed — the whole point of the shortcut goes.
			"Advisory is not ignored: a bot finding that names a real defect is still fixed, not that the finding is dropped.",
		},
	},
})

// falsificationSettleLoopPins pin Step 10's source partition: the half that must NOT spend a settle
// cycle, and the half that must still spend one.
var falsificationSettleLoopPins = regProsePins([]falsificationProsePin{
	{
		name:         "settle-loop-bot-consumes-no-cycle",
		pattern:      "It\\s+consumes\\s+no\\s+`settleCap`\\s+cycle,\\s+and\\s+never\\s+re-enters\\s+Step\\s+8\\.",
		live:         "It consumes no `settleCap` cycle, and never re-enters Step 8.",
		tokenRemoved: "It consumes no cycle, and never re-enters Step 8.",
		alsoRemoved: []string{
			// The Step 8 prohibition dropped — a bot review could still open a repair pass.
			"It consumes no `settleCap` cycle.",
			// The budget carve-out inverted.
			"It consumes a `settleCap` cycle, and never re-enters Step 8.",
		},
	},
	{
		name:         "settle-loop-human-and-red-ci-unchanged",
		pattern:      "These\\s+are\\s+unchanged:\\s+they\\s+still\\s+go\\s+back\\s+to\\s+Step\\s+8\\s+\\(boss-repair\\),\\s+still\\s+re-verify\\s+finalize,\\s+and\\s+still\\s+consume\\s+a\\s+settle\\s+cycle\\.",
		live:         "These are unchanged: they still go back to Step 8 (boss-repair), still re-verify finalize, and still consume a settle cycle.",
		tokenRemoved: "These are unchanged: they still go back to Step 8 (boss-repair), still re-verify finalize, and consume no settle cycle.",
		alsoRemoved: []string{
			// The Step 8 route dropped from the human/red-CI half.
			"These are unchanged: they still re-verify finalize, and still consume a settle cycle.",
			// The re-verify dropped.
			"These are unchanged: they still go back to Step 8 (boss-repair), and still consume a settle cycle.",
		},
	},
})

// falsificationCiSignalPin is declared separately because two files assert it: Step 10's settle
// loop and boss-repair's Strategy C. Referencing it by name beats slicing another file's list by
// position, where inserting a pin silently changes what the second call site asserts.
var falsificationCiSignalPin = regProsePin(falsificationProsePin{
	name:         "settle-loop-ci-signal-is-gh-pr-checks",
	pattern:      "Read\\s+CI\\s+from\\s+`gh\\s+pr\\s+checks`\\s+—\\s+a\\s+PR\\s+that\\s+flips\\s+to\\s+`UNSTABLE`\\s+after\\s+being\\s+readied\\s+is\\s+not\\s+red\\s+CI\\.",
	live:         "Read CI from `gh pr checks` — a PR that flips to `UNSTABLE` after being readied is not red CI.",
	tokenRemoved: "Read CI from the merge state — a PR that flips to `UNSTABLE` after being readied is not red CI.",
	alsoRemoved: []string{
		// The UNSTABLE carve-out inverted — readying a PR would look like red CI.
		"Read CI from `gh pr checks` — a PR that flips to `UNSTABLE` after being readied is red CI.",
		// The UNSTABLE carve-out dropped entirely.
		"Read CI from `gh pr checks`.",
	},
})

// falsificationAdvisoryFixReverifyPin is the sentence that keeps an advisory fix a real fix. It is
// declared separately because two files state it verbatim — the contract in receiving-code-review.md
// and Step 10 in finalize-and-stop.md — and a normative sentence pinned in only one of the files
// that state it can be rewritten away in the other without a test going red.
var falsificationAdvisoryFixReverifyPin = regProsePin(falsificationProsePin{
	name:         "advisory-fix-still-reverifies-finalize",
	pattern:      "A\\s+fix\\s+taken\\s+on\\s+the\\s+advisory\\s+path\\s+gets\\s+the\\s+same\\s+finalize\\s+re-verification\\s+a\\s+Step\\s+8\\s+repair\\s+would:\\s+run\\s+the\\s+gates,\\s+commit,\\s+push,\\s+and\\s+re-verify\\s+finalize\\s+on\\s+the\\s+new\\s+head\\s+before\\s+the\\s+grouped\\s+response\\s+is\\s+posted\\.",
	live:         "A fix taken on the advisory path gets the same finalize re-verification a Step 8 repair would: run the gates, commit, push, and re-verify finalize on the new head before the grouped response is posted.",
	tokenRemoved: "A fix taken on the advisory path gets the same finalize re-verification a Step 8 repair would: run the gates and re-verify finalize on the new head before the grouped response is posted.",
	alsoRemoved: []string{
		// The push dropped — the fix would never reach the PR being merged.
		"A fix taken on the advisory path gets the same finalize re-verification a Step 8 repair would: run the gates, commit, and re-verify finalize on the new head before the grouped response is posted.",
		// The re-verify dropped — a fix would land past the finalize check Step 9 established.
		"A fix taken on the advisory path gets the same finalize re-verification a Step 8 repair would: run the gates, commit, push, and post the grouped response.",
		// The ordering relaxed — the response could be posted before the fix was verified.
		"A fix taken on the advisory path gets the same finalize re-verification a Step 8 repair would: run the gates, commit, push, and re-verify finalize on the new head after the grouped response is posted.",
	},
})

// falsificationAdvisoryBoundPin is Step 10's own bound on the path that spends no `settleCap` cycle.
// The parenthetical is load-bearing: without it "round" is undefined, and a run facing two bot
// reviews on one head can read the per-head bound as licence to leave the second one unanswered —
// which the acknowledge-once pin above forbids.
var falsificationAdvisoryBoundPin = regProsePin(falsificationProsePin{
	name:         "advisory-path-carries-its-own-bound",
	pattern:      "The\\s+advisory\\s+path\\s+carries\\s+its\\s+own\\s+bound,\\s+so\\s+removing\\s+it\\s+from\\s+`settleCap`\\s+cannot\\s+make\\s+it\\s+unbounded:\\s+at\\s+most\\s+one\\s+grouped\\s+response\\s+round\\s+per\\s+PR\\s+head\\s+SHA\\s+\\(a\\s+round\\s+answers\\s+every\\s+bot\\s+review\\s+pending\\s+on\\s+that\\s+head,\\s+one\\s+grouped\\s+comment\\s+each\\),\\s+and\\s+at\\s+most\\s+three\\s+advisory\\s+rounds\\s+per\\s+run\\.",
	live:         "The advisory path carries its own bound, so removing it from `settleCap` cannot make it unbounded: at most one grouped response round per PR head SHA (a round answers every bot review pending on that head, one grouped comment each), and at most three advisory rounds per run.",
	tokenRemoved: "The advisory path carries its own bound, so removing it from `settleCap` cannot make it unbounded: at most one grouped response round per PR head SHA (a round answers every bot review pending on that head, one grouped comment each).",
	alsoRemoved: []string{
		// The per-head bound dropped — a bot could be answered repeatedly on one head.
		"The advisory path carries its own bound, so removing it from `settleCap` cannot make it unbounded: at most three advisory rounds per run.",
		// The bound inverted into an unbounded reading.
		"The advisory path spends no `settleCap` cycle, so it is not bounded: any number of grouped response rounds per PR head SHA, and any number of advisory rounds per run.",
		// The definition of "round" dropped — the per-head bound collides with the acknowledge-once
		// contract the moment two bots review the same head.
		"The advisory path carries its own bound, so removing it from `settleCap` cannot make it unbounded: at most one grouped response round per PR head SHA, and at most three advisory rounds per run.",
	},
})

// falsificationReviewVerdictPins pin the Step 6 run note that every other gate here reads.
var falsificationReviewVerdictPins = regProsePins([]falsificationProsePin{
	{
		name:         "review-verdict-absent-record-reads-as-none",
		pattern:      "Readers\\s+take\\s+an\\s+absent\\s+or\\s+unreadable\\s+`boss-build-review-verdict`\\s+file\\s+as\\s+`none`,\\s+so\\s+a\\s+review\\s+that\\s+never\\s+settled\\s+can\\s+never\\s+be\\s+read\\s+downstream\\s+as\\s+clean\\.",
		live:         "Readers take an absent or unreadable `boss-build-review-verdict` file as `none`, so a review that never settled can never be read downstream as clean.",
		tokenRemoved: "Readers take an absent or unreadable `boss-build-review-verdict` file as `capped`, so a review that never settled can never be read downstream as clean.",
		alsoRemoved: []string{
			// The absent case dropped — a missing note would be unclassified.
			"Readers take an unreadable `boss-build-review-verdict` file as `none`, so a review that never settled can never be read downstream as clean.",
			// The safety rationale dropped.
			"Readers take an absent or unreadable `boss-build-review-verdict` file as `none`.",
		},
	},
})

// TestBossBuildStep6RecordsReviewVerdictRunNote pins the durable run note that makes the advisory
// bot path decidable at all. Step 6 classifies the whole-branch verdict in a shell block that ends
// with the sentinel cleanup; without a record written there, Step 10 and boss-repair run in fresh
// processes with no way to tell a clean review from a dispatch failure, and the safe reading of
// "no record" has to be `none` rather than a guess.
func TestBossBuildStep6RecordsReviewVerdictRunNote(t *testing.T) {
	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			skill := readPayloadFile(t, payload, "skills/boss-build/SKILL.md")
			step6 := sectionBetween(t, skill, "## Step 6: Whole-branch review (dispatch the review pass)", "## Step 6.5:")

			// Command shapes keep their literal spaces: a rewrap cannot reach inside a fenced block.
			assertContains(t, step6, `case "$VERDICT" in clean|capped) REVIEW_VERDICT="$VERDICT" ;; *) REVIEW_VERDICT="none" ;; esac`)
			assertContains(t, step6, `printf 'REVIEW_VERDICT=%s\n' "$REVIEW_VERDICT" \`)
			assertContains(t, step6, `>"$(git rev-parse --git-dir)/boss-build-review-verdict"`)

			assertFalsificationPins(t, step6, falsificationReviewVerdictPins)

			// Preflight must clear a previous run's note, or the "run-scoped" claim above is false
			// the moment a worktree is reused. Assert it inside the Preflight window: asserted
			// against the whole skill, the rm could be moved below Step 6's write — where it would
			// delete this run's verdict instead of the previous run's — and stay green.
			preflight := sectionBetween(t, skill, "## Preflight", "## Step 1:")
			assertContains(t, preflight, `rm -f "$(git rev-parse --git-dir)/boss-build-review-verdict"`)
		})
	}
}

// TestBossBuildBotReviewsAreAdvisoryAfterCleanVerdict pins the rule in both boss-build files that
// state it: the reference that defines the contract, and Step 10 where it is spent.
func TestBossBuildBotReviewsAreAdvisoryAfterCleanVerdict(t *testing.T) {
	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			t.Run("receiving-code-review", func(t *testing.T) {
				receiving := readPayloadFile(t, payload, "skills/boss-build/references/receiving-code-review.md")
				botSection := sectionBetween(t, receiving, "### From Bot Reviewers, After a Clean Review", "## YAGNI Check")
				assertFalsificationPins(t, botSection, falsificationBotReviewPins)
				// The acknowledge-once contract is only actionable if it points at the thread-reply
				// mechanism; a grouped comment posted top-level is not "within the bot's threads".
				assertContains(t, botSection, "## GitHub Thread Replies")
				// The same normative sentence Step 10 pins: stating it in only one of the two files
				// that carry it lets the other rot silently.
				assertFalsificationPins(t, botSection, []falsificationProsePin{falsificationAdvisoryFixReverifyPin})
				// The verdict gate is only actionable if the contract doc names where the verdict lives.
				assertContains(t, botSection, "$(git rev-parse --git-dir)/boss-build-review-verdict")
			})

			t.Run("finalize-step-10", func(t *testing.T) {
				finalize := readPayloadFile(t, payload, "skills/boss-build/references/finalize-and-stop.md")
				step10 := sectionBetween(t, finalize, "## Step 10: Settle loop (capped)", "## Step 11:")
				assertFalsificationPins(t, step10, falsificationBotReviewPins)
				assertFalsificationPins(t, step10, falsificationSettleLoopPins)
				// The bound and the escape hatch the partition must leave untouched.
				assertContains(t, step10, "`policy.settleCap` (**3**) settle cycles")
				assertContains(t, step10, "re-quarantine")
				assertContains(t, step10, "stop with BLOCKED")
				// The two properties that keep the advisory path safe once it is removed from the
				// `settleCap` accounting: an advisory fix still goes through the same
				// gate/commit/push/re-verify path, and the path that spends no settle cycle still
				// has a bound.
				assertFalsificationPins(t, step10, []falsificationProsePin{falsificationAdvisoryFixReverifyPin, falsificationAdvisoryBoundPin})
				assertFalsificationPins(t, step10, []falsificationProsePin{falsificationCiSignalPin})
				assertContains(t, step10, "$(git rev-parse --git-dir)/boss-build-review-verdict")
			})
		})
	}
}

// TestBotDetectionNamesNoSpecificBot is the standing acceptance gate: the detection rule must be
// expressed as author signals, never as a roster of product names. A named bot in shipped prose is
// an allowlist that goes stale the moment the next automated reviewer is installed, and it
// silently reclassifies that reviewer as human.
func TestBotDetectionNamesNoSpecificBot(t *testing.T) {
	forbidden := []string{"pr_agent", "pr-agent", "codex-bot", "coderabbit", "dependabot[bot]"}

	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			for _, root := range []string{"skills/boss-build", "skills/boss-repair"} {
				err := fs.WalkDir(payload, root, func(path string, entry fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if entry.IsDir() {
						return nil
					}
					body := strings.ToLower(readPayloadFile(t, payload, path))
					for _, name := range forbidden {
						if strings.Contains(body, name) {
							t.Errorf("%s names a specific bot (%q); describe the detection signals instead", path, name)
						}
					}
					return nil
				})
				if err != nil {
					t.Fatalf("walk %s: %v", root, err)
				}
			}
		})
	}
}

// TestBossBuildStep6DispatchesOneReviewPass pins the collapsed review stack:
// Step 6 dispatches a single awaited boss-review pass over the whole branch and
// routes on the run-file sentinel it writes. A regression that re-introduces a
// second review system — a whole-branch loop of its own, a separate cross-model
// chain, or a reviewer prompt template — fails here.
func TestBossBuildStep6DispatchesOneReviewPass(t *testing.T) {
	for label, payload := range shippedPayloads(t) {
		t.Run(label, func(t *testing.T) {
			skill := readPayloadFile(t, payload, "skills/boss-build/SKILL.md")
			step6 := sectionBetween(t, skill, "## Step 6: Whole-branch review (dispatch the review pass)", "## Step 6.5:")

			// Exactly one review pass, awaited, and it is boss-review.
			assertContains(t, step6, "Dispatch the review pass — exactly one.")
			assertContains(t, step6, "one `boss-review` pass")
			assertContains(t, step6, "Do\n**not** dispatch a second review of any kind")
			assertContains(t, step6, "**await**, **never** `run_in_background`")

			// The pass's verdict is the run-file sentinel, and it is blocking.
			assertContains(t, step6, "bs-run-sentinel.mjs")
			assertContains(t, step6, "`bs-review clean:`")
			assertContains(t, step6, "`bs-review capped:`")
			assertContains(t, step6, "so nothing downstream may demote\nit to advisory")

			// The retired second and third review systems must stay retired. Scoped to the WHOLE
			// skill, not the Step 6 window: four of the seven pre-change `Step 6b`/`Step 6c`
			// mentions lived in Step 7's PR-body prose, outside this window entirely. The needles
			// are case-sensitive, so the surviving `STEP_6C_DEADLINE` interface name does not
			// collide.
			assertNotContains(t, skill, "Step 6b")
			assertNotContains(t, skill, "Step 6c")

			// SKILL.md may not point at the retired template. The boss-build references/ tree is
			// covered separately by TestPublishedCoresShipTheReferencesTheyName, which walks the
			// whole payload for unresolvable `references/<file>` links.
			assertNotContains(t, skill, "code-reviewer-template")
		})
	}
}

// TestBossBuildCappedReviewShipsReviewReady pins the routing inversion this core had to unlearn: a
// round-capped review used to finalize BLOCKED, so pushed, green work with an open finding shipped
// nothing at all. Coverage is not a defect — the capped, provisional-seed and dispatch-failure arms
// all take the REVIEW_READY-with-findings route, and reach BLOCKED only through a failed push or red
// gates. The distinctive route name is asserted, not a paraphrase, because prose that merely stops
// saying "BLOCKED" is not the same as prose that names where the run goes instead.
func TestBossBuildCappedReviewShipsReviewReady(t *testing.T) {
	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			skill := readPayloadFile(t, payload, "skills/boss-build/SKILL.md")
			step6 := sectionBetween(t, skill, "## Step 6: Whole-branch review (dispatch the review pass)", "## Step 6.5:")

			// The route exists and is named the same way everywhere a reader is sent to it.
			assertContains(t, step6, "REVIEW_READY-with-findings")
			// Each of the three non-clean arms takes it. Substrings are kept short and
			// rewrap-proof: the arm bullets are prettier-managed prose. The routing
			// sentence is COUNTED, not merely contained: it appears once per non-clean
			// arm, so a `Contains` check would stay green while two of the three arms
			// were routed back to BLOCKED. The leading `I` is dropped from the needle
			// because the provisional arm spells it mid-sentence as `it becomes`.
			assertContains(t, step6, "published, not fatal")
			const blockedOnly = "ecomes `BLOCKED` **only** when the push or the quality gates fail."
			if got := strings.Count(unwrapped(step6), blockedOnly); got != 3 {
				t.Errorf("%s: want the BLOCKED-only routing sentence on all 3 non-clean arms, got %d", label, got)
			}
			// The published artifacts the route owes, so a run cannot claim the state while
			// swallowing the findings.
			assertContains(t, unwrapped(step6), "`please-review` applied, PR readied")

			// The terminal-state list and Hard rules must agree with the arms.
			assertContains(t, unwrapped(skill), "Open review findings are **not** one of them.")

			// The publication route itself lives in review-stack.md and must precede the BLOCKED
			// one, so a top-down reader meets the shipping route first.
			reviewStack := readPayloadFile(t, payload, "skills/boss-build/references/review-stack.md")
			readyIdx := strings.Index(reviewStack, "### REVIEW_READY-with-findings publication")
			blockedIdx := strings.Index(reviewStack, "### BLOCKED-route publication")
			if readyIdx < 0 {
				t.Fatalf("%s: review-stack.md has no REVIEW_READY-with-findings publication section", label)
			}
			if blockedIdx < 0 {
				t.Fatalf("%s: review-stack.md has no BLOCKED-route publication section", label)
			}
			if readyIdx > blockedIdx {
				// Fatal, not Errorf: the slice below would panic on the reversed bounds.
				t.Fatalf("%s: REVIEW_READY-with-findings publication must precede BLOCKED-route publication", label)
			}

			// The route's own obligations: marker comment, tracker comment, generic label, ready PR.
			readySection := reviewStack[readyIdx:blockedIdx]
			assertContains(t, readySection, "<!-- bs-review -->")
			assertContains(t, readySection, "please-review")
			assertContains(t, readySection, "gh pr ready")
			assertContains(t, readySection, "writeComment")
			// The merge-gate token stays reserved for the PARTIAL hold. The reserved token
			// is the `do not merge` substring in a PR title/body, not a label, so the pin is
			// the PARTIAL marker literal: this route must state the ban and must never
			// compose the marker that holds a PR back.
			assertContains(t, unwrapped(readySection), "may contain the reserved `do not merge` substring")
			assertNotContains(t, unwrapped(readySection), "do not merge — partial:")
		})
	}
}

// TestBossBuildBlockedHasExactlyFourCauses pins the shrunk BLOCKED list in the Step 12 terminal
// pick. Each cause is asserted separately so a rewrite cannot drop one and stay green, and the
// exhaustiveness sentence is asserted too: without it the list reads as examples, which is how the
// old open-ended BLOCKED grew in the first place.
func TestBossBuildBlockedHasExactlyFourCauses(t *testing.T) {
	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			finalize := readPayloadFile(t, payload, "skills/boss-build/references/finalize-and-stop.md")
			// Step 12 is the last section in the file, so it has no following heading to
			// bound it; window it from its own heading to EOF rather than widening the
			// assertions to the whole reference.
			step12Idx := strings.Index(finalize, "## Step 12: Stop cleanly")
			if step12Idx < 0 {
				t.Fatalf("%s: finalize-and-stop.md has no Step 12 heading", label)
			}
			step12 := finalize[step12Idx:]

			for _, cause := range []string{
				"**quality gates are red**",
				"**the branch cannot be pushed**",
				"**a required API-version bump or down-convert transform is missing**",
				"**the plan demands something unsafe**",
			} {
				assertContains(t, unwrapped(step12), cause)
			}

			// The API cause must stay generic: this core ships into every repository on the
			// machine, so it may name a lens role, never a project's wire surface.
			assertContains(t, unwrapped(step12), "API-compatibility lens role")

			// The list is closed, and the thing it most often used to be stretched to cover is
			// named as excluded.
			assertContains(t, unwrapped(step12), "That list is exhaustive.")
			assertContains(t, unwrapped(step12), "**open review findings are not on it**")

			// PARTIAL keeps exactly one meaning, and it is a narrowing of REVIEW_READY rather than
			// a softer BLOCKED.
			step9 := sectionBetween(t, finalize, "## Step 9: Finalize (idempotent tag guard, ready), Linear writeback", "## Step 10:")
			gateIdx := strings.Index(step9, "**The `PARTIAL` gate — one meaning, and only one.**")
			if gateIdx < 0 {
				t.Fatalf("%s: Step 9 has no single-meaning PARTIAL gate", label)
			}
			partialGate := step9[gateIdx:]
			assertContains(t, unwrapped(partialGate), "**only** in-scope acceptance criteria this run did not meet")
			assertContains(t, unwrapped(partialGate), "and it is usually not `BLOCKED`")
		})
	}
}

// unwrapped collapses every run of whitespace to a single space so a prose pin survives a prettier
// rewrap. The markdown in these payloads is reflowed on every format pass, so a pin carrying a
// hard newline is a pin that unpins itself the next time a sentence moves by one word.
func unwrapped(markdown string) string {
	return strings.Join(strings.Fields(markdown), " ")
}

// TestBossBuildReviewTierIsPickedFromTheDiff pins the diff-derived review depth: quick
// vs full is decided at Step 6 entry from the branch diff alone — the configured lens
// globs plus the changed-file count against reviewDefaults.deltaFileThreshold — with
// forceFull winning outright and an unreadable diff selecting full. The tier it names
// is "quick", never "degraded". A regression that reintroduces a wall-clock term into
// the selection, or reinstates any of the retired breaker/budget names anywhere in the
// boss-build payload, fails here.
func TestBossBuildReviewTierIsPickedFromTheDiff(t *testing.T) {
	// Deleted with the wall-clock tier ladder. Any of these reappearing anywhere in the
	// payload means a clock term is back in the review path, wherever it was reintroduced.
	retired := []string{
		"PREFLIGHT_DEADLINE",
		"REMAINING_MINUTES",
		"FULL_TIER_MINUTES",
		"POST_REVIEW_RESERVE",
		// Prose, not a variable: the Preflight breaker these clauses escape to no longer exists.
		"wall-clock breaker",
		"the breaker trips",
	}

	for label, payload := range shippedPayloads(t) {
		t.Run(label, func(t *testing.T) {
			stack := readPayloadFile(t, payload, "skills/boss-build/references/review-stack.md")
			rule := sectionBetween(t, stack, "## Step 6 entry — review tier selection", "### Quick tier (minimal)")

			// The three inputs, every one of them repo-local, named by the helper that reads it.
			// Without these the rule is prose and the two paths can drift apart.
			assertContains(t, rule, "reviewDeltaDefaults(config).forceFull")
			assertContains(t, rule, "lensesForFile(config, path)")
			assertContains(t, rule, "reviewDeltaDefaults(config).deltaFileThreshold")

			assertContains(t, rule, "decided **from the diff**")

			// The four branches, each pinned THROUGH its outcome and in ORDER. Both properties
			// were previously only claimed. Pinning a branch's opening words alone leaves the
			// outcome free: the unreadable-diff branch could be flipped to select the quick tier
			// with every assertion still green, though acceptance criterion 1 requires exactly
			// "unreadable diff selects full". And four independent Contains calls pass under any
			// permutation, so the evaluation order the prose calls load bearing — the override
			// resolved before the diff is read, an unreadable diff before either matcher — was
			// not pinned at all. Indexes fix both. Matched against whitespace-collapsed bytes so
			// a prettier rewrap of the payload cannot red them.
			flatRule := whitespaceRun.ReplaceAllString(rule, " ")

			// Document order is not evaluation order. The index check below proves only that the
			// four branches appear in this sequence; the sentence that makes the sequence load
			// bearing is separate prose, and deleting it shifts all four indexes equally — still
			// ascending, still green, first-match semantics gone.
			assertContains(t, flatRule, "Evaluate the branches below **in order** and take the **first** one that matches. The order is load bearing: the override must be resolved before the diff is read, and an unreadable diff before either matcher")

			branches := []string{
				"1. `reviewDefaults.forceFull` is **true** → **full tier**.",
				"2. The diff is **unreadable** — the `git diff` failed, the helper exited non-zero, or `REVIEW_TIER_JSON` is empty or does not parse → **full tier**.",
				"3. **No** changed path matches **any** configured lens glob **and** the changed-file count is **strictly below** `deltaFileThreshold` → **quick tier (minimal)**,",
				"4. Otherwise → **full tier**.",
			}
			previous := -1
			for _, branch := range branches {
				at := strings.Index(flatRule, branch)
				if at < 0 {
					t.Fatalf("tier rule is missing the branch %q", branch)
				}
				if at <= previous {
					t.Fatalf("tier rule branch %q is out of order", branch)
				}
				previous = at
			}

			// The executable form of the rule, not just its prose restatement. Without these the
			// ternary the run actually evaluates can be inverted or deleted with every other
			// assertion in this test still green.
			assertContains(t, rule, "const lensHit = files.some((f) => m.lensesForFile(cfg, f).length > 0)")
			assertContains(t, rule, "const tier = forceFull || lensHit || files.length >= deltaFileThreshold ? \"full\" : \"quick\"")

			// The clock must not return as an extra branch or a tiebreaker.
			assertContains(t, rule, "Do not re-introduce a wall-clock term into this rule")

			// The tier is named "quick" throughout this reference, never "degraded".
			assertContains(t, stack, "### Quick tier (minimal)")
			assertNotContains(t, stack, "degraded")

			// The bounds that survive the ladder: the per-dispatch timeout every allowance is
			// derived from, the caller-deadline interface name, and boss-review's round cap.
			assertContains(t, stack, "BOSS_SKILL_EXTENSION_TIMEOUT_MS")
			assertContains(t, stack, "STEP_6C_DEADLINE")
			assertContains(t, stack, "$MAX_ROUNDS")

			// The coverage-token vocabulary moved with the rename — in the reference that
			// defines it and in the PR-body enumeration Step 7 writes from.
			assertContains(t, stack, "`quick: <reason> (skipped: <round list>)`")
			skill := readPayloadFile(t, payload, "skills/boss-build/SKILL.md")
			assertContains(t, skill, "quick: <reason> (skipped: <round list>)")
			assertNotContains(t, skill, "degraded: <reason>")

			err := fs.WalkDir(payload, "skills/boss-build", func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return nil
				}
				data, readErr := fs.ReadFile(payload, path)
				if readErr != nil {
					return readErr
				}
				body := string(data)
				for _, name := range retired {
					if strings.Contains(body, name) {
						t.Errorf("%s still names %q; the review tier is picked from the diff, not from a clock", path, name)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk boss-build payload: %v", err)
			}
		})
	}
}

// falsificationCiWaitProhibitionPins open BOS-1106's contract: every wait on this PR's CI is driven by state,
// never by a clock. Four properties are load-bearing and each is deletable on its own without any
// other gate here going red — the prohibition and its carve-out (a blanket ban would outlaw the
// bounded poll's own pacing, so the exemption must be pinned beside the ban), Step 9's waits being
// callback-driven under the gate rather than optionally so, and Step 10's opener waiting on the
// bounded poll while deliberately NOT re-arming. That last one reads like an omission and is not:
// Step 9 already gated the branch green, so `checks_passed` reads satisfied, and a watch armed on a
// satisfied trigger fires immediately (callback-watches.md Protocol step 4) — arming there burns the
// watch and cancels its group siblings instead of waiting for anything.
var falsificationCiWaitProhibitionPins = regProsePins([]falsificationProsePin{
	{
		name:         "ci-wait-forbids-fixed-sleep",
		pattern:      "A\\s+fixed\\s+`sleep`\\s+of\\s+\\*\\*60\\s+seconds\\s+or\\s+longer\\*\\*\\s+spent\\s+waiting\\s+for\\s+CI\\s+is\\s+a\\s+defect",
		live:         "A fixed `sleep` of **60 seconds or longer** spent waiting for CI is a defect",
		tokenRemoved: "A fixed `sleep` of **60 seconds or longer** spent waiting for CI is acceptable",
		alsoRemoved: []string{
			// The threshold dropped — the rule stops naming what it bans.
			"A fixed `sleep` spent waiting for CI is a defect",
			// The scope dropped — the ban would reach every sleep in the skill, including the
			// bounded poll's own interval.
			"A fixed `sleep` of **60 seconds or longer** is a defect",
		},
	},
	{
		name:         "ci-wait-exempts-bounded-backoff",
		pattern:      "those\\s+pace\\s+a\\s+bounded\\s+read\\s+that\\s+is\\s+already\\s+running,\\s+they\\s+do\\s+not\\s+stand\\s+in\\s+for\\s+a\\s+wait\\.",
		live:         "those pace a bounded read that is already running, they do not stand in for a wait.",
		tokenRemoved: "those pace a bounded read that is already running, they stand in for a wait.",
		alsoRemoved: []string{
			// The bounded-read anchor dropped — the exemption stops saying which sleeps qualify,
			// and becomes an escape hatch for the fixed wait the rule above bans.
			"those are already running, they do not stand in for a wait.",
		},
	},
})

// falsificationStep9CallbackWaitPins pin Step 9's two re-push waits: driven by the watches under the
// gate, and backed by the bounded poll on both sides of it.
var falsificationStep9CallbackWaitPins = regProsePins([]falsificationProsePin{
	{
		name:         "step9-waits-are-callback-driven-under-the-gate",
		pattern:      "Both\\s+waits\\s+above\\s+are\\s+\\*\\*driven\\s+by\\s+the\\s+one-shot\\s+callback\\s+watches\\s+whenever\\s+`callbacksAvailable\\(env\\)`\\s+is\\s+true\\*\\*",
		live:         "Both waits above are **driven by the one-shot callback watches whenever `callbacksAvailable(env)` is true**",
		tokenRemoved: "Both waits above are **optionally driven by the one-shot callback watches**",
		alsoRemoved: []string{
			// The gate dropped — arming would be unconditional, which fails in a standalone run.
			"Both waits above are **driven by the one-shot callback watches**",
			// One of the two waits dropped back out of the contract.
			"The second wait above is **driven by the one-shot callback watches whenever `callbacksAvailable(env)` is true**",
		},
	},
	{
		name:         "step9-fallback-poll-backs-both-branches",
		pattern:      "Protocol\\s+step\\s+5\\s+bounded\\s+poll\\s+backs\\s+the\\s+wait\\s+on\\s+\\*\\*both\\*\\*\\s+branches:\\s+safety\\s+net\\s+under\\s+a\\s+true\\s+gate,\\s+sole\\s+wait\\s+mechanism\\s+under\\s+a\\s+false\\s+one\\.",
		live:         "Protocol step 5 bounded poll backs the wait on **both** branches: safety net under a true gate, sole wait mechanism under a false one.",
		tokenRemoved: "Protocol step 5 bounded poll backs the wait on the gate-false branch: sole wait mechanism under a false one.",
		alsoRemoved: []string{
			// The two roles dropped — a reader takes "backs both branches" as decorative and skips
			// the poll under a true gate, so a missed or expired delivery hangs the wait.
			"Protocol step 5 bounded poll backs the wait on **both** branches.",
		},
	},
})

// falsificationSettleOpenerWaitPins pin Step 10's opener: it waits on the bounded poll, and it
// deliberately does not arm.
var falsificationSettleOpenerWaitPins = regProsePins([]falsificationProsePin{
	{
		name:         "settle-opener-waits-on-state-not-clock",
		pattern:      "so\\s+this\\s+step\\s+\\*\\*waits\\s+on\\s+state,\\s+never\\s+on\\s+a\\s+clock\\*\\*",
		live:         "so this step **waits on state, never on a clock**",
		tokenRemoved: "so this step **waits on a clock**",
		alsoRemoved: []string{
			// The prohibition half dropped — "waits on state" alone readmits the fixed five-minute
			// wait this ticket removed, since a clock-wait can be described as waiting on state.
			"so this step **waits on state**",
		},
	},
	{
		name:         "settle-opener-does-not-rearm-a-satisfied-trigger",
		pattern:      "`checks_passed`\\s+already\\s+reads\\s+satisfied\\s+and\\s+re-arming\\s+a\\s+satisfied\\s+trigger\\s+fires\\s+it\\s+immediately\\s+and\\s+burns\\s+the\\s+watch",
		live:         "`checks_passed` already reads satisfied and re-arming a satisfied trigger fires it immediately and burns the watch",
		tokenRemoved: "`checks_passed` already reads satisfied and re-arming a satisfied trigger is harmless",
		alsoRemoved: []string{
			// The consequence dropped — an immediate fire reads as a cheap no-op rather than a
			// spent watch, so the reason not to arm here evaporates.
			"`checks_passed` already reads satisfied and re-arming a satisfied trigger fires it immediately",
			// The precondition dropped — the rule stops saying WHICH trigger is already satisfied,
			// and generalises into "never re-arm", contradicting Protocol step 4's re-arm rule.
			"re-arming a satisfied trigger fires it immediately and burns the watch",
		},
	},
})

// falsificationBlockedRouteGreenReadingPin pins the green reading on review-stack's BLOCKED
// publication route. Its sibling on the PARTIAL route was converted to the bounded poll upstream
// while this one was left on the bare unbounded watch, which is exactly the failure a single pin per
// pair cannot catch: two sentences saying the same thing, only one of them gated.
var falsificationBlockedRouteGreenReadingPin = regProsePin(falsificationProsePin{
	name:         "blocked-route-green-reading-is-the-bounded-poll",
	pattern:      "Only\\s+`CI_WAIT_STATE=settled`\\s+is\\s+green\\.\\s+Red,\\s+`timeout`,\\s+`unknown`,\\s+or\\s+a\\s+rollup\\s+you\\s+cannot\\s+resolve,\\s+is\\s+cause\\s+\\(1\\)",
	live:         "Only `CI_WAIT_STATE=settled` is green. Red, `timeout`, `unknown`, or a rollup you cannot resolve, is cause (1)",
	tokenRemoved: "Only `CI_WAIT_STATE=settled` is green. Red, or a rollup you cannot resolve, is cause (1)",
	alsoRemoved: []string{
		// The fail-closed half dropped — an exhausted or unresolvable wait would fall through as
		// something other than a blocker.
		"Only `CI_WAIT_STATE=settled` is green.",
		// The single-green-state half dropped — every non-red state, timeout included, reads green.
		"Red, `timeout`, `unknown`, or a rollup you cannot resolve, is cause (1)",
	},
})

// TestBossBuildCiWaitsAreCallbackFirst is BOS-1106's acceptance gate. Every wait boss-build takes on
// this PR's checks is pinned at the site that takes it, because the sites do not share prose: Step
// 8's green gate, Step 9's two re-push waits, Step 10's settle opener, and review-stack's two green
// readings each state the contract in their own words, and a gate that reads only one of them lets
// the others drift back to a clock one at a time.
func TestBossBuildCiWaitsAreCallbackFirst(t *testing.T) {
	for label, payload := range shippedPayloads(t) {
		label, payload := label, payload
		t.Run(label, func(t *testing.T) {
			finalize := readPayloadFile(t, payload, "skills/boss-build/references/finalize-and-stop.md")

			t.Run("step-8-green-gate", func(t *testing.T) {
				step8 := sectionBetween(t, finalize, "## Step 8: Tag commits, then repair to green", "## Step 9:")
				assertFalsificationPins(t, step8, falsificationCiWaitProhibitionPins)
				// The prohibition is only actionable if the reader can reach the wait it mandates.
				assertContains(t, step8, "[`callback-watches.md`](callback-watches.md)")
			})

			t.Run("step-9-repush-waits", func(t *testing.T) {
				step9 := sectionBetween(t, finalize, "## Step 9:", "## Step 10:")
				assertFalsificationPins(t, unquoteBlockquote(step9), falsificationStep9CallbackWaitPins)
				assertContains(t, step9, "[`callback-watches.md`](callback-watches.md)")
			})

			t.Run("step-10-settle-opener", func(t *testing.T) {
				step10 := sectionBetween(t, finalize, "## Step 10: Settle loop (capped)", "## Step 11:")
				assertFalsificationPins(t, step10, falsificationSettleOpenerWaitPins)
				assertContains(t, step10, "`policy.settleCap`")
			})

			t.Run("review-stack-blocked-route", func(t *testing.T) {
				stack := readPayloadFile(t, payload, "skills/boss-build/references/review-stack.md")
				assertFalsificationPins(t, stack, []falsificationProsePin{falsificationBlockedRouteGreenReadingPin})
			})

			t.Run("no-unbounded-watch-is-prescribed", func(t *testing.T) {
				// The bare command has no timeout of its own, so a site that names it as the wait
				// has no bound at all. It may still appear as the thing the prose forbids, which is
				// why this asserts on the prescribing shape rather than on the command name.
				for _, rel := range []string{
					"skills/boss-build/references/finalize-and-stop.md",
					"skills/boss-build/references/review-stack.md",
				} {
					body := readPayloadFile(t, payload, rel)
					for _, prescribed := range []string{
						"take the reading here, **after** the push and the ready below and against the PR this section publishes:\n  `gh pr checks",
						"then `gh pr checks \"$PR_NUMBER\" --watch --fail-fast`",
					} {
						if strings.Contains(body, prescribed) {
							t.Errorf("%s prescribes the unbounded watch (%q); cite callback-watches.md Protocol step 5's bounded poll instead", rel, prescribed)
						}
					}
				}
			})
		})
	}
}

// unquoteBlockquote strips markdown blockquote markers so a prose pin can match text that lives
// inside a `>` block. wrapProsePin's fixtures wrap on a bare newline, so a pin pattern's `\s+`
// separators cannot cross the `> ` a blockquote prepends to every continuation line — without this
// the pin would report "prose does not match" for prose that is present and correct.
func unquoteBlockquote(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, ">") {
			continue
		}
		lines[i] = strings.TrimPrefix(strings.TrimPrefix(trimmed, ">"), " ")
	}
	return strings.Join(lines, "\n")
}
