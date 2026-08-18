package skillinstall

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// BOS-663: discovered extensions are loaded by skillPath, never dispatched by Skill-tool name.
//
// An extension skill is dispatched explicitly by its core and never model-matched, so it SHOULD
// declare `disable-model-invocation: true` — and the Skill tool refuses exactly such a skill. A
// core that dispatches by descriptor `name` therefore renders its whole extension layer inert,
// silently, because the fallback tiers were suppressed on an extension being PRESENT rather than
// on the dispatch SUCCEEDING. The descriptor already carries `skillPath`; the fix is to read it
// from disk. This file holds the prose gate that keeps every published core honest.
//
// The gate is POLARITY-INVERTED: rather than trying to recognise a discovered extension and then
// asking whether it is loaded wrongly, it treats every passage that instructs a Skill-tool load as
// suspect and demands the passage clear itself — by naming the `skillPath` remedy, by being about a
// `lensMap` config skill (the one thing legitimately loaded by name), or by sitting in the small
// documented allowlist below. That direction is both smaller and stricter: it never has to identify
// the subject, so a regression that names the extension in an adjacent paragraph, a heading, a
// sibling bullet or not at all is caught exactly like one that spells it out.
//
// The gate is a floor, not a census. It reads prose, so a sufficiently oblique rewrite can evade it;
// what it guarantees is that the shapes reviewers have actually constructed against it — and every
// literal wording BOS-663 removed — cannot come back unnoticed. Two limitations are known and left
// standing deliberately:
//
//   - A directive that separates its load verb from its Skill-tool mention across clauses ("Load the
//     extension. Do that through the Skill tool.") is not caught. Requiring both in one clause is
//     what keeps the correct `lensMap` prose in the contract from being flagged; dropping the verb
//     requirement trades this narrow miss for false positives on every passage that merely NAMES the
//     tool, which is the shape that makes a prose gate get deleted rather than fixed.
//   - A published CORE legitimately loaded through the Skill tool is textually indistinguishable
//     from a discovered extension wrongly loaded the same way (compare fixture E15 with the
//     `extensionDispatchAllowlist` entries). No rule separates them, so each known core-loading
//     passage is exempted by name and the exemption is proven live by a test.

// extensionDispatchListItem matches the start of a markdown list item, which is where a core's
// tier bullets begin. Splitting windows at a top-level marker keeps a Tier-1 bullet from being
// merged with the Tier-2/3 bullets that follow it with no intervening blank line.
var extensionDispatchListItem = regexp.MustCompile(`^(?:[-*+]\s|\d+\.\s)`)

// skillToolLoad matches the load mechanism this gate forbids for a discovered extension. The
// separator is deliberately loose: these documents already write the tool backticked ("the `Skill`
// tool") and hyphenated ("a Skill-tool invocation"), and a gate that only knew the bare two-word
// spelling would miss a regression written in the payload's own house style.
var skillToolLoad = regexp.MustCompile("(?i)\\bskill[\\s`*_-]{1,3}tool\\b")

// extensionDispatchLoadVerb is the one thing that turns a Skill-tool MENTION into a Skill-tool
// DIRECTIVE. It is the gate's only remaining anchor: with the polarity inverted there is no subject
// to identify, so the whole classification reduces to "does this passage instruct loading something
// through the Skill tool, and does it fail to say `skillPath`". Breadth is the point — a directive
// written as "dispatch it", "invoke it", "run it" or "use it" is the same regression as "load it".
var extensionDispatchLoadVerb = regexp.MustCompile(`(?i)\b(?:load|loads|loaded|loading|invoke|invokes|invoked|invoking|dispatch|dispatches|dispatched|dispatching|run|runs|call|calls|use|uses|using)\b`)

// extensionDispatchRemedy marks a passage that states the CORRECT mechanism. This is the primary
// exemption, and it is order-independent by construction: a passage that names `skillPath` has said
// the right thing, wherever in the passage it said it.
var extensionDispatchRemedy = regexp.MustCompile(`(?i)skillPath`)

// extensionDispatchResourceBase pins the second half of path-based dispatch. Passing only an
// extension's SKILL.md text loses the base directory for sibling references, so workers must also
// receive the descriptor directory and resolve relative extension resources from it.
var extensionDispatchResourceBase = regexp.MustCompile(`(?i)(?:relative\s+extension\s+resources\s+to\s+resolve|resolve\s+relative\s+extension\s+resources)\s+from ` + "`dir`")

// extensionDiscoverySkippedClause marks a passage that tells the reader to record the BROKEN half
// of a discovery's `skipped` array, keyed on the helper's own `deliberate` classification.
//
// It deliberately requires the literal `deliberate` predicate rather than merely a mention of
// `skipped`: a paraphrase that drops the classification is the exact regression BOS-744 fixes. Two
// wordings are both wrong and both stay green under a mention-only check — "record every `skipped`
// entry", which cries wolf on every markerless same-prefix helper for as long as that skill exists,
// and a hand-rolled exclusion keyed on the literal `reason` text, which silently stops excluding
// anything the moment the helper rewords a sentence it is free to reword.
//
// The window is generous (and matched against whitespace-collapsed bytes) because the `skipped`
// reference and the predicate can sit a couple of sentences apart — boss-review's lens paragraph
// names `LENS_EXTENSIONS_JSON.skipped`, explains what it carries, and only then states the rule.
var extensionDiscoverySkippedClause = regexp.MustCompile("(?is)skipped.{0,600}?`deliberate` is `false`")

// extensionDiscoverySkippedSites is the number of discover sites in each core's spine that must
// tell the reader to record the broken skips. Every call to `skill-extensions.mjs discover` is one
// site, wherever in the spine it lives.
//
// This is a FLOOR, not a census, exactly like extensionDispatchSkillPathSites above: it catches
// deletion, which is the failure mode. A presence-only check stays green after the clause is
// deleted from all but one site, which would let a core ship a discover site whose misconfigured
// extensions vanish with no ledger line — and a vanished extension is indistinguishable from one
// that was never installed, so the core's fallback tier reads as the intended tier.
//
// If you genuinely consolidate two discover sites into one, lower the number here in the same
// commit and say why. Adding a discover site RAISES it: the contract's MUST-record rule applies at
// every site that calls discover, with no exemption.
//
// KNOWN TRADE-OFF, accepted deliberately. This counts clause OCCURRENCES across the spine, not
// per-site coverage, so consolidating two clauses into one shared paragraph reds this gate even
// though coverage is unchanged. The obvious alternative — require a clause within N bytes of each
// `discover --core` invocation — is not available: boss-build's resident SKILL.md is 8 bytes under
// a ratchet whose hard `RATCHET < PRE_EXTRACTION_BASELINE` invariant leaves ~12 bytes, so its three
// clauses live in the references that are each site's full spec, arbitrarily far from the
// invocation. A proximity gate would therefore fail the one core that most needs the rule. An
// occurrence floor over-constrains; a proximity gate under-covers; this picks the former because
// its failure mode is a red build a human reads, and the latter's is a silent gap.
//
// It also shares `coreBodyFiles` with two other floor maps, so WIDENING that spine can loosen this
// floor and theirs at once (BOS-744 widened it and had to restore two dispatch floors in the same
// change). Re-derive every floor that reads `bodyFilesFor` whenever the spine list changes.
// boss-build states the rule in its REFERENCES rather than in SKILL.md, and that is forced, not a
// preference: its resident body is injected whole at turn 0 of every run and sits 8 bytes under the
// ratchet in scripts/boss-build-skill.test.mjs, whose own hard
// `RATCHET < PRE_EXTRACTION_BASELINE` invariant leaves 12 bytes before the pre-extraction size the
// Steps 8-12 extraction banked. Even the barest one-line clause measured 55 bytes over. That gate's
// error message names this exact remedy — put the prose in a reference, leave the body a pointer —
// so each of boss-build's three discover sites carries the rule in the reference that is its own
// full spec: core-spine.md §2 for the Step 5 methodology tier, knowledge-extensions.md for Step
// 6.5, finalize-and-stop.md for the Step 12 notes phase.
//
// That applies to BOTH of boss-build's resident discover sites, not just Step 5, and the omission
// is deliberate at each: SKILL.md:789 (Step 5, methodology) and SKILL.md:1059 (Step 6.5, knowledge)
// each state the discover call resident and route the rule to their reference. Step 6.5's resident
// prose does mention `extension <name>: skipped (<reason>)`, but for `validate --role knowledge`
// FAILURES rather than discovery skips — a different cause with the same ledger line — so a reader
// checking only SKILL.md should not read either site as unfixed.
var extensionDiscoverySkippedSites = map[string]int{
	"boss-build":  3, // core-spine methodology, knowledge-extensions, finalize-and-stop notes
	"boss-epic":   1, // SKILL.md notes
	"boss-plan":   4, // SKILL.md notes, interactive-mode draft, headless-drafting-brief draft, extension-reviewers plan-reviewer
	"boss-repair": 1, // SKILL.md notes
	"boss-review": 3, // SKILL.md lens + round + notes
}

