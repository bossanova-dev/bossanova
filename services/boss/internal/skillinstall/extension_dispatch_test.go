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

// extensionDispatchSkillPathSites is the number of times each dispatching core's OWN `SKILL.md`
// names the `skillPath` mechanism. Each core dispatches extensions from more than one place in some
// cases (`boss-review` from three, `boss-build` from two), and a presence-only check stays green
// after the clause is deleted from all but one of them — a core can then ship a dispatch site with
// no mechanism at all while the negative gate above, which only sees passages that DO mention the
// Skill tool, has nothing to fire on.
//
// This is a FLOOR, not a census: it catches deletion, which is the failure mode. If you genuinely
// consolidate two dispatch sites into one, lower the number here in the same commit and say why.
var extensionDispatchSkillPathSites = map[string]int{
	"boss-build":  2,
	"boss-epic":   1,
	"boss-plan":   1,
	"boss-repair": 1,
	"boss-review": 3,
}

// extensionDispatchResourceBaseSites counts actual extension-dispatch sites in each core's own
// SKILL.md. It deliberately excludes explanatory mentions of skillPath that do not dispatch a
// worker, such as boss-review's lensMap distinction.
var extensionDispatchResourceBaseSites = map[string]int{
	"boss-build":  2,
	"boss-epic":   1,
	"boss-plan":   1,
	"boss-repair": 1,
	"boss-review": 2,
}

// TestPublishedCoresNameSkillPathForExtensionDispatch is the POSITIVE half of the gate above,
// which on its own only recognises the OLD bug. Without this, deleting the path-load clause
// outright from a core leaves every test green while that core ships no dispatch mechanism at all —
// staleness and silent deletion both slip through a negative-only pin.
//
// The pin is anchored to each core's OWN `SKILL.md`, not to any markdown under its tree: `boss-plan`
// names `skillPath` in three references covering the draft and plan-reviewer roles, so a tree-wide
// search would stay green after the clause was deleted from the resident body's notes dispatch.
func TestPublishedCoresNameSkillPathForExtensionDispatch(t *testing.T) {
	for label, fsys := range shippedPayloads(t) {
		for core, want := range extensionDispatchSkillPathSites {
			path := filepath.Join("skills", core, "SKILL.md")
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				t.Fatalf("read %s %s: %v", label, path, err)
			}
			if got := len(extensionDispatchRemedy.FindAll(data, -1)); got < want {
				t.Errorf("%s: core %q names the `skillPath` load %d time(s), want at least %d — a dispatch site lost its mechanism, and the negative gate cannot catch an omission", label, core, got, want)
			}
		}
	}
}

func TestPublishedCoresPreserveExtensionResourceBase(t *testing.T) {
	for label, fsys := range shippedPayloads(t) {
		for core, want := range extensionDispatchResourceBaseSites {
			path := filepath.Join("skills", core, "SKILL.md")
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				t.Fatalf("read %s %s: %v", label, path, err)
			}
			if got := len(extensionDispatchResourceBase.FindAll(data, -1)); got < want {
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
	for _, want := range []string{
		"disable-model-invocation: true",
		"skillPath",
		"relative extension resources",
		"lensMap",
	} {
		if !strings.Contains(contract, want) {
			t.Errorf("docs/skills/extension-contract.md must document %q — extension authors and core authors both build against this file", want)
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