// TestPublishedCoresRecordBrokenDiscoverySkips is the BOS-744 gate. `discoverExtensions` can drop a
// descriptor nine different ways; before this, nine of the twelve discover sites read `.extensions`
// alone, so a misconfigured lens or drafter vanished silently and the core reported its fallback
// tier as though that had always been the intended tier. The remaining three did read `.skipped`,
// but each re-derived the deliberate-vs-broken split from the literal `reason` text.
//
// Twelve is the count the floor map below sums to, and it counts SITES, not textual matches: a
// `discover --core` grep finds fourteen, because boss-plan names its plan-reviewer discover in both
// SKILL.md and references/extension-reviewers.md, and boss-build names its knowledge discover in
// both SKILL.md and references/knowledge-extensions.md. Each of those pairs is one site stated
// twice, and the reference is where its clause lives.
func TestPublishedCoresRecordBrokenDiscoverySkips(t *testing.T) {
	for label, fsys := range shippedPayloads(t) {
		for core, want := range extensionDiscoverySkippedSites {
			got := 0
			scanned := 0
			for _, rel := range bodyFilesFor(core) {
				path := filepath.Join("skills", core, rel)
				data, err := fs.ReadFile(fsys, path)
				if err != nil {
					t.Fatalf("read %s %s: %v", label, path, err)
				}
				scanned++
				flat := whitespaceRun.ReplaceAll(data, []byte(" "))
				got += len(extensionDiscoverySkippedClause.FindAll(flat, -1))
			}
			// A floor over zero scanned files passes vacuously: if bodyFilesFor ever stops
			// resolving this core's spine, every count silently becomes 0 >= 0 for a want of 0
			// and the gate reports success while checking nothing.
			if scanned == 0 {
				t.Errorf("%s: core %q scanned no spine files — the floor below would pass vacuously", label, core)
			}
			if got < want {
				t.Errorf("%s: core %q records broken discovery skips at %d site(s), want at least %d — a discover site lost its `deliberate: false` clause, so a misconfigured extension there now vanishes with no ledger line and its fallback tier reads as the intended tier", label, core, got, want)
			}
		}
	}
}

// skipReasonBlock isolates the `SKIP_REASONS` object literal; skipReasonKey pulls each quoted or
// bare key out of it. Reading the helper beats re-declaring the codes in Go: the helper is the only
// place they are defined, and a Go-side copy is exactly the drift this gate exists to catch.
var (
	skipReasonBlock = regexp.MustCompile(`(?s)export const SKIP_REASONS = \{(.*?)\n\}`)
	skipReasonKey   = regexp.MustCompile(`(?m)^\s*'([a-z-]+)':`)
)

// skipReasonCodes returns every code declared in the canonical helper's SKIP_REASONS table.
// `make vendor-toolbox-check` pins the five vendored copies and the plugin mirror byte-identical to
// this file, so reading the canonical one covers them all.
func skipReasonCodes(t *testing.T) []string {
	t.Helper()
	helperPath := filepath.Join(findRepoRoot(t), "skills-toolbox", "skill-extensions.mjs")
	data, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatalf("read discovery helper: %v", err)
	}
	block := skipReasonBlock.FindSubmatch(data)
	if block == nil {
		t.Fatalf("skills-toolbox/skill-extensions.mjs no longer declares an `export const SKIP_REASONS = {` block — this gate reads the codes from there, and silently matching nothing would make it vacuous")
	}
	var codes []string
	for _, m := range skipReasonKey.FindAllSubmatch(block[1], -1) {
		codes = append(codes, string(m[1]))
	}
	// Non-vacuity: an empty set would satisfy the caller's loop trivially. The helper documents
	// nine codes today; the floor only guards against the regex silently matching nothing.
	if len(codes) < 2 {
		t.Fatalf("parsed %d skip codes from SKIP_REASONS, want at least 2 — the key regex has stopped matching and the documentation check below would pass vacuously", len(codes))
	}
	return codes
}

// TestExtensionContractDocumentsSkipClassification pins the authoring guidance the gate above
// enforces. Without it the contract could drift into silence while the gate stayed green, leaving
// core authors to re-derive the deliberate-vs-broken split from the `reason` text — which is the
// re-derivation BOS-744 moved into the helper precisely so it would stop being re-derived.
func TestExtensionContractDocumentsSkipClassification(t *testing.T) {
	contractPath := filepath.Join(findRepoRoot(t), "docs", "skills", "extension-contract.md")
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read extension contract: %v", err)
	}
	flatContract := whitespaceRun.ReplaceAllString(string(data), " ")

	// The entry shape and both classification fields.
	for _, want := range []string{
		"{ name, reason, code, deliberate }",
		"`code`",
		"`deliberate`",
	} {
		if !strings.Contains(flatContract, want) {
			t.Errorf("docs/skills/extension-contract.md must document %q — core authors build their skip-recording clause against this file", want)
		}
	}

	// Every code the helper can emit must be documented. The set is DERIVED from the helper rather
	// than hand-copied here: a literal list only ever asserts doc ⊇ list, so a code added to
	// SKIP_REASONS lands undocumented with this test still green — which is precisely the
	// "arrives unannounced" case the list was meant to prevent.
	for _, code := range skipReasonCodes(t) {
		if !strings.Contains(flatContract, code) {
			t.Errorf("docs/skills/extension-contract.md must document skip code %q — it is in SKIP_REASONS, so discovery can emit it and a core author has to know what it means", code)
		}
	}

	// The normative rule itself. "MUST record ... at every site that calls discover" is the half
	// that makes the floor gate above legible as a contract requirement rather than a local
	// house rule, and "never fatal" is what stops a core turning a discovery skip into a stop.
	for _, want := range []struct{ phrase, why string }{
		{
			"MUST** record every discovered `skipped` entry whose `deliberate` is `false`",
			"must state the record-the-broken-ones rule normatively, keyed on `deliberate` rather than on the `reason` text a core would otherwise have to pattern-match",
		},
		{
			"at **every** site that calls `discover`",
			"must say the rule applies at every discover site — a rule stated once, for one phase, is how ten of the thirteen sites went unfixed",
		},
		{
			"never fatal and never changes control flow",
			"must state that recording is all that is due, so a core cannot escalate a discovery skip into a stop or a tier change",
		},
	} {
		if !strings.Contains(flatContract, want.phrase) {
			t.Errorf("docs/skills/extension-contract.md must contain %q: it %s", want.phrase, want.why)
		}
	}
}

// extensionDispatchConfigSkill marks a window that is talking about a `.boss-skills.json`
// `lensMap` entry. Those name a foreign, model-invocable global skill with no known on-disk path
// in an arbitrary checkout, so the Skill tool is the correct — and only — way to load one. This is
// the distinction BOS-663 encodes rather than assumes.
var extensionDispatchConfigSkill = regexp.MustCompile(`(?i)(<LENS_SKILL>|lensMap|\.boss-skills\.json)`)

// extensionDispatchLensTemplate is the SENTENCE-level form of the carve-out above. Inside an exempt
// window only the literal `<LENS_SKILL>` placeholder clears a sentence, never a passing mention of
// `lensMap`: every `boss-review` Phase 1 window name-drops `lensMap` already, so honouring the wider
// pattern per sentence would let "each discovered lens extension is loaded by its descriptor `name`
// via the Skill tool, exactly as a `lensMap` entry is" launder itself (fixture E13).
var extensionDispatchLensTemplate = regexp.MustCompile(`<LENS_SKILL>`)

// extensionDispatchProhibition marks a clause that FORBIDS the load rather than instructing it —
// the canonical "Never load a discovered extension by its bare descriptor `name` via the Skill
// tool" wording, and the "the Skill tool refuses such a skill" rationale that follows it.
//
// It is applied per CLAUSE, not per sentence. A sentence-scoped prohibition test clears any
// sentence containing a prohibition word anywhere in it, which laundered the exact regression this
// gate exists for: "If the `skillPath` file can never be read, load the extension by its descriptor
// `name` via the Skill tool instead" is a directive whose only negation governs an unrelated verb
// (fixture E14). Clause scoping asks the narrower question that actually matters — is the clause
// carrying the Skill-tool load itself a prohibition?
//
// `\bnot\b` is safe at clause scope for the same reason: "cannot" is a single word and never
// matches, and a "does not exist" failure condition lives in its own clause, not in the clause that
// then instructs the load.
var extensionDispatchProhibition = regexp.MustCompile(`(?i)\b(?:never|not|forbidden|prohibited|refuses?)\b`)

// extensionDispatchSentence splits a segment into sentences. The break requires whitespace after
// the terminator, so `skill-extensions.mjs` and `SKILL.md` do not break mid-token.
var extensionDispatchSentence = regexp.MustCompile(`[.!?;]\s+`)

// extensionDispatchClause splits a sentence into clauses for the prohibition and remedy tests. The
// em/en dash counts as a clause break because these documents use it as one heavily — "so the Skill
// tool is the correct — and only — way to load one" is a single comma-free sentence whose tool
// mention and load verb sit in different clauses.
var extensionDispatchClause = regexp.MustCompile(`[,;:]\s+|\s+[—–]\s+`)

// whitespaceRun collapses a markdown run of whitespace to one space, so a prose gate can pin a
// multi-word phrase without also pinning the line breaks prettier chose for it today.
var whitespaceRun = regexp.MustCompile(`\s+`)

// extensionDispatchAllowlist exempts the passages in the REAL shipped prose that instruct a
// Skill-tool load, name no `skillPath`, and are nonetheless correct. An entry is a literal
// substring of the window it clears. The list is shrink-only in spirit: it exists to buy the
// polarity inversion, not to absorb future findings, and `TestExtensionDispatchAllowlistIsLive`
// fails if an entry stops matching anything so a stale entry cannot quietly widen the gate.
//
// Measured against both shipped payloads and both prose surfaces, the inversion produces five
// flagged passages in two families, both of them about loading a published CORE rather than a
// discovered extension. A core IS model-invocable and has no descriptor and no `skillPath`, so the
// Skill tool is the right way to reach one — and a sentence saying so is textually indistinguishable
// from fixture E15 ("Load it through the Skill tool"), which is a real regression. That is why these
// are named passages rather than a pattern: no rule can separate the two, so the exemption is spent
// deliberately, once per known passage, instead of carved out as a shape.
var extensionDispatchAllowlist = []string{
	// `boss-review`'s own `BASE` argument-resolution bullet. It explains that when a user enters
	// THIS CORE through the Skill tool there is no positional `$1`, so the base ref falls back to
	// the repo default. It describes how the core is invoked, not how an extension is loaded, and
	// there is no `skillPath` it could name. Present in both payload copies of `boss-review/SKILL.md`
	// and, reworded, in `docs/skills/boss-review.md`.
	"Invoked via the `Skill` tool there is usually no",
	// `boss-build` Step 6c entering the `boss-review` CORE. The paragraph goes on to describe the
	// `lensMap`, which exempts the window, so the sentence is reached by the directive pass. Present
	// in both payload copies of `boss-build/references/review-stack.md`.
	"Invoke it via the `Skill` tool (`boss-review`, no args",
}

// extensionDispatchAllowed reports whether text is one of the documented legitimate passages above.
func extensionDispatchAllowed(text string) bool {
	for _, allowed := range extensionDispatchAllowlist {
		if strings.Contains(text, allowed) {
			return true
		}
	}
	return false
}

// extensionDispatchInstructsLoad reports whether text instructs loading something through the Skill
// tool, as opposed to merely mentioning the tool. Both halves are required: "If the host exposes a
// Skill tool affordance for its own built-in commands, prefer it" names the tool but directs no
// load, and flagging it would make the gate unusable in the cores that legitimately discuss host
// affordances.
func extensionDispatchInstructsLoad(text string) bool {
	return skillToolLoad.MatchString(text) && extensionDispatchLoadVerb.MatchString(text)
}

// extensionDispatchWindow is a bounded passage: a markdown paragraph, or a top-level list item plus
// its continuation lines and nested sub-bullets.
//
// text is the whole window with every line joined by a space, so an instruction wrapped across
// source lines is matched as one string. segments preserves the window's internal list structure —
// each list item (with its own continuation lines joined) is one segment. Segments matter because a
// prohibition in one bullet must not clear a directive in the NEXT bullet: markdown bullets rarely
// carry terminal punctuation, so a sentence splitter alone collapses a whole nested list into one
// "sentence" and every carve-out in it becomes a blanket amnesty (fixture E17).
type extensionDispatchWindow struct {
	text     string
	segments []string
}

// extensionDispatchWindows splits doc into windows.
//
// Only an UNINDENTED list marker starts a new window. A nested sub-bullet stays with its parent,
// because the cores' tier prose routinely names the subject in the parent bullet ("Tier 1: dispatch
// each discovered extension:") and the mechanism in a child ("- Load it via the Skill tool"), and
// splitting there would let that shape — a likely form of a real regression — through.
func extensionDispatchWindows(doc string) []extensionDispatchWindow {
	var windows []extensionDispatchWindow
	var segments []string
	var cur []string
	flushSegment := func() {
		if len(cur) > 0 {
			segments = append(segments, strings.Join(cur, " "))
			cur = nil
		}
	}
	flushWindow := func() {
		flushSegment()
		if len(segments) > 0 {
			windows = append(windows, extensionDispatchWindow{text: strings.Join(segments, " "), segments: segments})
			segments = nil
		}
	}
	lines := strings.Split(doc, "\n")
	// nextIsIndented reports whether the next non-empty line after i is a continuation or nested
	// sub-bullet. A LOOSE markdown list (blank lines between items) is valid CommonMark and renders
	// identically to a tight one, so flushing on that blank line would split a parent bullet from
	// its nested mechanism — exactly the shape E3/E12 pin — and let a real regression through.
	nextIsIndented := func(i int) bool {
		for _, line := range lines[i+1:] {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return len(line) != len(strings.TrimLeft(line, " \t"))
		}
		return false
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if !nextIsIndented(i) {
				flushWindow()
			}
			continue
		}
		indented := len(line) != len(strings.TrimLeft(line, " \t"))
		item := extensionDispatchListItem.MatchString(trimmed)
		if item && !indented {
			flushWindow()
		}
		if item {
			flushSegment()
		}
		cur = append(cur, trimmed)
	}
	flushWindow()
	return windows
}

// extensionDispatchExcerpt bounds a reported window so a failure message stays readable. The cut is
// pulled back to a rune boundary: these documents are full of em dashes and `…`, and slicing a byte
// count out of one emits invalid UTF-8 into the test log.
func extensionDispatchExcerpt(window string) string {
	const limit = 240
	if len(window) <= limit {
		return window
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(window[cut]) {
		cut--
	}
	return window[:cut] + "…"
}

// extensionDispatchDirectives returns the sentences in an EXEMPT window that still instruct a
// Skill-tool load. Every corrected passage in the payload states `skillPath` in the same paragraph
// as the Skill-tool mention it corrects, so every site this gate protects sits inside an exempt
// window — appending a fallback clause to one of them ("if the `skillPath` file cannot be read,
// load it through the Skill tool instead") is a complete regression and by far the likeliest one.
// Window-level exemption therefore cannot be the last word.
//
// A sentence clears when it is the `<LENS_SKILL>` lens template, when it is allowlisted, or when
// every clause of it that mentions the Skill tool either names `skillPath` or is a prohibition.
func extensionDispatchDirectives(window extensionDispatchWindow) []string {
	var hits []string
	for _, segment := range window.segments {
		for _, sentence := range extensionDispatchSentence.Split(segment, -1) {
			if !extensionDispatchInstructsLoad(sentence) {
				continue
			}
			if extensionDispatchLensTemplate.MatchString(sentence) || extensionDispatchAllowed(sentence) {
				continue
			}
			for _, clause := range extensionDispatchClause.Split(sentence, -1) {
				if !extensionDispatchInstructsLoad(clause) {
					continue
				}
				if extensionDispatchRemedy.MatchString(clause) || extensionDispatchProhibition.MatchString(clause) {
					continue
				}
				hits = append(hits, strings.TrimSpace(sentence))
				break
			}
		}
	}
	return hits
}

// skillToolExtensionDispatches returns every passage in content that instructs loading something
// through the Skill tool without naming the `skillPath` remedy. A window qualifies unless it states
// the remedy, is a `lensMap` config-skill window, or is allowlisted; an exempt window is then
// re-examined sentence by sentence. Results are deduplicated in first-seen order.
func skillToolExtensionDispatches(content string) []string {
	var hits []string
	seen := map[string]bool{}
	add := func(passage string) {
		passage = extensionDispatchExcerpt(strings.TrimSpace(passage))
		if passage == "" || seen[passage] {
			return
		}
		seen[passage] = true
		hits = append(hits, passage)
	}
	for _, window := range extensionDispatchWindows(content) {
		if !extensionDispatchInstructsLoad(window.text) || extensionDispatchAllowed(window.text) {
			continue
		}
		if !extensionDispatchRemedy.MatchString(window.text) && !extensionDispatchConfigSkill.MatchString(window.text) {
			add(window.text)
			continue
		}
		for _, sentence := range extensionDispatchDirectives(window) {
			add(sentence)
		}
	}
	return hits
}

// extensionProseSurfaces returns every non-payload markdown file that carries the extension
// contract in prose: the normative contract, the rest of `docs/skills/` (the published authoring
// surface other repos build against), and the docs-site page. Scanning only the contract file left
// the sibling overviews free to keep documenting the superseded mechanism.
func extensionProseSurfaces(t *testing.T) []string {
	t.Helper()

	root := findRepoRoot(t)
	paths, err := filepath.Glob(filepath.Join(root, "docs", "skills", "*.md"))
	if err != nil {
		t.Fatalf("glob docs/skills: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("docs/skills/*.md matched no files; the extension-dispatch gate would skip its prose surfaces")
	}
	return append(paths, filepath.Join(root, "services", "docs", "docs", "skills", "extensions.md"))
}

// extensionDispatchScan calls visit with a label and the content of every markdown file this gate
// reads: both shipped payloads (the embedded skillinstall FS the boss CLI extracts and the on-disk
// claude plugin mirror bossd ships, because either one alone can be the copy a user's global skill
// directory is populated from) and the prose surfaces, which live outside BOTH payloads.
func extensionDispatchScan(t *testing.T, visit func(label, content string)) {
	t.Helper()

	for label, fsys := range shippedPayloads(t) {
		scanned := 0
		err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			scanned++
			visit(fmt.Sprintf("%s: published core file %q", label, strings.TrimPrefix(path, "skills/")), string(data))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", label, err)
		}
		// A payload that silently resolves to zero markdown files would make this gate vacuous.
		if scanned == 0 {
			t.Fatalf("%s: no markdown files scanned; the extension-dispatch gate would pass vacuously", label)
		}
	}

	for _, path := range extensionProseSurfaces(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		visit(fmt.Sprintf("published prose surface %s", filepath.Base(path)), string(data))
	}
}

// TestPublishedCoresDispatchExtensionsByPath is the BOS-663 regression gate. Every published core
// is extracted into each user's global skill directory, so a core that loads a discovered extension
// by its bare descriptor `name` via the Skill tool breaks that repo's whole extension layer the
// moment the extension declares `disable-model-invocation: true` — and breaks it SILENTLY, because
// the fallback tiers are suppressed on the extension being present rather than on the dispatch
// succeeding.
func TestPublishedCoresDispatchExtensionsByPath(t *testing.T) {
	extensionDispatchScan(t, func(label, content string) {
		for _, passage := range skillToolExtensionDispatches(content) {
			t.Errorf("%s instructs a Skill-tool load without naming the `skillPath` remedy: %q — the Skill tool refuses a skill declaring `disable-model-invocation: true`, which every extension SHOULD declare; read the descriptor's `skillPath` from disk instead", label, passage)
		}
	})
}

// TestExtensionDispatchAllowlistIsLive keeps the allowlist honest in the only direction that can
// hurt: an entry that no longer matches anything is a hole held open for nothing, and the next
// author to write a passage containing that phrase inherits a silent exemption. Entries are removed
// when the prose they cleared changes, never left to rot.
func TestExtensionDispatchAllowlistIsLive(t *testing.T) {
	used := make([]bool, len(extensionDispatchAllowlist))
	extensionDispatchScan(t, func(_, content string) {
		// Matched against joined windows, not raw file content: an entry is a phrase the gate sees
		// AFTER wrapped source lines are joined, and it earns its place only by clearing a window
		// that would otherwise be flagged.
		for _, window := range extensionDispatchWindows(content) {
			if !extensionDispatchInstructsLoad(window.text) {
				continue
			}
			for i, allowed := range extensionDispatchAllowlist {
				if strings.Contains(window.text, allowed) {
					used[i] = true
				}
			}
		}
	})
	for i, allowed := range extensionDispatchAllowlist {
		if !used[i] {
			t.Errorf("extensionDispatchAllowlist entry %q matches nothing in the shipped payloads or prose surfaces — delete it rather than leaving a standing exemption", allowed)
		}
	}
}

// extensionDispatchSkillPathSites is the number of times each dispatching core's spine names the
// `skillPath` mechanism. Each core dispatches extensions from more than one place in some cases
// (`boss-review` from three, `boss-build` from three), and a presence-only check stays green
// after the clause is deleted from all but one of them — a core can then ship a dispatch site with
// no mechanism at all while the negative gate above, which only sees passages that DO mention the
// Skill tool, has nothing to fire on.
//
// This is a FLOOR, not a census: it catches deletion, which is the failure mode. If you genuinely
// consolidate two dispatch sites into one, lower the number here in the same commit and say why.
//
// BOS-678 raised `boss-review` 3 → 4: Phase 1 gained a real per-lens Tier-1 dispatch of a
// discovered `lens` extension bound to a config lens id, which names the mechanism twice
// (`skillPath` from disk, and `skillPath` + `dir` in the worker brief). Without raising the floor
// that new site could be deleted again with every test still green.
// BOS-744 re-baselines `boss-plan` 1 → 7 and `boss-build` 3 → 5, and the reason is a REPAIR, not
// growth: this floor counts over `bodyFilesFor(core)`, and BOS-744 widened that spine (see
// coreBodyFiles in skills_manifest_test.go) so the new discovery-skip gate could see the discover
// sites that live in references. Widening the shared spine raises what every floor over it measures,
// so a floor left alone silently gains that much slack. Measured against the merged tree, `boss-plan`
// scored 1 over a one-file spine and now scores 7 over four; `boss-build` scored 5 over two files and
// now scores 7 over four. Re-baselining restores each core's PRE-WIDENING margin exactly — 0 for
// `boss-plan`, 2 for `boss-build` — rather than inventing a new tightness: without it, deleting
// boss-plan's only `skillPath` dispatch clause from SKILL.md left this gate green.
var extensionDispatchSkillPathSites = map[string]int{
	"boss-build":  5,
	"boss-epic":   1,
	"boss-plan":   7,
	"boss-repair": 1,
	"boss-review": 4,
}

// extensionDispatchResourceBaseSites counts actual extension-dispatch sites in each core's own
// SKILL.md. It deliberately excludes explanatory mentions of skillPath that do not dispatch a
// worker, such as boss-review's lensMap distinction.
//
// BOS-678 raised `boss-review` 2 → 3 for the new Phase 1 lens dispatch site, which is a real
// worker dispatch and therefore must carry the resource-base clause like the Phase R and Phase 8
// sites do. extensionDispatchResourceBase requires the literal "resolve ... from `dir`" form, so a
// paraphrased Tier-1 sentence fails here rather than shipping a worker that cannot reach the
// extension's sibling assets.
// BOS-744 re-baselines `boss-plan` 1 → 4 and `boss-build` 2 → 3, for the same spine-widening reason
// recorded on extensionDispatchSkillPathSites above. Both cores' pre-widening margin was 0, so the
// new numbers are the exact merged-tree counts and this gate is exactly tight again.
var extensionDispatchResourceBaseSites = map[string]int{
	"boss-build":  3,
	"boss-epic":   1,
	"boss-plan":   4,
	"boss-repair": 1,
	"boss-review": 3,
}

// TestPublishedCoresNameSkillPathForExtensionDispatch is the POSITIVE half of the gate above,
// which on its own only recognises the OLD bug. Without this, deleting the path-load clause
// outright from a core leaves every test green while that core ships no dispatch mechanism at all —
// staleness and silent deletion both slip through a negative-only pin.
//
// The pin covers only each core's spine, not every markdown file under its tree. BOS-744 pulled
// `boss-plan`'s three draft/plan-reviewer references INTO that spine (they hold discover sites the
// new skip gate has to see), so the spine is no longer a single file per core and the floor is what
// keeps the pin honest: `boss-plan` scores 7 across four files, and the floor is re-baselined to 7
// so deleting any one of them still turns this red. What is still excluded is everything OUTSIDE
// `bodyFilesFor` — a tree-wide search would count `skillPath` in reviewer briefs and contract prose
// that dispatch nothing, and would stay green after a real dispatch clause was deleted.
func TestPublishedCoresNameSkillPathForExtensionDispatch(t *testing.T) {
	for label, fsys := range shippedPayloads(t) {
		for core, want := range extensionDispatchSkillPathSites {
			got := 0
			for _, rel := range bodyFilesFor(core) {
				path := filepath.Join("skills", core, rel)
				data, err := fs.ReadFile(fsys, path)
				if err != nil {
					t.Fatalf("read %s %s: %v", label, path, err)
				}
				got += len(extensionDispatchRemedy.FindAll(data, -1))
			}
			if got < want {
				t.Errorf("%s: core %q names the `skillPath` load %d time(s), want at least %d — a dispatch site lost its mechanism, and the negative gate cannot catch an omission", label, core, got, want)
			}
		}
	}
}

func TestPublishedCoresPreserveExtensionResourceBase(t *testing.T) {
	for label, fsys := range shippedPayloads(t) {
		for core, want := range extensionDispatchResourceBaseSites {
			// BOS-674: count across the core's whole spine, not just SKILL.md — boss-build's
			// Step 12 dispatch site now lives in references/finalize-and-stop.md.
			got := 0
			for _, rel := range bodyFilesFor(core) {
				path := filepath.Join("skills", core, rel)
				data, err := fs.ReadFile(fsys, path)
				if err != nil {
					t.Fatalf("read %s %s: %v", label, path, err)
				}
				got += len(extensionDispatchResourceBase.FindAll(data, -1))
			}
			if got < want {
				t.Errorf("%s: core %q preserves the extension resource base %d time(s), want at least %d — a worker may lose access to sibling extension assets", label, core, got, want)
			}
		}
	}
}

// TestExtensionContractDocumentsPathLoading pins the authoring guidance the gate above enforces:
// the contract must tell extension authors to set `disable-model-invocation: true`, tell cores to
// load by `skillPath`, and carve out the `lensMap` config-skill case that is legitimately loaded
// by name. Without this, the contract could drift into silence while the gate stayed green.
func TestExtensionContractDocumentsPathLoading(t *testing.T) {
	contractPath := filepath.Join(findRepoRoot(t), "docs", "skills", "extension-contract.md")
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read extension contract: %v", err)
	}
	contract := string(data)
	// Collapsed before matching: every multi-word phrase this test pins spans prose that a human
	// rewrap can re-break at a different word. Prettier will not do it for them — `.prettierrc`
	// leaves `proseWrap` at its `preserve` default, so markdown prose is never reflowed and
	// `printWidth` does not apply to it — but a hand edit that rewraps a paragraph changing no
	// words would otherwise red every gate below. Single tokens are matched the same way; there is
	// no cost, and it keeps the rule "phrases are matched flat" rather than a per-entry judgement
	// call about how many words are too many.
	flatContract := whitespaceRun.ReplaceAllString(contract, " ")
	for _, want := range []string{
		"disable-model-invocation: true",
		"skillPath",
		"relative extension resources",
		"lensMap",
	} {
		if !strings.Contains(flatContract, want) {
			t.Errorf("docs/skills/extension-contract.md must document %q — extension authors and core authors both build against this file", want)
		}
	}

	// BOS-693: the contract is the source of truth cores are authored against, so the
	// partial-failure rule has to be stated HERE, not only in the cores that got fixed. Both
	// halves matter: skips are recorded per extension regardless of what its siblings did, and a
	// core whose extensions must PRODUCE something folds that into its own "succeeded".
	for _, want := range []struct{ phrase, why string }{
		{
			"including when a sibling succeeded",
			"must state that a failed extension is recorded as a skip even when a sibling succeeds — the pre-693 wording scoped recording to the all-extensions-failed branch, so a partial failure went unrecorded in the very case that suppresses the lower tiers",
		},
		{
			"same definition to both the suppression gate and the fall-through gate",
			"must require one definition of `succeeded` for both the suppression gate and the fall-through gate — two differently-worded gates for the same decision is how a tier gets silently skipped",
		},
		// An output check written as "the artifact exists now" tests SHARED state: siblings are
		// dispatched against one target, so the first extension to produce something credits
		// every sibling dispatched after it. The MUST is only sound if the contract also
		// requires the output to be attributed to the dispatch being classified, and says how.
		{
			"Attribute the output to the dispatch being classified",
			"must require the produced output to be attributed to the dispatch being classified — siblings share one target, so a bare existence check credits a silently-failing extension with a peer's artifact",
		},
		{
			"**Give each dispatch its own target**",
			"must say HOW to attribute the output — naming the hazard without the remedy leaves each core to invent its own check",
		},
		// …and the remedy has to hold on an arbitrary host. A before/after comparison of one
		// shared target does not: equal bytes are the ordinary output of a deterministic re-run,
		// and the mtime need not move either where the filesystem's timestamp resolution is
		// coarser than the rewrite — so a valid dispatch reads as skipped and a lower tier
		// overwrites its work. A per-dispatch target attributes by construction and, unlike a
		// provenance marker, needs nothing from the extension: the core picks the path it passes.
		{
			"a filesystem whose timestamp resolution is coarser than the rewrite stamps both writes the same",
			"must state why write-time inequality cannot carry the attribution — a core that reads a coarse-resolution mtime as proof records a valid dispatch as skipped and lets a lower tier overwrite its output",
		},
		{
			"the core chooses the value it passes",
			"must state that a per-dispatch target needs no agreement from the extensions — otherwise a core reads the remedy as unaffordable and falls back to comparing one shared target",
		},
	} {
		if !strings.Contains(flatContract, want.phrase) {
			t.Errorf("docs/skills/extension-contract.md %s (missing %q)", want.why, want.phrase)
		}
	}

	// The superseded remedy must be GONE, not merely outvoted: a core author who greps the
	// contract for an attribution rule and finds the mtime sentence still standing beside the
	// per-dispatch target has been handed back the exact signal this fix removed.
	for _, banned := range []struct{ phrase, why string }{
		{
			"compare the target's modification time across the dispatch",
			"must not offer timestamp inequality as an attribution remedy — it silently fails on any filesystem whose timestamp resolution is coarser than the rewrite",
		},
		{
			"inspect the target _before_ dispatching and require it to have changed",
			"must not send a core back to a before/after comparison of one shared target — byte inequality false-skips a deterministic redraft",
		},
	} {
		if strings.Contains(flatContract, banned.phrase) {
			t.Errorf("docs/skills/extension-contract.md %s (found %q)", banned.why, banned.phrase)
		}
	}
}

// TestExtensionContractDefinesWorkerBrief pins the transport the contract used to name and never
// explain. Pre-BOS-693 the phrase "worker brief" occurred exactly once in the whole document —
// inside the loading rule that requires `skillPath` and `dir` to be "carried in the worker brief" —
// while the only documented thing a dispatched extension receives, the invocation envelope, carries
// `role`/`core`/`context`/`runTmp`/`outPath` and neither of those two fields. A core author reading
// the contract therefore had no way to know how a worker obtains its own path or the base its
// relative resources resolve from, and an extension author could not tell which fields belong to
// the result protocol. The brief must be defined in its own right, name both descriptor-derived
// fields plus the loaded instructions, and be shown wrapping the envelope unchanged.
func TestExtensionContractDefinesWorkerBrief(t *testing.T) {
	contractPath := filepath.Join(findRepoRoot(t), "docs", "skills", "extension-contract.md")
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read extension contract: %v", err)
	}
	contract := string(data)

	// A single mention is the pre-fix state: the term must be used AND defined.
	if got := strings.Count(strings.ToLower(contract), "worker brief"); got < 2 {
		t.Fatalf("docs/skills/extension-contract.md mentions the worker brief %d time(s) — it must define the term, not just name it once as an unexplained transport", got)
	}

	// Locate the defining section and require the definition to live inside it, so a passing
	// grep cannot be satisfied by the fields being scattered across unrelated sections.
	heading := regexp.MustCompile(`(?im)^(#{2,4})\s+.*worker brief.*$`)
	loc := heading.FindStringSubmatchIndex(contract)
	if loc == nil {
		t.Fatalf("docs/skills/extension-contract.md must carry a section heading that defines the worker brief")
	}
	// Terminate on the next heading of the SAME or HIGHER level, not on `## ` alone: demoting the
	// worker-brief heading to `###` would otherwise let the section run on to the next `## ` and
	// let unrelated prose satisfy the token checks below.
	level := loc[3] - loc[2]
	section := contract[loc[1]:]
	sameOrHigher := regexp.MustCompile(fmt.Sprintf(`(?m)^#{1,%d} `, level))
	if next := sameOrHigher.FindStringIndex(section); next != nil {
		section = section[:next[0]]
	}
	// Collapsed for the same reason as the loop above: a hand rewrap of this section would
	// otherwise red the phrase gates below without changing a word. Slice first, then flatten —
	// flattening first would destroy the line starts the heading terminator matches on.
	flatSection := whitespaceRun.ReplaceAllString(section, " ")

	for _, want := range []struct{ token, why string }{
		{"`skillPath`", "the brief must name the descriptor-derived skillPath it carries"},
		{"`dir`", "the brief must name the descriptor-derived dir that relative extension resources resolve from"},
		{"relative extension resources", "the brief must state that relative resources resolve from dir"},
		{"SKILL.md", "the brief must state that it carries the loaded SKILL.md instructions"},
		{"invocation envelope", "the brief must be shown wrapping the invocation envelope"},
		// Not the bare word "unchanged", which any sentence in the section could supply by
		// accident: pin the phrase that actually asserts the envelope is passed through as-is.
		{"passed through unchanged", "the brief must state that it passes the invocation envelope through unchanged"},
	} {
		if !strings.Contains(flatSection, want.token) {
			t.Errorf("the worker-brief section must contain %q: %s", want.token, want.why)
		}
	}

	// The envelope's stable shape is what extension authors build against; the brief must not be
	// documented as replacing or extending it.
	for _, field := range []string{"`role`", "`core`", "`context`", "`runTmp`", "`outPath`"} {
		if !strings.Contains(flatSection, field) {
			t.Errorf("the worker-brief section must name the unchanged envelope field %s so authors can tell brief plumbing from the result protocol", field)
		}
	}
}

// TestSkillToolExtensionDispatchesDetection pins the classifier itself. The A cases are the four
// literal pre-fix sentences BOS-663 removed, so the detector is proven to fire on the exact bug
// rather than on a paraphrase invented after the fix. The E cases are evasion shapes review rounds
// constructed against earlier cuts of the detector. The negative cases pin the boundary that makes
// the gate usable: a `lensMap` config skill IS loaded by name via the Skill tool and must not
// match, and neither must prose that names both concepts while stating the `skillPath` remedy.
func TestSkillToolExtensionDispatchesDetection(t *testing.T) {
	matched := map[string]string{
		"A1 boss-plan interactive-mode": "- **Tier 1:** if one or more extensions are returned, load each discovered extension by its returned\n" +
			"  descriptor `name` via the Skill tool on the main thread, passing\n" +
			"  `{ role: \"draft\", core: \"boss-plan\", context: { mode: \"interactive\", planPath, ticket, designDoc },\n" +
			"runTmp, outPath }`. The extension owns the interview and writes the plan.",
		"A2 boss-plan headless-drafting-brief": "- **Tier 1:** if a draft extension exists, load each discovered extension by its returned descriptor\n" +
			"  `name` via the Skill tool with\n" +
			"  `{ role: \"draft\", core: \"boss-plan\", context: { mode: \"headless\" },\n" +
			"runTmp, outPath }`.",
		"A3 boss-plan extension-reviewers": "   The subagent loads the extension's `SKILL.md` via the Skill tool and receives the `plan-reviewer`\n" +
			"   **invocation envelope** from the contract:",
		"A4 extension-contract": "Each extension is dispatched as a **fresh, read-only subagent** that loads the extension's\n" +
			"`SKILL.md` via the Skill tool and receives:",
		// Evasion shapes reviewers constructed against earlier cuts of the detector. Each is a
		// realistic regression written in the cores' own register that the classifier once missed.
		"E1 contrastive opener launders the directive": "Rather than reimplementing the round inline, load each discovered extension by its\n" +
			"descriptor `name` via the Skill tool.",
		"E2 role-qualified subject": "Use the Skill tool to load the discovered `round` extension, then pass it the envelope.",
		"E3 subject in parent bullet, mechanism in nested bullet": "- **Tier 1:** if `ROUNDS_JSON.extensions` is non-empty, dispatch each discovered extension in\n" +
			"  ascending `(order, name)` order:\n" +
			"  - Load it via the Skill tool, passing the standard envelope.",
		"E4 possessive descriptor reference": "For each descriptor returned by discovery, load the corresponding skill via the Skill tool\n" +
			"using the descriptor's `name` field.",
		"E5 negated conditional clause": "If the host does not expose a subagent tool, load each discovered extension by its descriptor\n" +
			"`name` via the Skill tool.",
		"E6 semicolon after a leading prohibition": "Never dispatch by path alone; load each discovered extension by its descriptor `name` via the\n" +
			"Skill tool.",
		"E7 backticked tool spelling": "Load each discovered extension by its descriptor `name` via the `Skill` tool.",
		"E8 hyphenated tool spelling": "Each extension is loaded with a Skill-tool invocation keyed on its descriptor `name`.",
		"E9 by-name fallback appended to a corrected paragraph": "Load each extension by **reading the descriptor's `skillPath` from disk** (`dir` is its\n" +
			"directory) and passing that `SKILL.md` content into the dispatch — never by its bare\n" +
			"descriptor `name` through the Skill tool. If the `skillPath` file cannot be read, load the\n" +
			"extension by its descriptor `name` via the Skill tool instead.",
		"E10 by-name clause appended to the lensMap paragraph": "`<LENS_SKILL>` is a `.boss-skills.json` `lensMap` config value loaded by name via the Skill\n" +
			"tool. Discovered round extensions are dispatched by their descriptor `name` through the\n" +
			"Skill tool as well.",
		// E11 is E9 with the word "name" removed. The exempt-window override once keyed on the
		// literal token `name`, so a fallback clause that never says it slipped through — and every
		// corrected passage in the payload contains `skillPath`, which means every site this gate
		// protects sits inside an exempt window. Editing one of those is the likeliest regression.
		"E11 nameless fallback appended to a corrected paragraph": "Load each extension by **reading the descriptor's `skillPath` from disk** (`dir` is its\n" +
			"directory) and passing that `SKILL.md` content into the dispatch. If the `skillPath` file\n" +
			"cannot be read, load the extension through the Skill tool instead.",
		// E12 is E3 written as a LOOSE markdown list (a blank line before the nested bullet), which
		// is valid CommonMark and renders identically. Flushing the window on that blank line split
		// the subject from the mechanism and let the shape E3 exists to pin walk straight through.
		"E12 loose list splits subject from mechanism": "- **Tier 1:** if `ROUNDS_JSON.extensions` is non-empty, dispatch each discovered extension in\n" +
			"  ascending `(order, name)` order:\n" +
			"\n" +
			"  - Load it via the Skill tool, passing the standard envelope.",
		// E13 launders a real extension dispatch by name-dropping the config-skill carve-out. Every
		// `boss-review` Phase 1 window already mentions `lensMap`, so if a lens seam ever grows a
		// real discovered-extension binding, the carve-out would exempt the whole thing. Only the
		// literal `<LENS_SKILL>` template placeholder clears a sentence.
		"E13 lensMap name-drop launders an extension dispatch": "Each discovered lens extension is loaded by its descriptor `name` via the Skill tool, exactly\n" +
			"as a `lensMap` entry is.",
		// E14 is E9 with a single stray "never" moved into the fallback sentence. A sentence-scoped
		// prohibition test cleared the whole sentence on that word alone, laundering the regression
		// while E9 itself stayed caught. The prohibition is now tested per CLAUSE: the clause that
		// carries the Skill-tool load has no negation in it, and the one that does is a failure
		// condition, not a prohibition.
		"E14 stray prohibition word launders the fallback": "Load each extension by **reading the descriptor's `skillPath` from disk** (`dir` is its\n" +
			"directory) and passing that `SKILL.md` content into the dispatch — never by its bare\n" +
			"descriptor `name` through the Skill tool. If the `skillPath` file can never be read, load\n" +
			"the extension by its descriptor `name` via the Skill tool instead.",
		// E15/E16 drop the subject entirely: the exempt window states its remedy (E15) or its
		// config-skill carve-out (E16) in one sentence and dispatches in the next, referring back
		// with a bare pronoun. A gate that had to recognise "a discovered extension" to fire missed
		// both, because "it" is not a subject any pattern can name.
		"E15 next-sentence bare load inside the skillPath carve-out": "Load each extension by reading the descriptor's `skillPath` from disk and passing that\n" +
			"content into the dispatch. Load it through the Skill tool.",
		"E16 next-sentence bare load inside the lensMap carve-out": "`<LENS_SKILL>` comes from the `.boss-skills.json` `lensMap` and is loaded by name.\n" +
			"Load it through the Skill tool.",
		// E17 is the cross-bullet prohibition leak. Markdown bullets rarely carry terminal
		// punctuation, so a whole nested list collapses into one "sentence" and the prohibition in
		// the first bullet cleared the directive three bullets later. Windows now keep their list
		// segments apart.
		"E17 prohibition in one bullet leaks across the list": "- **Tier 1:** dispatch each discovered extension:\n" +
			"  - Never load it by its bare descriptor `name` via the Skill tool\n" +
			"  - Read the descriptor's `skillPath` from disk and pass that content into the dispatch\n" +
			"  - If that read fails, load it via the Skill tool",
		// E18/E19/E20 put the subject somewhere the window never sees it — the previous paragraph, a
		// heading, an earlier numbered step. All three returned zero hits while the gate went to
		// great trouble to pin the nested-bullet variant; the inversion dissolves the distinction,
		// because the dispatching passage no longer has to name what it dispatches.
		"E18 subject in the adjacent paragraph": "Discovered round extensions are dispatched in ascending `(order, name)` order.\n" +
			"\n" +
			"Load each one via the Skill tool, passing the standard envelope.",
		"E19 subject in the heading": "## Dispatching discovered extensions\n" +
			"\n" +
			"Load it via the Skill tool with the standard envelope.",
		"E20 subject in an earlier numbered step": "1. Run `skill-extensions.mjs discover` to enumerate the round extensions.\n" +
			"2. Load each via the Skill tool.",
		// E21 is E13 realised. BOS-678 gave Phase 1 a REAL discovered-extension dispatch: a `lens`
		// extension whose marker declares `lens: <id>` binds to the matching config entry and becomes
		// that lens's Tier 1. Every Phase 1 window already name-drops `lensMap`, so a Tier-1 sentence
		// that loads the bound extension by descriptor `name` — citing the config entry as its
		// justification — is laundered by the window-level carve-out, which is exactly what E13
		// predicted would happen "if a lens seam ever grows a real discovered-extension binding".
		// Only the literal `<LENS_SKILL>` template placeholder clears a sentence, so this still fires.
		"E21 bound lens extension laundered by the lensMap carve-out": "When a discovered lens extension declares a binding matching a `lensMap` entry, load that\n" +
			"extension by its descriptor `name` via the Skill tool, exactly as the entry's own `skill` is\n" +
			"loaded.",
	}
	for name, content := range matched {
		t.Run(name, func(t *testing.T) {
			if got := skillToolExtensionDispatches(content); len(got) == 0 {
				t.Errorf("skillToolExtensionDispatches() = no hits, want at least one")
			}
		})
	}

	ignored := map[string]string{
		"lens template loads a config-named skill": "  You are a code reviewer. Load the `<LENS_SKILL>` skill via the Skill tool and review the\n" +
			"  following changed files through that skill's lens ONLY.",
		"lens template fallback": "  If `<LENS_SKILL>` cannot be loaded\n" +
			"  (the Skill tool does not find it), do NOT abort: <LENS_FALLBACK> and continue the review.",
		"lens clarifying sentence": "`<LENS_SKILL>` is a `.boss-skills.json` `lensMap` config value naming a model-invocable global " +
			"skill, never a discovered extension descriptor, so it is loaded by name via the Skill tool.",
		"Skill tool with no extension in scope": "If the host exposes a Skill tool affordance for its own built-in commands, prefer it.",
		"discovered extension with no Skill-tool load": "Dispatch each discovered extension in ascending `(order, name)` order as a fresh awaited\n" +
			"subagent bounded by `BOSS_SKILL_EXTENSION_TIMEOUT_MS`.",
		"the canonical replacement wording": "Load the extension by **reading the descriptor's `skillPath` from disk** (`dir` is its directory)\n" +
			"and passing that `SKILL.md` content into the dispatch as the extension's instructions. Never load\n" +
			"a discovered extension by its bare descriptor `name` via the Skill tool: extension skills are\n" +
			"dispatched explicitly, never model-matched, so they SHOULD declare\n" +
			"`disable-model-invocation: true`, and the Skill tool refuses such a skill.",
		"rationale stated before the prohibition": "The Skill tool refuses a skill declaring `disable-model-invocation: true`, so read the\n" +
			"descriptor's `skillPath` from disk and pass that content into the dispatch.",
		// The REAL BOS-678 Phase 1 Tier-1 passage, copied verbatim from the shipped core. It is the
		// counterpart to E21: the same seam, written correctly, must not be flagged. It also pins the
		// canonical resource-base wording that `extensionDispatchResourceBaseSites` counts — a
		// paraphrase would fail that floor rather than this fixture, so both halves are held.
		"the new Phase 1 bound-lens Tier-1 wording": "### Tier 1 — a discovered lens extension bound to this lens id\n" +
			"\n" +
			"Load that extension by **reading the descriptor's `skillPath` from disk** (`dir` is its\n" +
			"directory), passing both `skillPath` and `dir` in the worker brief, and requiring relative extension\n" +
			"resources to resolve from `dir`. Pass that `SKILL.md` content into the dispatch as the extension's instructions —\n" +
			"never by its bare descriptor `name` through the Skill tool, which refuses a skill declaring\n" +
			"`disable-model-invocation: true`.\n" +
			"Each dispatch is a fresh `general-purpose` subagent, **awaited**, read-only, and receives the\n" +
			"standard extension invocation envelope, whose `changedFiles` is **this lens's matched subset**, not\n" +
			"the whole branch:",
		// The `BASE` bullet is the ONE shape in the real payload the inversion flags wrongly: the
		// core describes how IT is entered, not how an extension is loaded, and has no `skillPath`
		// to name. It is cleared by the documented allowlist, not by a pattern.
		"the core's own Skill-tool entry point": "- `BASE` — the base branch to diff against: arg `$1` when a caller passes one, else the repo\n" +
			"  default (`origin/HEAD`), else `main`. Invoked via the `Skill` tool there is usually no `$1`,\n" +
			"  so it resolves to the default.",
		"one core entering another core": "After the Step 6b outside-voice pass and **before** Step 7, run one **`boss-review`** pass — a\n" +
			"consolidated, multi-lens review over the implementation branch. Invoke it via the `Skill` tool\n" +
			"(`boss-review`, no args → it reviews the current branch against its merge-base with the default base).\n" +
			"Conditional specialist **lenses** come from the `lensMap` in `.boss-skills.json`.",
		"the lensMap carve-out explained in the contract": "- A `lensMap`-style config `skill` value is **exempt**: those name a foreign, _model-invocable_\n" +
			"  global skill (`golang-pro`, `impeccable`, …) with no known on-disk path in an arbitrary\n" +
			"  checkout, so the Skill tool is the correct — and only — way to load one. A `lensMap` entry\n" +
			"  is not a discovered extension descriptor.",
	}
	for name, content := range ignored {
		t.Run(name, func(t *testing.T) {
			if got := skillToolExtensionDispatches(content); len(got) != 0 {
				t.Errorf("skillToolExtensionDispatches() = %v, want none", got)
			}
		})
	}
}

// BOS-758: Phase R must dispatch its discovered round descriptors CONCURRENTLY.
//
// A round extension is a read-only whole-branch reviewer with its own `outPath`, so N rounds
// dispatched one at a time cost the SUM of every reviewer instead of the slowest — minutes of
// serial reviewer time before any fix or gate runs, multiplied by the round cap. Phase 1 already
// dispatches the same class of read-only reviewer "in parallel (one message, multiple `Task`
// calls)", which is the precedent this gate holds Phase R to.
//
// The gate is scoped to the Phase R Tier-1 passage rather than the whole file because
// `boss-review` carries a SECOND "Tier 1" heading (the Phase 1 bound-lens site), and a file-wide
// search for "in parallel" would be satisfied by Phase 1's own sentence while Phase R stayed
// serial.
var phaseRTier1Heading = regexp.MustCompile(`(?m)^(#{2,6})\s+Tier 1 — repo-local round extensions\s*$`)

// phaseRRoundDispatchSerial matches wording that makes `(order, name)` a DISPATCH-SEQUENCING
// instruction, or otherwise says the rounds run one after another. The first alternative is the
// literal pre-BOS-758 sentence ("dispatch each descriptor in ascending `(order, name)` order")
// generalised over the noun, so restoring it under a different noun is caught too.
// The alternation is deliberately wider than the shapes seen so far: a reviewer falsified an
// earlier cut by rewriting the passage as "work through the descriptors one by one, awaiting each
// before starting the next", which every then-listed alternative missed while a Phase-1-referencing
// parenthetical supplied the positive tokens. Serial dispatch has many idiomatic spellings, so
// prefer over-matching here — a false positive is a wording nudge, a miss ships the bug.
var phaseRRoundDispatchSerial = regexp.MustCompile(`(?i)dispatch(?:es|ing)?\s+(?:each|every)(?:\s+\S+){0,4}\s+in\s+ascending|\bone at a time\b|\bone-at-a-time\b|\bone by one\b|\bone-by-one\b|\bsequential(?:ly)?\b|\bserial(?:ly)?\b|\bin turn\b|\bsingly\b|\bawait(?:ing)? each\b|\bbefore starting the next\b|\bbefore the next one\b`)

// phaseRRoundDispatchNegated matches the SANCTIONED negated forms — "no longer one at a time",
// "never sequentially", "rather than serially" — which describe what the passage stopped doing.
// Over-matching is the right default for the detector above, but not at the cost of forbidding the
// clearest way to document the change: without this the gate fires on a correct passage and its
// message ("still instructs serial round dispatch") accuses the author of the opposite of what they
// wrote. Strip these first, exactly as the sibling .mjs coverage gate drops its sanctioned
// "rather than appending" before rejecting a bare "append".
// The negator must sit IMMEDIATELY before the serial term. An earlier cut allowed up to two words
// between them without checking what was being negated, which stripped "instead of parallel
// dispatch one at a time" and "rather than parallel run one at a time" — genuinely serial
// instructions that then passed the detector silently. Exempting a phrase must never be able to
// exempt the bug it contains.
var phaseRRoundDispatchNegated = regexp.MustCompile(`(?i)\b(?:never|not|no longer|rather than|instead of)\s+(?:sequential(?:ly)?|serial(?:ly)?|one at a time|one-at-a-time|one by one|one-by-one|singly|in turn)\b`)

// phaseRRoundDispatchGoverns requires the parallel instruction to GOVERN the descriptors rather
// than merely appear in the passage. Without it the gate is satisfied by any nearby mention of
// parallelism — including a parenthetical that contrasts Phase 1's parallel lenses while telling
// the reader to take rounds singly, which is the mutant that defeated the token-only form.
var phaseRRoundDispatchGoverns = regexp.MustCompile(`(?i)dispatch\s+\*\*every\*\*\s+discovered\s+round\s+descriptor\s+\*\*in\s+parallel\*\*`)

// phaseRTier1Passage returns the Tier-1 round-extension section of a `boss-review` payload, with
// whitespace collapsed so a prose gate pins words rather than the line breaks prettier chose. The
// section ends at the next heading of the same or a higher level, so *promoting* Tier 2 cannot
// escape the window; demoting it below `###` would widen it (past Tier 2 and Tier 3 to the next
// `##`), which loosens the positive tokens rather than hiding a regression.
func phaseRTier1Passage(t *testing.T, label, doc string) string {
	t.Helper()

	loc := phaseRTier1Heading.FindStringSubmatchIndex(doc)
	if loc == nil {
		t.Fatalf("%s: no `Tier 1 — repo-local round extensions` heading — the Phase R round-dispatch passage moved or was renamed; re-point this gate rather than deleting it", label)
	}
	level := loc[3] - loc[2]
	section := doc[loc[1]:]
	sameOrHigher := regexp.MustCompile(fmt.Sprintf(`(?m)^#{1,%d} `, level))
	if next := sameOrHigher.FindStringIndex(section); next != nil {
		section = section[:next[0]]
	}
	return whitespaceRun.ReplaceAllString(section, " ")
}

// TestPublishedCoresDispatchRoundExtensionsInParallel pins BOS-758 in both shipped payloads: the
// Phase R Tier-1 passage must instruct a concurrent dispatch, must say the `(order, name)` sort
// survives only for post-return ledger/report assembly, and must keep every load-bearing clause
// that concurrency depends on — per-descriptor `outPath`s (no two rounds share a file), the
// awaited/never-backgrounded rule (parallel is several awaited calls in one message, NOT
// backgrounding), and the per-extension timeout that bounds a hung round.
func TestPublishedCoresDispatchRoundExtensionsInParallel(t *testing.T) {
	for label, fsys := range shippedPayloads(t) {
		for _, rel := range bodyFilesFor("boss-review") {
			path := filepath.Join("skills", "boss-review", rel)
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				t.Fatalf("read %s %s: %v", label, path, err)
			}
			passage := phaseRTier1Passage(t, fmt.Sprintf("%s %s", label, path), string(data))

			for _, want := range []struct{ token, why string }{
				{"in parallel", "the rounds must be dispatched concurrently, as Phase 1 already dispatches its lenses"},
				{"one message, multiple `Task` calls", "parallel must be spelled out as the Phase 1 mechanism, not left to interpretation"},
				{"after the dispatches return", "the `(order, name)` sort must be scoped to post-return assembly"},
				{"`(order, name)`", "the deterministic ledger/report ordering must still be named"},
				// Load-bearing clauses concurrency depends on. Each is counted elsewhere as a
				// floor, but a rewrite of THIS paragraph is exactly how one gets dropped.
				{"`skillPath`", "the path-load mechanism must survive the rewrite"},
				{"resolve from `dir`", "relative extension resources must still resolve from the descriptor directory"},
				{"`run_in_background`", "parallel dispatch must not be mistaken for backgrounding"},
				{"BOSS_SKILL_EXTENSION_TIMEOUT_MS", "each concurrent round must stay individually bounded"},
				{"findings-round-<extension-name>.json", "per-descriptor outPaths are what make concurrent rounds safe"},
				// The determinism GUARANTEE is the merge/ledger instruction, not the claim in the
				// dispatch paragraph. Those two live 35 lines apart in the same section, and the
				// claim's own tokens (`(order, name)`, "after the dispatches return") stay present
				// when the instruction is reverted to the completion-ordered "record … and
				// continue" — so pinning the claim alone let the real regression through.
				{"do not merge or write the ledger as they arrive", "merging as each round returns is completion-ordered, the nondeterminism the claim above rules out"},
				// No leading "once": this token starts a sentence in the shipped prose, and
				// strings.Contains is case-sensitive.
				{"**all** rounds have returned", "the ordered merge/ledger pass is what actually makes the pool and evidence rows byte-stable"},
			} {
				if !strings.Contains(passage, want.token) {
					t.Errorf("%s %s: Phase R Tier 1 must contain %q: %s", label, path, want.token, want.why)
				}
			}

			if !phaseRRoundDispatchGoverns.MatchString(passage) {
				t.Errorf("%s %s: Phase R Tier 1 must instruct dispatching **every** discovered round descriptor **in parallel** — the tokens above can all be supplied by prose that merely mentions parallelism while still dispatching the rounds one by one", label, path)
			}

			// Drop the sanctioned negated forms before looking for a serial instruction, so
			// "no longer one at a time" reads as the correction it is rather than the bug.
			asserted := phaseRRoundDispatchNegated.ReplaceAllString(passage, " ")
			if hit := phaseRRoundDispatchSerial.FindString(asserted); hit != "" {
				t.Errorf("%s %s: Phase R Tier 1 still instructs serial round dispatch (%q) — N read-only rounds must cost the slowest, not the sum", label, path, hit)
			}
		}
	}
}
