#!/usr/bin/env node

// check-skill-shell — a `bash -n` syntax gate over every fenced shell block in the skill tree,
// plus one non-syntactic predicate (at most one UNQUOTED glob per `rm`/`find` invocation).
//
// Why this exists: a mid-review fix once left a markdown fence unclosed in a SKILL.md; prettier
// normalized it to a four-backtick fence, which swallowed the block below it. The phase became a
// bash parse error, a helper was never defined, and the skill's write branch exited 0 reporting
// "nothing to create". That class is invisible to prettier, to `make codex-skills-check`, and to
// every substring assertion. It is not invisible to the shell's own parser.
//
// The rules below are decided and written down so the next author does not re-litigate them.
//
// (a) PLACEHOLDER CONVENTION, and its accepted false-negative.
//     Skill blocks are fragments, not scripts: `git add <exact-files>`, `boss chat show
//     <session-id|chat-id>`. To bash an unquoted `<word>` is an input redirection, so those are
//     genuine syntax errors that say nothing about the author's intent. Before `bash -n`, every
//     match of /(?<!<)<(?![<\s(])[^<>\n]*>/g is replaced with the bare word `PLACEHOLDER`. The
//     negative lookahead excludes `<<` (heredoc), `<<<` (herestring), `< file` (space-separated
//     redirection) and `<(…)` (process substitution); the negative LOOKBEHIND is what keeps a
//     redirecting heredoc intact — without it `cat <<'EOF' > out.txt` matches from the SECOND `<`
//     through the `>` and is rewritten to `cat <PLACEHOLDER out.txt`, destroying the heredoc
//     operator so that the literal heredoc body is re-parsed as live shell. That is a
//     false-POSITIVE, which this convention must never produce. So real redirection is parsed.
//     ACCEPTED COST: a same-line `cmd <in >out` pair is rewritten to `cmd PLACEHOLDERout`, which
//     still parses — so the gate can miss a malformed redirection on such a line. That is a
//     deliberate false-NEGATIVE, narrow and one-directional; the gate never false-POSITIVES a
//     correct block. Skipping placeholder-bearing blocks instead was rejected: it would silently
//     drop ~14 of the most command-dense blocks in the tree, which is exactly the coverage this
//     gate exists for.
//
// (b) TWO AUTHORING ROOTS ONLY — no mirrors. `plugins/bossd-plugin-claude/skilldata/skills/` is
//     byte-identical to the canonical cores (kept in sync by `make copy-skills`, asserted by the
//     "plugin mirror SKILL.md is byte-identical to the canonical SKILL.md" tests in
//     scripts/bs-plan-skill.test.mjs and scripts/boss-build-skill.test.mjs). `.codex/skills/` is a
//     verbatim-body copy of `.claude/skills/` produced by scripts/sync-codex-skills.mjs and gated
//     by `make codex-skills-check`. Scanning either would triple the runtime and report every
//     offender two or three times with a mirror path a fixer must NOT edit. So the gate names no
//     mirror path, ever.
//
// (c) FENCE-CLOSING IS >=-LENGTH, NOT ==. A fence is closed by a run of the SAME character, AT
//     LEAST as long as the one that opened it, on a line whose remainder (after the container
//     prefix is stripped) is blank. This is what makes the intentional four-backtick template
//     fences safe: they wrap an inner three-backtick block, and ``` does not close ````. An
//     opened-but-never-closed fence is reported as its own `unterminated` finding rather than
//     silently swallowing the rest of the file — that is the most direct signal for the failure
//     described at the top.
//
// (d) CONTAINER-PREFIX GRAMMAR, deliberately small and sufficient for this corpus:
//         prefix := [ \t]* ( '>' [ \t]{0,4} )* [ \t]*
//     Four, because the allowances are ADDITIVE: one optional space is consumed after a marker, and
//     the nested marker may then carry up to three columns of its own indentation, so `>    > ` is
//     valid. Allowing fewer fails to see the second marker and the block is never extracted at all —
//     a silent coverage hole rather than a finding.
//     It covers blockquote containers (`> ```bash `, 14 blocks today, invisible to a
//     whitespace-only extractor) and list indentation in one rule. The OPENING fence's prefix is
//     stripped from every body line, exactly as a Markdown renderer does; a body line that does
//     not carry that prefix falls back to stripping whatever container prefix it does carry.
//     Dedent is load-bearing: an indented `<<'EOF'` heredoc whose terminator carries the fence
//     indentation fails to parse without it. A fence nested inside a list item inside a block
//     quote (mixed prefix) is not modelled; none exists, and such a case would surface loudly as
//     `unterminated` rather than be silently skipped.
//
// (e) BASH PARSER, NOT POSIX `sh` VALIDATION. `bash -n` is run over every accepted label
//     (bash/sh/shell/zsh) using the `bash` found on PATH, with NO options — no `shopt`, no `-O`,
//     no `set -o posix` — because the gate's job is to match the parser skill blocks actually
//     execute under. A green run therefore does NOT prove a block is portable POSIX `sh`.
//     `console` is deliberately excluded: it conventionally holds prompts and command output.
//     Keying is on bash's EXIT STATUS, never on the word "error" in its stderr.
//     CAVEAT, measured: the status cannot see an UNTERMINATED HEREDOC. On a body of `cat <<EOF`,
//     bash 5.3.9 prints `warning: here-document ... delimited by end-of-file` and exits 0, and bash
//     3.2.57 (macOS /bin/bash) prints nothing and exits 0. Neither the status nor stderr is a
//     portable signal, and the consequence — the rest of the block swallowed as payload, its
//     commands never run — is this gate's founding failure. So it is decided by `findUnterminatedHeredoc`
//     from the block's own heredoc scan (kind `heredoc`), which behaves identically on every bash.
//
//     SECOND ACCEPTED FALSE-NEGATIVE, in the same one direction: an arithmetic region is stepped
//     over whole, so a command substitution nested inside one — `x=$(( $(cat <<EOF) + 1 ))` — is not
//     re-entered, and an unterminated heredoc in there is not reported. Deliberate. Inside `$((…))`
//     a `<<` is a left SHIFT, and stepping over the region whole is precisely what stops its operand
//     being read as a heredoc delimiter that blanks the rest of the block — a false POSITIVE, the
//     direction this gate must never fail in. Measured against the corpus before deciding: blocks
//     with a substitution inside arithmetic exist (4 files), but blocks with a heredoc inside
//     arithmetic number ZERO, so recursing would put real blocks at risk of the shift/heredoc
//     confusion to chase a construct that does not occur. Revisit only with a real instance in hand.
//
// (f) THE GLOB RULE COUNTS ONLY UNQUOTED GLOBS. Under zsh and fish an unmatched glob aborts the
//     WHOLE command line, so every co-located removal on that line is silently skipped. `bash -n`
//     cannot catch this — `rm -f "a"*.md "b"*.md` is valid syntax — so it is a predicate layered
//     on the same fence walker. "Unquoted" is the correctness of the rule, not a detail:
//     `find … -name "*.yml" -o -name "*.yaml"` has QUOTED patterns that `find` matches internally
//     and the shell never expands, so it cannot exhibit the abort bug and must not be flagged.
//     The sanctioned fix form uses a single-quoted `-name '<PATTERN>'`; under a quoted-counting
//     rule that fix would sit one edit away from tripping the gate meant to protect it.
//
// (g) GH BODY FIELDS MUST NOT SHELL-INTERPOLATE MARKDOWN. `gh api -f body="...`...`"` and
//     `body="$(...)"` execute the Markdown's command substitutions before `gh` receives it.
//     This predicate rejects only `body=` values supplied through `-f`/`--field`/`--raw-field`
//     when they are unquoted or double-quoted and carry a backtick or `$(`. A single-quoted value
//     is literal shell text, and `@file` is accepted only with `-F`/`--field`: `-F body=@"$FILE"`
//     is the safe submission form for arbitrary Markdown, while `-f`/`--raw-field` submit that
//     value literally. Its producer is checked separately:
//     heredoc framing can collide with reply text even when quoted, and an unquoted delimiter also
//     expands the Markdown while writing it. This is deliberately a narrow safety rule, not a ban
//     on double quotes or an attempt to parse all shell expansions.
//
// (g) INERT GUARDS ARE REJECTED. `test -f tool || echo missing` looks defensive but falls through
//     and lets the rest of the block run. The rule only recognises `test`/`[` guards, a braced
//     fallback, `|| exit`/`|| return`, and bare `command -v … ||`; it deliberately does not infer
//     arbitrary shell control flow. A block whose first non-comment command is `set -e` (including
//     `set -euo pipefail`) is accepted; otherwise every recognised failure branch must reach
//     `exit` or `return`. Accepted false negatives: aliases, functions, conditionals, pipelines,
//     and shell expansion are outside this small lexical rule. A final bare `test`/`[` is also
//     accepted: with no later command/list for it to accidentally permit, it cannot fall through.
//     Heredoc payloads are masked, while command substitutions are re-entered because they execute
//     shell commands.
//
// NO CACHE. Measured: a bounded pool of 16 runs the whole corpus in ~0.4–2 s, inside a target that
// already runs a ~200-file `node --check` sweep. A stamp key that omits the checker's own hash
// silently runs the OLD extractor after you edit it, and `lint-all` reusing a stamp makes the
// exhaustive target non-exhaustive. If the cost ever does matter, scripts/lint-scripts.mjs already
// implements the stamp pattern and can be mirrored then.
//
// Node builtins only; no new dependency.

import { execFile, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

export const SKILL_ROOTS = ['services/boss/internal/skillinstall/skills', '.claude/skills']

export const SHELL_INFO_STRINGS = new Set(['bash', 'sh', 'shell', 'zsh'])

const CONCURRENCY = 16

// See (d). Matched against every line before any fence decision is made.
const CONTAINER_PREFIX = /^[ \t]*(?:>[ \t]{0,4})*[ \t]*/

const FENCE_OPEN = /^(`{3,}|~{3,})(.*)$/
const FENCE_CLOSE = /^(`{3,}|~{3,})[ \t]*$/

// See (a).
const PLACEHOLDER = /(?<!<)<(?![<\s(])[^<>\n]*>/g

// A line ending in an ODD number of backslashes continues onto the next one.
const LINE_CONTINUES = /(^|[^\\])(\\\\)*\\$/

// Words that may precede the real command word inside a segment; skipping them widens the glob
// rule's reach (`then find …`) and cannot introduce a false positive. The keywords that OPEN a
// construct belong here as much as the ones that continue it: a removal used directly as a
// condition (`if rm -f a*.log b*.tmp; then …`) puts `if` at the head of the segment.
const LEADING_WORDS = new Set([
  'if',
  'while',
  'until',
  'then',
  'do',
  'else',
  'elif',
  '!',
  'time',
  'sudo',
  'command',
  'builtin',
  'exec',
  'env',
  // Braced-group syntax, never a command word: `{ rm -f a*.log b*.tmp; }` would otherwise resolve
  // its command to `{` and never look at the removal.
  '{',
  '}',
])

// The subset of the above that INVOKES another command and takes its own options. The shell expands
// pathnames before any of them runs, so `command rm -f a*.log b*.tmp` carries exactly the abort
// hazard a direct `rm` does; the options between wrapper and command must be stepped over to reach
// the effective command word. (`env`'s leading `NAME=value` arguments are already skipped by the
// assignment test below.)
const WRAPPER_WORDS = new Set(['sudo', 'command', 'builtin', 'exec', 'env'])

function stripContainerPrefix(line) {
  return line.slice(line.match(CONTAINER_PREFIX)[0].length)
}

// See (d): a closer must sit in the OPENER's container. Compared on blockquote depth rather than on
// the prefix bytes, so `>  ```bash ` … `>``` ` (same quote, different padding) still closes while a
// depth-0 bare fence cannot close a depth-1 one.
function blockquoteDepth(prefix) {
  return (prefix.match(/>/g) || []).length
}

// Columns of indentation a prefix carries AFTER its last blockquote marker, tabs advanced to the
// next 4-column stop.
//
// Both fences of a block are bounded to `[container, container + 3]`, where `container` is the
// content column of the enclosing list item (0 at top level) — CommonMark measures a fence's three
// allowed columns from its container, not from top level and not from the other fence. Each side of
// that window catches a distinct document-swallowing miss:
//   ABOVE it, the line is an INDENTED CODE BLOCK, so the fence is literal text being shown; an
//   opener there is not a fence at all, and a closer there does not close one.
//   BELOW it, the line has dedented out of the container, which exits the list and reads the fence
//   as a NEW top-level opener rather than a closer.
// Measuring against the container rather than against the opener is what keeps the shallow case
// honest: under `- item` (content column 2) a fence at column 2 with an opener-relative tolerance
// would admit a column-0 closer, and that line leaves the list.
// `start` is the column the text begins at, so a tab lands on the right stop when the run being
// measured does not itself start at column 0 (list-marker padding). The return is always the WIDTH
// of `text`, not an absolute column.
function columnWidth(text, start = 0) {
  let width = start
  for (const ch of text) width += ch === '\t' ? 4 - (width % 4) : 1
  return width - start
}

function fenceIndent(prefix) {
  return columnWidth(prefix.slice(prefix.lastIndexOf('>') + 1))
}

// Columns before the FIRST container marker. The marker's own position is subject to the same
// window as the fence: `    > ```bash ` puts the `>` four columns deep, which makes the whole line
// indented code — a blockquote being SHOWN, not one being opened. Measuring only after the last
// marker sees the single trailing space and extracts the sample inside it.
function leadingIndent(prefix) {
  return columnWidth(prefix.match(/^[ \t]*/)[0])
}

// A list marker, matched against a line with its container prefix already stripped. Its content
// column is what an enclosing list item indents its children to, and it is the ONLY thing that
// legitimises a fence opener indented four columns or more — see the note beside its use. The
// marker characters and the whitespace after them are captured separately because only the first
// four columns of that whitespace are ever padding; see `listMarkerText`.
const LIST_MARKER = /^([-*+]|\d{1,9}[.)])([ \t]+)/

// A line that starts a BLOCK of its own instead of continuing the paragraph above it: a fence, a
// list marker, an ATX heading, a thematic break. Matched against a line whose container prefix is
// already stripped. It answers two questions with one test — such a line can never itself be a lazy
// continuation, and it leaves no paragraph behind for the NEXT line to continue lazily. `---` is a
// thematic break here rather than a list marker because the marker alternatives require whitespace
// or end-of-line after the marker character.
const INTERRUPTS_PARAGRAPH =
  /^(?:`{3,}|~{3,}|#{1,6}(?:[ \t]|$)|(?:[-*+]|\d{1,9}[.)])(?:[ \t]|$)|(?:\*[ \t]*){3,}$|(?:-[ \t]*){3,}$|(?:_[ \t]*){3,}$)/

// The part of a list marker match that is actually PADDING, as text to consume.
//
// CommonMark gives an item's content column as the marker width plus 1–4 columns of following
// whitespace. Five or more columns means the item's content begins with an INDENTED CODE BLOCK: only
// ONE column is padding and the remaining four-plus are code. Consuming all of it instead would put
// the content column right under the apparent fence, so ``-     ```bash `` — which CommonMark shows
// as literal text — is extracted as a shell sample. That is the same document-swallowing false
// positive an over-indented top-level fence would be, and it fails the gate on a deliberately
// malformed sample the Markdown never presents as shell.
function listMarkerText(marker, startColumn) {
  const padding = columnWidth(marker[2], startColumn + marker[1].length)
  return padding > 4 ? `${marker[1]} ` : marker[0]
}

// Strip the OPENING fence's prefix from a body line, falling back to that line's own container
// prefix when it does not carry the opener's (blank lines, `>`-only lines inside a block quote).
function dedentBodyLine(line, prefix) {
  if (prefix === '') return line
  if (line.startsWith(prefix)) return line.slice(prefix.length)
  return stripContainerPrefix(line)
}

function normalizeInfo(info) {
  return info
    .trim()
    .split(/[\s,{]/)[0]
    .toLowerCase()
}

/**
 * Extract every fenced block, container-aware. Returns
 * [{ info, startLine, prefix, body, terminated }] where `startLine` is the 1-based line of the
 * OPENING fence and `body` is already stripped of the opener's container prefix.
 */
export function extractFencedBlocks(markdown) {
  // Split on either ending. `.gitattributes` sets no `text`/`eol` normalization here, so a
  // `core.autocrlf=true` checkout yields CRLF files; a trailing `\r` fails FENCE_CLOSE's `[ \t]*$`
  // and every block in the tree would read as unterminated on line endings alone.
  const lines = markdown.split(/\r?\n/)
  const blocks = []
  let open = null
  // Currently open list items as `{ content, quote }`, outermost first: the item's content column
  // and the blockquote depth it was opened at. A stack rather than a single column so a line that
  // dedents out of a nested item falls back to the ENCLOSING item's column instead of collapsing to
  // top level, which would reject the fences nested under it. The blockquote depth is carried
  // because indentation alone cannot retire an item: a list inside a blockquote (`> 1. item`) leaves
  // its content column behind once the quote ends, and a later TOP-LEVEL four-space-indented fence
  // then looks enclosed and is extracted — though CommonMark makes it literal indented code. That is
  // a false positive, so a deliberately malformed shell sample in a doc would fail the gate.
  const listStack = []
  const listContentColumn = () =>
    listStack.length > 0 ? listStack[listStack.length - 1].content : 0
  // Whether the previous line left a PARAGRAPH open inside the innermost list item. CommonMark lets
  // such a paragraph be continued by a line indented less than the item requires — a lazy
  // continuation — and that line does NOT end the item. Treating every dedent as an exit retires an
  // item that is still open, and a following fence indented past the (now lost) content column reads
  // as top-level indented code and is skipped entirely, letting malformed shell bypass the gate.
  let paragraphOpen = false

  const close = () => {
    blocks.push({
      info: open.info,
      startLine: open.startLine,
      prefix: open.prefix,
      body: open.body.join('\n'),
      terminated: open.terminated,
    })
    open = null
  }

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]
    const rest = stripContainerPrefix(line)

    if (open) {
      const closer = rest.match(FENCE_CLOSE)
      const closerPrefix = line.match(CONTAINER_PREFIX)[0]
      if (
        closer &&
        closer[1][0] === open.char &&
        closer[1].length >= open.length &&
        blockquoteDepth(closerPrefix) === open.depth &&
        fenceIndent(closerPrefix) >= open.container &&
        fenceIndent(closerPrefix) <= open.container + 3 &&
        leadingIndent(closerPrefix) <= open.container + 3
      ) {
        open.terminated = true
        close()
        continue
      }
      // The geometry above sees only depth and columns, so it cannot tell this block's closer from
      // a LATER fence that merely sits at the same numbers after the container ended. A body line
      // that has left the opener's container ends the block then and there: CommonMark closes a
      // fenced code block at the end of its containing block, and a fence body is never a paragraph,
      // so no lazy continuation can hold the container open across it. Without this, `- ```bash `,
      // an indented body, a column-0 `outside` and a two-space ``` come back as ONE terminated block
      // — swallowing the prose into the shell body and hiding that the trailing fence is itself
      // unterminated. Closing as TERMINATED is deliberate: an implicit close at container end is
      // valid CommonMark, and the genuinely broken fence is the one reported when the line is
      // re-read below. Blank lines never leave a container, and a DEEPER quote is still inside this
      // one, so neither can split a block.
      const bodyDepth = blockquoteDepth(closerPrefix)
      if (
        rest !== '' &&
        (bodyDepth < open.depth ||
          (bodyDepth === open.depth && fenceIndent(closerPrefix) < open.container))
      ) {
        open.terminated = true
        close()
        i -= 1
        continue
      }
      open.body.push(dedentBodyLine(line, open.prefix))
      continue
    }

    const openerPrefix = line.match(CONTAINER_PREFIX)[0]
    const openerIndent = fenceIndent(openerPrefix)

    // Track the innermost list item's content column. A blank line does not close a list; a
    // non-blank line indented less than an item's content column has left that item.
    const openerQuoteDepth = (openerPrefix.match(/>/g) || []).length
    if (rest !== '') {
      // List state belongs to the blockquote context it was opened in, so ANY change of depth
      // retires it — not just leaving one. Popping only DEEPER entries leaves a top-level item's
      // column alive when a new blockquote is entered, and the indented sample inside that quote is
      // then extracted as a fence; CommonMark makes it literal code, so a malformed shell example
      // shown for documentation would fail the gate. Same false positive, adjacent direction.
      while (listStack.length > 0 && listStack[listStack.length - 1].quote !== openerQuoteDepth) {
        listStack.pop()
      }
      // A dedented line only exits the item when it is not a LAZY CONTINUATION of the paragraph
      // above it. It has to be a paragraph continuation to be lazy, so anything that starts a block
      // of its own — a fence, a list marker, a heading, a thematic break — still exits.
      const lazy = paragraphOpen && !INTERRUPTS_PARAGRAPH.test(rest)
      if (!lazy) {
        while (listStack.length > 0 && openerIndent < listContentColumn()) listStack.pop()
      }
    }
    // A marker only opens a container when the marker ITSELF is within the current window. At
    // `    - sample` with nothing enclosing it, the line is indented code — a list being shown —
    // and treating it as real would legitimise the deeper fence beneath it.
    const enclosingContent = listContentColumn()
    const markerMatch = openerIndent <= enclosingContent + 3 ? rest.match(LIST_MARKER) : null
    const marker = markerMatch ? listMarkerText(markerMatch, openerIndent) : null
    if (marker) {
      listStack.push({
        content: openerIndent + columnWidth(marker, openerIndent),
        quote: openerQuoteDepth,
      })
    }
    const listContent = listContentColumn()

    // `- ```bash ` is the standard same-line form: the fence opens in the item's CONTENT, so it must
    // be matched after the marker is consumed. Matching the unconsumed line misses the opener
    // outright, and the properly indented closer then reads as a new unterminated opener — failing
    // a valid, correctly closed block. The body dedents against the item's content column, which is
    // where its lines actually sit.
    const content = marker ? rest.slice(marker.length) : rest
    const fencePrefix = marker ? ' '.repeat(listContent) : openerPrefix

    // Record what this line leaves behind for the next one. Measured on the item's CONTENT, so
    // `- item` leaves a paragraph open (the item's own text) while `- ```bash ` does not — the fence
    // alternatives above also cover every `continue` below, so a line that merely looks like a fence
    // never leaves one open either.
    paragraphOpen = content !== '' && !INTERRUPTS_PARAGRAPH.test(content)

    const opener = content.match(FENCE_OPEN)
    if (!opener) continue
    const info = opener[2].trim()
    // A backtick info string may not contain a backtick — that is not a fence opener.
    if (opener[1][0] === '`' && info.includes('`')) continue
    // CommonMark measures a fence's three columns of allowed indentation from its CONTAINER's
    // content column, not from top level and not from the fence itself. Past that an unfenced line
    // is an INDENTED CODE BLOCK, so ` ```bash ` there is literal text being shown — often
    // deliberately-malformed sample shell — and extracting it would reject valid Markdown. The
    // container marker's own column is bounded the same way; see `leadingIndent`.
    if (!marker && openerIndent > listContent + 3) continue
    if (leadingIndent(openerPrefix) > listContent + 3) continue
    open = {
      char: opener[1][0],
      length: opener[1].length,
      info: normalizeInfo(info),
      startLine: i + 1,
      prefix: fencePrefix,
      depth: blockquoteDepth(openerPrefix),
      // The container the closer must stay inside. Carrying the CONTAINER column rather than the
      // opener's own indent is what keeps the shallow case honest: under `- item` (content column
      // 2) a fence at 2 with an opener-relative tolerance would accept a column-0 closer, but that
      // line exits the list rather than closing the fence.
      container: listContent,
      body: [],
      terminated: false,
    }
  }

  if (open) close()
  return blocks
}

/** See (a): rewrite `<placeholder>` words, leave `<<`, `<<<`, `< file` and `<(…)` alone. */
export function normalizePlaceholders(text) {
  return text.replace(PLACEHOLDER, 'PLACEHOLDER')
}

// Heredoc OPERATORS on a line, in order, ignoring any `<<` the shell would never read as one. A
// regex over the raw line cannot do this: `echo '<<EOF'` and `# example: <<EOF` would each open a
// bogus heredoc that blanks the real commands below it, silently dropping their findings. So the
// scan tracks quoting, backslash escapes and comments the way the tokenizer does. `<<<` is a
// herestring, not a heredoc, and is stepped over whole.
const ANSI_C_NAMED = {
  a: '\x07',
  b: '\b',
  e: '\x1b',
  E: '\x1b',
  f: '\f',
  n: '\n',
  r: '\r',
  t: '\t',
  v: '\v',
  '\\': '\\',
  "'": "'",
  '"': '"',
  '?': '?',
}

// Decode the body of a `$'…'` word. bash applies these escapes before quote removal, so `<<$'E\x4fF'`
// terminates on `EOF`; keeping the raw text records a terminator that never arrives.
//
// A sequence bash does NOT recognize keeps its backslash: `$'\x'` with no hex digit after it is the
// two characters `\x`, and so are `$'\z'` and `$'\9'` (bash 5.3.9, verified against `printf`).
// Dropping the backslash records a delimiter one character short of the real one, which reports a
// heredoc bash terminates happily as unterminated AND accepts one that bash leaves open — a false
// positive and a false negative from the same line.
function decodeAnsiC(text) {
  let out = ''
  for (let i = 0; i < text.length; i++) {
    if (text[i] !== '\\' || i + 1 >= text.length) {
      out += text[i]
      continue
    }
    const c = text[++i]
    if (c === 'x' || c === 'u' || c === 'U') {
      const width = c === 'x' ? 2 : c === 'u' ? 4 : 8
      const hex = new RegExp(`^[0-9a-fA-F]{1,${width}}`).exec(text.slice(i + 1))
      if (hex) {
        out += String.fromCodePoint(Number.parseInt(hex[0], 16))
        i += hex[0].length
        continue
      }
      out += `\\${c}`
      continue
    }
    if (c >= '0' && c <= '7') {
      const octal = /^[0-7]{0,2}/.exec(text.slice(i + 1))[0]
      out += String.fromCharCode(Number.parseInt(c + octal, 8))
      i += octal.length
      continue
    }
    out += ANSI_C_NAMED[c] ?? `\\${c}`
  }
  return out
}

// Consume an arithmetic region, carrying its nesting in `state.arith` so an expansion split by a
// newline — `echo $((1 +` / `2 << 3))` — stays arithmetic on the following line instead of letting
// its `<<` read as a heredoc operator. Returns the index of the closing delimiter, or the line
// length when the region is still open.
function skipArithmetic(line, start, state) {
  const arith = state.arith
  for (let i = start; i < line.length; i++) {
    const c = line[i]
    // Quoting is live inside arithmetic — `$(( $(echo '))') + 1 << 2 ))` is valid — so counting a
    // quoted delimiter would end the region early and expose the shift after it as a heredoc.
    if (arith.quote) {
      if (arith.quote === '"' && c === '\\' && i + 1 < line.length) i++
      else if (c === arith.quote) arith.quote = null
      continue
    }
    if (c === '\\' && i + 1 < line.length) {
      i++
      continue
    }
    if (c === "'" || c === '"') {
      arith.quote = c
      continue
    }
    if (c === arith.open) arith.depth += 1
    else if (c === arith.close && --arith.depth === 0) {
      state.arith = null
      return i
    }
  }
  return line.length
}

// `joins` holds the indices in `line` at which a physical newline was elided by a backslash
// continuation, so the delimiter parser can put back the pair bash itself keeps. See the single-quote
// branch below; everywhere else bash really does remove it, and an absent set means "no joins".
function heredocDelimiters(line, state, joins = null) {
  const found = []
  const joinedAt = (at) => joins != null && joins.has(at)
  const enclosing = state.enclosing
  let quote = state.quote
  // Resume an arithmetic region the previous line left open; it consumes the rest of the line whole.
  // A parameter expansion needs no resume hook: its frame simply stays on `enclosing`, which already
  // persists across lines.
  let resumeAt = 0
  if (state.arith) resumeAt = skipArithmetic(line, 0, state) + 1

  // Index of a character consumed as a backslash escape. `#` opens a comment only at a word start,
  // and in `cat foo\ # <<EOF` the escaped space stays INSIDE the word, so the `#` continues that
  // word and the `<<EOF` after it is a real operator. Testing the raw preceding character alone
  // reads that space as a separator, mistakes the `#` for a comment, and misses the unterminated
  // heredoc — and since `bash -n` only warns there, the gate would pass while the rest of the block
  // is swallowed as heredoc payload.
  let escapedAt = -1
  for (let i = resumeAt; i < line.length; i++) {
    const ch = line[i]
    // Innermost open region. Inside a `${…}` WORD a `<<`, a `#`, and an unmatched `(`/`)` are all
    // ordinary text rather than syntax; a command substitution nested inside one pushes its own
    // frame, which restores every one of them.
    const inParam = enclosing[enclosing.length - 1]?.param === true

    if (quote === "'") {
      if (ch === "'") quote = null
      continue
    }
    // `$(` resumes ordinary shell parsing even INSIDE double quotes, so the heredoc in the tree's
    // own `git commit -m "$(cat <<'EOF'` form is a real operator. Treating the opening `"` as
    // covering the rest of the line hides it and hands the commit message to the glob rule as
    // commands — a false positive on the corpus's most common heredoc shape.
    // Arithmetic, where `<<` is a left SHIFT rather than a heredoc. Stepped over whole so its
    // operand cannot be read as a delimiter that blanks the rest of the block. A bare `((…))` is the
    // same construct in command position, and `$[…]` is bash's deprecated-but-still-valid form.
    //
    // `$((` and `$[` are expansions and stay live inside double quotes and inside a `${…}` word. The
    // BARE `((` is a COMMAND, so it exists only where a command can start: in `echo "(( example"` it
    // is literal text. Opening a region there consumes the quote as arithmetic quoting, so the region
    // never closes and every following line resumes inside it — the `cat <<EOF` after it is skipped,
    // `findUnterminatedHeredoc` returns null, and since `bash -n` only WARNS on an unterminated
    // heredoc (exit 0, measured below) both checks accept a block whose remaining commands are
    // swallowed as payload. Single quotes are already handled by the branch above.
    if (
      (ch === '$' && line[i + 1] === '(' && line[i + 2] === '(') ||
      (ch === '(' && line[i + 1] === '(' && quote === null && !inParam)
    ) {
      state.arith = { open: '(', close: ')', depth: 0, quote: null }
      i = skipArithmetic(line, ch === '$' ? i + 1 : i, state)
      continue
    }
    if (ch === '$' && line[i + 1] === '[') {
      state.arith = { open: '[', close: ']', depth: 0, quote: null }
      i = skipArithmetic(line, i + 1, state)
      continue
    }
    // `${…}` is PARAMETER expansion, not command parsing: a `<<` inside it is pattern text, as in
    // the valid `echo ${x#<<EOF}`. Read as an operator it records a delimiter of `EOF}` and reports
    // an unterminated heredoc on a block bash runs happily — a false positive.
    //
    // It is pushed as a FRAME rather than skipped as a region so the loop's own quoting, escaping and
    // substitution handling keeps running inside the word — bash balances the closing brace exactly
    // that way. Skipping the region wholesale got three cases wrong at once: `${x:-\}<<EOF}` closed
    // on the ESCAPED brace bash treats as literal (a false positive), `${x:-$(cat <<EOF)}` swallowed
    // a command substitution whose `<<` bash really does read as an operator (a false negative), and
    // a bare `{` in the word — `${x:-a{b}<<EOF` — counted as nesting bash does not count, leaving
    // the scanner stuck inside the expansion (another false negative). Only `${` opens a level.
    if (ch === '$' && line[i + 1] === '{') {
      enclosing.push({ quote, depth: 0, param: true })
      quote = null
      i++
      continue
    }
    if (ch === '$' && line[i + 1] === '(') {
      enclosing.push({ quote, depth: 0 })
      quote = null
      i++
      continue
    }
    // The legacy backtick substitution resumes command parsing the same way `$(` does, including
    // inside double quotes. It does not nest, so a backtick toggles: the second one restores the
    // quoting that was in force when the first opened it.
    if (ch === '`') {
      const top = enclosing[enclosing.length - 1]
      if (top?.backtick) quote = enclosing.pop().quote
      else {
        enclosing.push({ quote, depth: 0, backtick: true })
        quote = null
      }
      continue
    }
    if (quote === '"') {
      if (ch === '\\' && i + 1 < line.length) i++
      else if (ch === '"') quote = null
      continue
    }
    if (ch === '\\') {
      escapedAt = i + 1
      i++
      continue
    }
    if (ch === "'" || ch === '"') {
      quote = ch
      continue
    }
    // The brace closing a parameter expansion. Reached only past the quote and escape branches
    // above, so the literal `}` in `${x:-"}"}` and in `${x:-\}<<EOF}` cannot close it — bash keeps
    // both expansions open there, and closing early exposes the following `<<` as a bogus operator.
    // A `}` outside an expansion is ordinary text: one ending a brace group or a function body must
    // never pop a command substitution's frame.
    if (ch === '}' && inParam) {
      quote = enclosing.pop().quote
      continue
    }
    // A substitution ends at the `)` matching ITS `(`, not at the first one seen. An ordinary
    // subshell nested inside — `"$( (true); echo "<<EOF" )"` — otherwise restores the outer quote
    // early, and the quoted `<<EOF` after it reads as a real operator. Parentheses inside a
    // parameter expansion are text, so a frame for one never counts them.
    if (enclosing.length > 0 && !inParam && (ch === '(' || ch === ')')) {
      const inner = enclosing[enclosing.length - 1]
      if (ch === '(') inner.depth += 1
      else if (inner.depth > 0) inner.depth -= 1
      else quote = enclosing.pop().quote
      continue
    }
    // `#` opens a comment only at the start of a word — and an ESCAPED separator does not start one.
    // Inside a parameter expansion it is an operator or plain text (`${#x}`, `${x#pat}`, `${x:-a #b}`),
    // never a comment, so a word-initial one there must not blank the rest of the line.
    if (
      !inParam &&
      ch === '#' &&
      (i === 0 || (/[\s;&|(]/.test(line[i - 1]) && i - 1 !== escapedAt))
    )
      break
    if (ch !== '<' || line[i + 1] !== '<') continue
    // A `<<` in an expansion word is text: `echo ${x#<<EOF}` is a valid one-line command.
    if (inParam) continue
    if (line[i + 2] === '<') {
      i += 2
      continue
    }

    // How deeply nested the OPERATOR is, so the scanner can tell which newline ends the command it
    // belongs to. Stable across the delimiter word below, which consumes substitution syntax as
    // literal text without opening a frame.
    const opDepth = enclosing.length

    i += 2
    const indented = line[i] === '-'
    if (indented) i++
    while (line[i] === ' ' || line[i] === '\t') i++

    // The delimiter is one WORD after quote removal, and its pieces may be quoted independently:
    // bash terminates `<<'E'OF` on `EOF`. Stopping at the first quoted segment yields `E`, a
    // terminator that never arrives, which blanks every later command in the block. Quoting only
    // affects expansion inside the body, never the delimiter's text.
    const delimStart = i
    let delim = ''
    // Any quoting or escaping anywhere in the word makes the delimiter QUOTED, which in turn makes
    // the heredoc body literal. An unquoted one is expanded, and bash removes backslash-newline in
    // that body — so its terminator may itself be split across physical lines.
    let quotedDelim = false
    // Set when the delimiter WORD itself spans a physical newline — either because a quoted segment
    // is still open at end of line, or because a backslash-newline fell inside single quotes, where
    // bash keeps both characters. Either way bash's delimiter contains a newline, and a heredoc
    // terminator is compared one whole line at a time, so NO body line can ever equal it: the
    // heredoc runs to end of file. bash only WARNS about that and exits 0, so nothing else reports it.
    let spansNewline = false
    while (i < line.length && !/[\s;&|<>()]/.test(line[i])) {
      const c = line[i]
      // `$'…'` applies ANSI-C escape decoding before quote removal, so it is consumed whole rather
      // than handed to the plain-quote branch below. `$"…"` is locale translation, whose text is
      // just its content, so only the `$` is dropped.
      if (c === '$' && line[i + 1] === "'") {
        quotedDelim = true
        i += 2
        let encoded = ''
        // Measured on bash 5.3.9: `<<$'EO\` / `F'` warns `wanted \`EO\` + newline + `F'`, so ANSI-C
        // quoting keeps a backslash-newline exactly as a plain single-quoted one does. The pair is
        // put back around a decode of each side so the reported delimiter is bash's own.
        let decoded = ''
        const keepJoin = () => {
          decoded += `${decodeAnsiC(encoded)}\\\n`
          encoded = ''
          spansNewline = true
        }
        while (i < line.length && line[i] !== "'") {
          if (joinedAt(i)) keepJoin()
          if (line[i] === '\\' && i + 1 < line.length) encoded += line[i++]
          encoded += line[i++]
        }
        if (joinedAt(i)) keepJoin()
        if (i >= line.length) spansNewline = true
        i++ // past the closing quote
        delim += decoded + decodeAnsiC(encoded)
        continue
      }
      if (c === '$' && line[i + 1] === '"') {
        quotedDelim = true
        i++
        continue
      }
      if (c === "'" || c === '"') {
        quotedDelim = true
        i++
        while (i < line.length && line[i] !== c) {
          // A backslash-newline is removed everywhere bash reads it EXCEPT inside single quotes,
          // which are literal to the last byte. So `cat <<'EO\` / `F'` does NOT open on `EOF`: the
          // delimiter is `EO`, backslash, newline, `F`. Reading the pair as a line continuation
          // instead lets a later `EOF` appear to close a heredoc bash keeps open, and since bash
          // only warns there the whole rest of the block is accepted as swallowed payload. The
          // elided pair is put back so the reported delimiter is the one bash actually wanted.
          if (c === "'" && joinedAt(i)) {
            delim += '\\\n'
            spansNewline = true
          }
          // Inside DOUBLE quotes bash honours a backslash before `$`, backtick, `"` or `\`, so
          // `<<"E\"OF"` terminates on `E"OF`. Single quotes are literal, backslash included.
          if (c === '"' && line[i] === '\\' && /["$`\\]/.test(line[i + 1] ?? '')) i++
          delim += line[i++]
        }
        // A continuation elided immediately before the CLOSING quote is inside the segment too.
        if (c === "'" && joinedAt(i)) {
          delim += '\\\n'
          spansNewline = true
        }
        // The segment never closed, so bash reads on past the newline: `cat <<'EO` / `F'` wants the
        // two-line delimiter `EO` + newline + `F`, which no single body line can equal. True of
        // double quotes as well — there the newline is literal even though a backslash before one
        // is not.
        if (i >= line.length) spansNewline = true
        i++ // step past the closing quote
        continue
      }
      // A delimiter is not expanded, so substitution SYNTAX inside it is literal text that still
      // belongs to the word: bash terminates `<<$(echo)` on the line `$(echo)`. Stopping at the
      // opening bracket would record `$` and wait for a terminator that never comes.
      if (c === '$' && (line[i + 1] === '(' || line[i + 1] === '[')) {
        const open = line[i + 1]
        const close = open === '(' ? ')' : ']'
        let depth = 0
        // Balance QUOTE-aware: in the valid `cat <<$(printf ')')` the quoted `)` is literal text,
        // not the closer. Counting it ends the word early, records a delimiter of `$(printf '))`,
        // and rejects a block `bash -n` accepts — a false positive.
        let subQuote = null
        delim += line[i++] // the `$`
        for (; i < line.length; i++) {
          const c2 = line[i]
          delim += c2
          if (subQuote) {
            if (c2 === '\\' && subQuote === '"' && i + 1 < line.length) delim += line[++i]
            else if (c2 === subQuote) subQuote = null
            continue
          }
          // Outside quotes a backslash escapes the next character, so the first `)` of the valid
          // `cat <<$(printf \))` is literal and the SECOND one closes. Measured on bash 5.3.9: that
          // block terminates on a `$(printf \))` line, and an unterminated one warns `wanted
          // `$(printf \))'` — backslash included. Reading the escaped `)` as the closer records
          // `$(printf \)` and reports an unterminated heredoc on a block bash accepts: a false
          // positive. The escape does NOT quote the delimiter — the body of that heredoc still
          // expands `$X` — so `quotedDelim` is deliberately left alone.
          if (c2 === '\\' && i + 1 < line.length) {
            delim += line[++i]
            continue
          }
          if (c2 === "'" || c2 === '"') {
            subQuote = c2
            continue
          }
          if (c2 === open) depth += 1
          else if (c2 === close && --depth === 0) break
        }
        i++
        continue
      }
      // A backtick pair is literal text in a delimiter word — bash terminates ``cat <<`echo x` `` on
      // the line ``​`echo x`​`` — UNLESS one is already open around it, in which case this backtick
      // is its closer and the word ends here: ``echo `cat <<EOF` `` waits for `EOF`, not ``EOF` ``.
      if (c === '`') {
        if (enclosing[enclosing.length - 1]?.backtick) break
        delim += line[i++]
        while (i < line.length && line[i] !== '`') delim += line[i++]
        if (i < line.length) delim += line[i++]
        continue
      }
      if (c === '\\') {
        quotedDelim = true
        i++
      }
      if (i < line.length) delim += line[i++]
    }
    // Key on whether a delimiter WORD was present, not on whether it is non-empty: quote removal
    // makes `cat <<''` a real operator with an empty delimiter, terminated by a blank line. Dropping
    // it on falsiness loses the one heredoc bash itself only warns about.
    const hadDelimiter = i > delimStart
    i-- // the terminating character is not ours; let the outer loop re-read it
    if (hadDelimiter)
      found.push({
        delim,
        indented,
        quoted: quotedDelim,
        unterminable: spansNewline,
        depth: opDepth,
      })
  }
  state.quote = quote
  return found
}

// Blank out heredoc BODIES before the glob rule walks the block. The skill tree builds PR bodies and
// prompts with heredocs, and a payload line such as `rm -f build/*.log tmp/*.tmp` is literal text
// the shell never expands — analyzing it as a command is a false positive on a valid block. Lines
// are replaced in place rather than removed so `lineOffset` still indexes the original body. Erring
// toward a body that never terminates only silences the rule (a false negative); `bash -n` remains
// the authority on whether the heredoc itself is well-formed.
function scanHeredocs(lines) {
  const masked = []
  let queue = []
  let active = null
  let openedAt = 0
  // First physical line of the command that queued the operators still waiting in `queue`. Distinct
  // from `pendingAt`, which only spans a backslash-continued logical line: a command held open by a
  // quote may queue on one line and activate several lines later, and a reader must be sent to the
  // `<<` itself.
  let queueAt = 0
  // Lexical state persists ACROSS command lines: a quoted string may span newlines, and bash keeps
  // a `<<EOF` on its second line quoted. Re-starting per line reads that as an operator, which now
  // costs twice — a bogus `heredoc` finding on a valid block, and the real commands after it masked
  // away. Heredoc BODY lines are literal text, not shell, so they never advance this state.
  const state = { quote: null, enclosing: [] }
  // A delimiter WORD may cross an escaped newline — bash removes the backslash-newline before
  // reading it, so `cat <<EO\` / `F` opens on `EOF`. Delimiters are therefore parsed from the whole
  // logical line; the heredoc body still begins after the last physical line of it.
  let pending = ''
  let pendingAt = 0
  // Offsets into the joined logical line at which a physical newline was elided. bash keeps the
  // backslash-newline INSIDE single quotes, so the delimiter parser needs to know where the joins
  // happened to put those back; everywhere else the removal above is what bash does.
  let joins = new Set()
  // Continued BODY lines held while looking for an unquoted heredoc's terminator. Reset whenever a
  // heredoc closes, so payload from one never leaks into the next one's comparison.
  let bodyPending = ''
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]
    if (active) {
      masked.push('')
      // `<<-` strips TABS only. Accepting spaces too would end a heredoc bash keeps open and hand
      // its payload to the glob rule as commands — a false POSITIVE on a valid block.
      const candidate = active.indented ? line.replace(/^\t+/, '') : line
      // An UNQUOTED delimiter leaves the body expanded, and bash removes backslash-newline there as
      // it does anywhere else — so a terminator written across `EO\` and `F` really does close the
      // heredoc. Comparing each physical line alone reports that valid block as unterminated: a
      // false positive. A QUOTED delimiter makes the body literal, where those are two payload
      // lines and joining them would end a heredoc bash keeps open.
      if (!active.quoted && LINE_CONTINUES.test(candidate)) {
        bodyPending += candidate.replace(/\\$/, '')
        continue
      }
      const joinedCandidate = bodyPending + candidate
      bodyPending = ''
      // A delimiter word containing a newline (an unclosed quote, or a backslash-newline kept inside
      // single quotes) can never equal a single body line, so bash reads to end of file. Comparing
      // anyway would close a heredoc bash keeps open and hand its payload to the glob rule.
      if (!active.unterminable && joinedCandidate === active.delim) {
        active = queue.shift() ?? null
        if (active) openedAt = i
      }
      continue
    }
    masked.push(line)
    if (pending === '') {
      pendingAt = i
      joins = new Set()
    }
    if (LINE_CONTINUES.test(line)) {
      pending += line.replace(/\\$/, '')
      joins.add(pending.length)
      continue
    }
    if (queue.length === 0) queueAt = pendingAt
    queue.push(...heredocDelimiters(pending + line, state, joins))
    pending = ''
    // bash queues an operator where it PARSES it, but the body only starts after the newline that
    // ends the COMMAND the operator belongs to — and an unclosed quote or substitution carries that
    // command past the newline. Measured on bash 5.3.9, `cat <<EOF "` / `EOF` / `"` / payload warns
    // `here-document at line 3 delimited by end-of-file (wanted `EOF')`: the `EOF` on line 2 is text
    // inside the multiline quoted argument, and the body only begins after line 3. Starting it on
    // line 2 instead used that text as the terminator, so the heredoc looked closed and every
    // command after it was swallowed as payload and never checked — the exact failure this scanner
    // exists to catch, and one `bash -n` only warns about (exit 0).
    //
    // The test is the operator's OWN nesting depth, not merely "something is open": a substitution
    // is a nested command list whose newlines end ITS commands. Measured on the same bash,
    // `echo ${x:-$(cat <<EOF` / `hi` / `EOF` / `)}` runs clean with `hi` as the body — the operator
    // sits inside the substitution, so line 1's newline does end its command — while
    // `cat <<EOF $(echo` / `x)` / body warns `here-document at line 2`, one line later, because that
    // operator is outside. Gating on any open frame would have delayed the first case too and read
    // its body as commands. Holding the queue also lets a second operator parsed on a later physical
    // line of the same command queue behind the first, in bash's order.
    const head = queue[0]
    if (head && (state.quote || state.enclosing.length > head.depth)) continue
    active = queue.shift() ?? null
    if (active) openedAt = queueAt
  }
  // A trailing escaped newline leaves the last logical line unparsed in `pending`. bash still removes
  // the backslash-newline and reads the operator on it, so a block ending in `cat <<EO\` opens a
  // heredoc that end-of-input never terminates — and it only WARNS, exiting 0, so returning here lets
  // both checks accept the malformed block. (`active` is necessarily null: while a heredoc is open
  // every line is body, so nothing can be accumulating in `pending`.)
  if (pending !== '') {
    if (queue.length === 0) queueAt = pendingAt
    queue.push(...heredocDelimiters(pending, state, joins))
    active = queue.shift() ?? null
    if (active) openedAt = queueAt
  }
  return { masked, active, openedAt }
}

function maskHeredocBodies(lines) {
  return scanHeredocs(lines).masked
}

/**
 * The heredoc operator a block opens and never closes, as `{ delim, lineOffset }`, or null.
 *
 * This is the one defect in (e)'s remit that bash's EXIT STATUS cannot express. Measured on a body
 * of `cat <<EOF`: bash 5.3.9 prints `warning: here-document ... delimited by end-of-file` and exits
 * 0; bash 3.2.57 (macOS /bin/bash) prints nothing and exits 0. So neither the status nor stderr is a
 * portable signal, and the consequence is exactly this gate's founding failure — the rest of the
 * block is swallowed as heredoc payload and its commands are never run. The block's own scanner
 * already pairs every operator with its terminator, so it decides this identically on every bash.
 */
export function findUnterminatedHeredoc(body) {
  const { active, openedAt } = scanHeredocs(body.split('\n'))
  return active ? { delim: active.delim, lineOffset: openedAt } : null
}

// Join backslash line-continuations, remembering the 0-based offset of the FIRST physical line of
// each logical line. Load-bearing: a multi-glob `rm` in this tree continues onto the next line.
// Shares LINE_CONTINUES with the heredoc scan, which needs the same notion to read a delimiter word
// that crosses an escaped newline.
function joinContinuations(lines) {
  const joined = []
  let buffer = null
  let start = 0
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]
    if (buffer === null) {
      buffer = line
      start = i
    } else {
      buffer += ` ${line.replace(/^[ \t]+/, '')}`
    }
    if (LINE_CONTINUES.test(line)) {
      buffer = buffer.replace(/\\$/, '')
      continue
    }
    joined.push({ text: buffer, lineOffset: start })
    buffer = null
  }
  if (buffer !== null) joined.push({ text: buffer, lineOffset: start })
  return joined
}

// Track options in command-list order. A later `set +e` or `shopt -u` must take effect before
// the next command; scanning the whole source for an enable would apply both options backwards.
function shellOptionStateAfter(body, initial) {
  const result = { ...initial }
  const tokenState = {
    quote: null,
    substDepth: 0,
    substQuote: null,
    backtick: false,
    paramDepth: 0,
  }
  let subshellDepth = initial.subshellDepth ?? 0
  // An AND/OR operator at the end of a physical line still controls the next
  // line's command. Keep the predecessor's known status until that RHS has
  // been considered, or a skipped `set -e` can incorrectly leak into later
  // commands.
  let pendingSeparator = null
  let priorStatus = null
  for (const line of body.split('\n')) {
    const tokens = tokenizeShellLine(line, tokenState)
    let segment = []
    let priorSeparator = pendingSeparator
    const segments = []
    const commandListOperators = new Set([';', '&&', '||', '&'])
    for (const token of tokens) {
      if (token.operator && commandListOperators.has(token.raw)) {
        segments.push({ tokens: segment, separator: token, priorSeparator })
        segment = []
        priorSeparator = token
        continue
      }
      segment.push(token)
    }
    segments.push({ tokens: segment, separator: null, priorSeparator })
    for (const { tokens: segmentTokens, separator, priorSeparator } of segments) {
      const words = segmentTokens.filter((token) => !token.operator)
      const args = words.map((token) => removeQuotes(token.raw))
      const commandIndex = commandWordIndex(words)
      const command = args[commandIndex]
      const parenDelta = segmentTokens.reduce((delta, token) => {
        if (token.raw === '(') return delta + 1
        if (token.raw === ')') return delta - 1
        return delta
      }, 0)
      const commandToken = words[commandIndex]
      const commandTokenIndex = commandToken ? segmentTokens.indexOf(commandToken) : -1
      const opensSubshellBeforeCommand =
        commandTokenIndex !== -1 &&
        segmentTokens.slice(0, commandTokenIndex).some((token) => token.raw === '(')
      const inSubshell = subshellDepth > 0 || opensSubshellBeforeCommand
      const pipelines = segmentTokens.filter((token) => token.raw === '|')
      const inAsyncList = priorSeparator?.raw === '&' || separator?.raw === '&'
      // Commands on an AND/OR RHS normally are not guaranteed to execute, so their option changes
      // cannot establish state for a later command. Literal true/false predecessors are the
      // exception: their RHS is known to execute. Braced groups execute in this shell, but
      // parenthesized groups do not, including when their opening and closing parens span physical
      // lines.
      const executesUnconditionally =
        !inAsyncList &&
        (priorSeparator?.raw !== '&&' || priorStatus === true) &&
        (priorSeparator?.raw !== '||' || priorStatus === false)
      if (executesUnconditionally && !inSubshell && pipelines.length === 0 && command === 'set') {
        for (let i = commandIndex + 1; i < args.length; i++) {
          if (args[i] === '--') break
          if (/^-[A-Za-z]*e[A-Za-z]*$/.test(args[i])) result.errexit = true
          else if (/^\+[A-Za-z]*e[A-Za-z]*$/.test(args[i])) result.errexit = false
          else if (args[i] === '-o' && args[i + 1] === 'errexit') result.errexit = true
          else if (args[i] === '+o' && args[i + 1] === 'errexit') result.errexit = false
          else if (args[i] === '-o' && args[i + 1] === 'pipefail') result.pipefail = true
          else if (args[i] === '+o' && args[i + 1] === 'pipefail') result.pipefail = false
          else if (args[i] === '-o' && args[i + 1] === 'posix') result.inheritErrexit = true
          if (/^-[A-Za-z]*o[A-Za-z]*$/.test(args[i]) && args[i + 1] === 'pipefail')
            result.pipefail = true
          else if (/^\+[A-Za-z]*o[A-Za-z]*$/.test(args[i]) && args[i + 1] === 'pipefail')
            result.pipefail = false
          else if (/^-[A-Za-z]*o[A-Za-z]*$/.test(args[i]) && args[i + 1] === 'posix')
            result.inheritErrexit = true
        }
      } else if (
        executesUnconditionally &&
        !inSubshell &&
        pipelines.length === 0 &&
        command === 'shopt'
      ) {
        let shoptMode = null
        let shoptSetOptionMode = false
        let shoptOptionStart = commandIndex + 1
        for (; shoptOptionStart < args.length; shoptOptionStart++) {
          const arg = args[shoptOptionStart]
          if (arg === '--') {
            shoptOptionStart += 1
            break
          }
          if (!/^-[opqsu]+$/.test(arg)) break
          for (const flag of arg.slice(1)) {
            if (flag === 's') shoptMode = true
            if (flag === 'u') shoptMode = false
            if (flag === 'o') shoptSetOptionMode = true
          }
        }
        if (shoptMode !== null) {
          const shoptOptions = args.slice(shoptOptionStart)
          if (shoptOptions.includes('inherit_errexit')) result.inheritErrexit = shoptMode
          if (shoptSetOptionMode && shoptOptions.includes('errexit')) result.errexit = shoptMode
          if (shoptSetOptionMode && shoptOptions.includes('pipefail')) result.pipefail = shoptMode
        }
      }
      if (executesUnconditionally) {
        priorStatus =
          command === 'true' || command === ':' ? true : command === 'false' ? false : null
      }
      subshellDepth = Math.max(0, subshellDepth + parenDelta)
    }
    pendingSeparator =
      tokens.at(-1)?.operator && ['&&', '||'].includes(tokens.at(-1).raw) ? tokens.at(-1) : null
    if (!pendingSeparator) priorStatus = null
  }
  result.subshellDepth = subshellDepth
  return result
}

function branchReachesExit(branch) {
  let trimmed = branch.trimStart()
  while (trimmed.startsWith('#')) {
    const newline = trimmed.indexOf('\n')
    if (newline === -1) return false
    trimmed = trimmed.slice(newline + 1).trimStart()
  }
  // A direct `exit` still needs the operator check below: `exit 1 | cat` and
  // `exit 1 & wait` terminate only a pipeline/background child, not this shell.
  if (/^(?:exit|return)\b/.test(trimmed)) return branchContainsTerminator(trimmed)
  if (!trimmed.startsWith('{')) return false
  const end = matchingBrace(trimmed, 0)
  if (end === -1) return false
  // A braced group on the left of a pipeline or in the background runs in a child shell, so its
  // `exit`/`return` cannot stop the guarded block. Skip redirections while finding the first real
  // operator: `} 2>&1 | cat` is just as inert as `} | cat`.
  const suffix = tokenizeShellLine(trimmed.slice(end + 1))
  const firstOperator = suffix.find((token) => token.operator)
  if (firstOperator?.raw === '|' || firstOperator?.raw === '&') return false
  return branchContainsTerminator(trimmed.slice(1, end))
}

// The final command in the RHS of an OR list is subject to `errexit`. Keep this deliberately
// narrow: only a literal `false` command, or a group whose own final command has that shape, can
// prove that the fallback terminates without an explicit exit or return. Bash ignores `errexit`
// for commands whose status is inverted with `!`, so a broader known-failure predicate is unsafe.
function branchFailsUnderErrexit(branch) {
  const trimmed = branch.trim()
  if (trimmed.startsWith('{')) {
    const end = matchingBrace(trimmed, 0)
    if (end === -1 || trimmed.slice(end + 1).trim()) return false
    return branchFailsUnderErrexit(trimmed.slice(1, end))
  }
  if (trimmed.startsWith('(')) {
    const end = matchingParen(trimmed, 0)
    if (end === -1 || trimmed.slice(end + 1).trim()) return false
    return branchFailsUnderErrexit(trimmed.slice(1, end))
  }
  const command = trimmed.replace(/;\s*(?:#.*)?$/s, '').trim()
  const { separators, pipelines } = scanGuardOperators(command)
  return separators.length === 0 && pipelines.length === 0 && commandWord(command) === 'false'
}

// Tokenize each physical line instead of scanning raw text: a diagnostic may legitimately quote a
// line beginning with `exit`, but that is data for the fallback rather than a terminating command.
function branchContainsTerminator(body) {
  const state = { quote: null, substDepth: 0, substQuote: null, backtick: false, paramDepth: 0 }
  let parenDepth = 0
  let previousOperator = null
  // An exit nested in a compound command is conditional (or, for a function declaration, not run at
  // all). It cannot prove that every path through this fallback terminates. This deliberately stays
  // conservative: only an exit in the branch's own command list is accepted as a terminator.
  const compoundClosers = []
  const braceGroups = []
  let pendingBraceExit = false
  let functionName = false
  let functionBody = false
  for (const line of body.split('\n')) {
    const tokens = tokenizeShellLine(line, state)
    let commandTokens = []
    const reachesExit = (followingOperator = null) => {
      const command = commandWordIndex(commandTokens)
      const directExit =
        command !== -1 &&
        /^(?:exit|return)$/.test(removeQuotes(commandTokens[command].raw)) &&
        compoundClosers.length === 0 &&
        !['&&', '||', '|', '&'].includes(previousOperator) &&
        !['|', '&'].includes(followingOperator)
      commandTokens = []
      if (directExit && braceGroups.length > 0) {
        braceGroups.at(-1).hasExit = true
        return false
      }
      // A direct exit before later commands still terminates this branch. Only an exit reached
      // through `&&` / `||`, or one piped/backgrounded itself, is non-terminating.
      return directExit
    }
    for (const token of tokens) {
      if (token.raw === '(') {
        const command = commandWordIndex(commandTokens)
        functionName =
          command !== -1 &&
          /^[A-Za-z_][A-Za-z0-9_]*$/.test(removeQuotes(commandTokens[command].raw))
        parenDepth += 1
        commandTokens = []
        continue
      }
      if (token.raw === ')') {
        parenDepth = Math.max(0, parenDepth - 1)
        if (parenDepth === 0 && functionName) functionBody = true
        functionName = false
        commandTokens = []
        continue
      }
      if (parenDepth > 0) continue
      if (token.operator) {
        if (pendingBraceExit) {
          pendingBraceExit = false
          if (!['|', '&'].includes(token.raw)) return true
        }
        if (reachesExit(token.raw)) return true
        previousOperator = token.raw
        continue
      }
      const raw = removeQuotes(token.raw)
      if (functionBody && raw === '{') {
        compoundClosers.push('}')
        functionBody = false
      } else if (raw === '{') {
        braceGroups.push({ hasExit: false })
      } else if (raw === '}' && compoundClosers.at(-1) === '}') {
        compoundClosers.pop()
      } else if (raw === '}') {
        const group = braceGroups.pop()
        if (group?.hasExit) {
          if (braceGroups.length > 0) braceGroups.at(-1).hasExit = true
          pendingBraceExit = true
        }
      }
      // Braces only delimit compound groups; they are not command words. Leaving them in the
      // pending command made a following `if` look like an argument to `{`, so an exit in that
      // conditional was incorrectly promoted to the enclosing fallback group.
      if (raw === '{' || raw === '}') {
        commandTokens = []
        continue
      }
      if (commandTokens.length === 0) {
        if (raw === 'if') compoundClosers.push('fi')
        else if (['while', 'until', 'for', 'select'].includes(raw)) compoundClosers.push('done')
        else if (raw === 'case') compoundClosers.push('esac')
        // `function name { ... }` declares a body; its braces are not an executable group.
        // Defer recording the closer until the opening brace so the function body is never
        // mistaken for the fallback group that contains it.
        else if (raw === 'function') functionBody = true
        else if (raw === compoundClosers.at(-1)) compoundClosers.pop()
      }
      commandTokens.push(token)
    }
    if (pendingBraceExit) return true
    if (parenDepth === 0 && reachesExit()) return true
    // `&&`, `||`, and `|` continue a command list across an unescaped physical newline. Keep
    // them so a following `exit` is not accepted as an unconditional terminator. Other operators
    // end the list at the newline.
    if (!(tokens.at(-1)?.operator && ['&&', '||', '|'].includes(tokens.at(-1).raw)))
      previousOperator = null
  }
  return false
}

function conditionalHasTerminator(candidate, terminator) {
  const state = { quote: null, substDepth: 0, substQuote: null, backtick: false, paramDepth: 0 }
  for (const line of candidate.split('\n')) {
    const tokens = tokenizeShellLine(line, state)
    let followsSeparator = true
    for (const token of tokens) {
      if (token.operator) {
        followsSeparator = token.raw === ';'
        continue
      }
      if (followsSeparator && removeQuotes(token.raw) === terminator) return true
      followsSeparator = false
    }
  }
  return false
}

function matchingBrace(source, open) {
  let depth = 0
  let quote = null
  for (let i = open; i < source.length; i++) {
    const ch = source[i]
    if (quote === "'") {
      if (ch === "'") quote = null
      continue
    }
    if (ch === '\\') {
      i += 1
      continue
    }
    if (quote === '"') {
      if (ch === '"') quote = null
      continue
    }
    if (ch === "'" || ch === '"') {
      quote = ch
      continue
    }
    if (ch === '#' && (i === 0 || /[\s;|&(){}]/.test(source[i - 1]))) {
      const end = source.indexOf('\n', i + 1)
      if (end === -1) return -1
      i = end
      continue
    }
    if (ch === '{') depth += 1
    else if (ch === '}' && --depth === 0) return i
  }
  return -1
}

function guardCommand(left) {
  const command = left.trim().replace(/^(?:!|time)\s+/, '')
  if (command.startsWith('{')) return false
  if (isGuardCommand(command)) return true
  const words = tokenizeShellLine(command, {
    quote: null,
    substDepth: 0,
    substQuote: null,
    backtick: false,
    paramDepth: 0,
  }).filter((token) => !token.operator)
  let index = 0
  while (/^[A-Za-z_][A-Za-z0-9_]*=/.test(words[index]?.raw)) index += 1
  return (
    removeQuotes(words[index]?.raw ?? '') === 'command' &&
    removeQuotes(words[index + 1]?.raw ?? '') === '-v'
  )
}

function commandWord(command) {
  const tokens = tokenizeShellLine(command, {
    quote: null,
    substDepth: 0,
    substQuote: null,
    backtick: false,
    paramDepth: 0,
  })
  const index = commandWordIndex(tokens)
  return index === -1 ? '' : removeQuotes(tokens[index].raw)
}

function functionCalls(text, definitions) {
  if (isFunctionDeclaration(text)) return []
  const { pipelines } = scanGuardOperators(text)
  const boundaries = [-1, ...pipelines.map(({ at }) => at), text.length]
  return boundaries.slice(0, -1).flatMap((start, index) => {
    const name = functionCallCommandWord(text.slice(start + 1, boundaries[index + 1]))
    if (!definitions.has(name)) return []
    return [
      { name, pipeline: pipelines.length > 0, finalPipelineSegment: index === pipelines.length },
    ]
  })
}

function functionCallCommandWord(text) {
  const casePrefix = /^\s*case\b[\s\S]*?\bin\b\s*/.exec(text)
  const command = casePrefix ? stripLeadingCaseArmPattern(text.slice(casePrefix[0].length)) : text
  const tokens = tokenizeShellLine(command, {
    quote: null,
    substDepth: 0,
    substQuote: null,
    backtick: false,
    paramDepth: 0,
  })
  const index = commandWordIndex(tokens)
  if (index === -1) return ''
  // Command wrappers execute an external command rather than doing ordinary shell-function
  // lookup. `command disable`, for example, must not be resolved to a local `disable()` body.
  if (
    tokens
      .slice(0, index)
      .filter((token) => !token.operator)
      .some((token) => WRAPPER_WORDS.has(removeQuotes(token.raw)))
  )
    return ''
  return removeQuotes(tokens[index].raw)
}

function knownCommandStatus(text) {
  const tokens = tokenizeShellLine(text, {
    quote: null,
    substDepth: 0,
    substQuote: null,
    backtick: false,
    paramDepth: 0,
  })
  const index = commandWordIndex(tokens)
  if (index === -1) return null
  const command = removeQuotes(tokens[index].raw)
  const status = command === 'true' || command === ':' ? true : command === 'false' ? false : null
  if (status === null) return null
  const negated =
    tokens.slice(0, index).filter((token) => !token.operator && removeQuotes(token.raw) === '!')
      .length %
      2 ===
    1
  return negated ? !status : status
}

// A conditional predicate is a command list, not one command. Keep its segments in source order so
// calls and option builtins can update the caller state in the same order Bash evaluates them.
// A RHS after an unknown command is reachable, but not definitely executed; inspect it for guards
// without allowing its option changes to alter the caller's certain state.
function commandListSegments(text) {
  const { separators } = scanGuardOperators(text)
  const segments = []
  let start = 0
  let previousSeparator = null
  let previousStatus = null
  for (const separator of [...separators, { at: text.length }]) {
    const segment = text.slice(start, separator.at)
    // An unknown predecessor may succeed, so its RHS is reachable. A literal false/true is the
    // only static proof that the following AND/OR segment cannot run.
    const executes =
      previousSeparator === null ||
      (previousSeparator === '&&' && previousStatus !== false) ||
      (previousSeparator === '||' && previousStatus !== true)
    const guaranteed =
      previousSeparator === null ||
      (previousSeparator === '&&' && previousStatus === true) ||
      (previousSeparator === '||' && previousStatus === false)
    segments.push({ text: segment, executes, guaranteed })
    if (executes) previousStatus = knownCommandStatus(segment)
    start = separator.operator ? separator.at + separator.operator.length : text.length
    previousSeparator = separator.operator ?? null
  }
  return segments
}

function isGuardCommand(command) {
  const word = commandWord(command)
  return word === 'test' || word === '['
}

function fallbackAssignsValue(branch) {
  const { separators } = scanGuardOperators(branch)
  const assignment = branch.slice(0, separators[0]?.at).trim()
  const tokens = tokenizeShellLine(assignment, {
    quote: null,
    substDepth: 0,
    substQuote: null,
    backtick: false,
    paramDepth: 0,
  })
  return (
    tokens.some((token) => /^[A-Za-z_][A-Za-z0-9_]*=/.test(token.raw)) &&
    commandWordIndex(tokens) === -1
  )
}

function isDocumentationPlaceholderFallback(branch) {
  return normalizePlaceholders(branch).trim() === 'PLACEHOLDER'
}

// Return top-level `||` operators along with nested command substitutions. This intentionally
// stays lexical: the gate only promises the narrow forms documented in (g), not a shell parser.
function scanGuardOperators(line) {
  const operators = []
  const separators = []
  const pipelines = []
  const substitutions = []
  let quote = null
  let parens = 0
  let parenDelta = 0
  let braces = 0
  let braceDelta = 0
  let start = 0
  for (let i = 0; i < line.length; i++) {
    const ch = line[i]
    if (quote === "'") {
      if (ch === "'") quote = null
      continue
    }
    if (ch === '\\') {
      i += 1
      continue
    }
    if (quote === '"') {
      if (ch === '"') quote = null
      else if (ch === '$' && line[i + 1] === '(') {
        const end = matchingParen(line, i + 1)
        if (end !== -1) {
          substitutions.push({
            body: line.slice(i + 2, end),
            prefix: line.slice(0, i),
            suffix: line.slice(end + 1),
          })
          i = end
        }
      } else if (ch === '`') {
        const end = matchingBacktick(line, i)
        if (end !== -1) {
          substitutions.push({
            body: line.slice(i + 1, end),
            prefix: line.slice(0, i),
            suffix: line.slice(end + 1),
          })
          i = end
        }
      }
      continue
    }
    if (ch === "'" || ch === '"') {
      quote = ch
      continue
    }
    if (ch === '$' && line[i + 1] === '(') {
      const end = matchingParen(line, i + 1)
      if (end !== -1) {
        substitutions.push({
          body: line.slice(i + 2, end),
          prefix: line.slice(0, i),
          suffix: line.slice(end + 1),
        })
        i = end
      }
      continue
    }
    if (ch === '`') {
      const end = matchingBacktick(line, i)
      if (end !== -1) {
        substitutions.push({
          body: line.slice(i + 1, end),
          prefix: line.slice(0, i),
          suffix: line.slice(end + 1),
        })
        i = end
      }
      continue
    }
    if (ch === '#') {
      if (i === 0 || /[\s;|&(){}]/.test(line[i - 1])) break
    }
    if (ch === '(') {
      parens += 1
      parenDelta += 1
    } else if (ch === ')') {
      parenDelta -= 1
      if (parens > 0) parens -= 1
    } else if (ch === '{') {
      braces += 1
      braceDelta += 1
    } else if (ch === '}') {
      if (braces > 0) braces -= 1
      braceDelta -= 1
    }
    if (parens === 0 && braces === 0 && ch === '|' && line[i + 1] !== '|' && line[i - 1] !== '>') {
      pipelines.push({ at: i })
    } else if (parens === 0 && braces === 0 && ch === '|' && line[i + 1] === '|') {
      operators.push({ at: i, start })
      separators.push({ at: i, operator: '||' })
      i += 1
      start = i + 1
    } else if (parens === 0 && braces === 0 && ch === ';') {
      const operator = line.startsWith(';;&', i)
        ? ';;&'
        : line.startsWith(';;', i)
          ? ';;'
          : line.startsWith(';&', i)
            ? ';&'
            : ';'
      separators.push({ at: i, operator })
      i += operator.length - 1
      start = i + 1
    } else if (
      parens === 0 &&
      braces === 0 &&
      ch === '&' &&
      line[i - 1] !== '>' &&
      line[i - 1] !== '<' &&
      line[i + 1] !== '>'
    ) {
      const operator = line[i + 1] === '&' ? '&&' : '&'
      separators.push({ at: i, operator })
      if (operator === '&&') i += 1
      start = i + 1
    }
  }
  return { operators, separators, pipelines, substitutions, braceDelta, parenDelta }
}

function guardLogicalLines(source) {
  const physical = joinContinuations(source.split('\n'))
  const lines = []
  const state = { quote: null, substDepth: 0, substQuote: null, backtick: false, paramDepth: 0 }
  let pending = null
  let pendingAndOr = false
  for (const line of physical) {
    if (pending) pending.text += `\n${line.text}`
    else pending = { ...line }
    tokenizeShellLine(line.text, state)
    if (state.quote || state.substDepth > 0 || state.backtick || state.paramDepth > 0) continue
    // A physical newline after && or || remains in the same command list. Keep comments with the
    // pending list too: Bash discards them before reading the RHS on the next physical line.
    const endsAndOr = /(?:&&|\|\|)\s*(?:#.*)?$/.test(line.text)
    const commentOrBlank = /^\s*(?:#.*)?$/.test(line.text)
    if (endsAndOr || (pendingAndOr && commentOrBlank)) {
      pendingAndOr = true
      continue
    }
    lines.push(pending)
    pending = null
    pendingAndOr = false
  }
  if (pending) lines.push(pending)
  const logical = []
  const pushLogicalLine = (entry) => {
    const { separators } = scanGuardOperators(entry.text)
    const segments = []
    let start = 0
    for (const separator of separators) {
      segments.push({
        text: entry.text.slice(start, separator.at),
        nextSeparator: separator.operator,
      })
      start = separator.at + separator.operator.length
    }
    segments.push({ text: entry.text.slice(start), nextSeparator: null })
    // A function declaration and a later invocation can share one physical line. Split only
    // those top-level command lists so scope analysis sees the declaration and the invocation.
    const texts = segments.some(({ text }) => isFunctionDeclaration(text))
      ? segments
      : [{ text: entry.text }]
    for (const [segmentIndex, segment] of texts.entries()) {
      let { text } = segment
      for (;;) {
        const trailing = leadingCompoundCloserTrailing(text)
        if (!trailing) {
          if (text.trim()) {
            const logicalLine = { text, lineOffset: entry.lineOffset }
            if (segment.nextSeparator !== undefined)
              logicalLine.nextSeparator = segment.nextSeparator
            if (texts === segments && segmentIndex > 0) {
              logicalLine.previousSeparator = segments[segmentIndex - 1].nextSeparator
              logicalLine.previousText = segments[segmentIndex - 1].text
            }
            logical.push(logicalLine)
          }
          break
        }
        logical.push({ text: trailing.closer, lineOffset: entry.lineOffset })
        text = trailing.suffix
        if (!text.trim()) break
      }
    }
  }
  for (let i = 0; i < lines.length; i++) {
    let { text } = lines[i]
    const lineOffset = lines[i].lineOffset
    // A braced fallback often puts its `{` on the next physical line. It is one guard command;
    // joining only this shape prevents the following `exit` from being mistaken for unrelated flow.
    const opening = scanGuardOperators(text)
    const continuedFallback = /\|\|\s*(?:#.*)?$/.test(text)
    if (continuedFallback || (/\|\|\s*\{/.test(text) && opening.braceDelta > 0)) {
      let depth = opening.braceDelta
      let opened = depth > 0
      for (; i + 1 < lines.length;) {
        const next = lines[++i].text
        text += `\n${next}`
        depth += scanGuardOperators(next).braceDelta
        if (depth > 0) opened = true
        if (opened && depth === 0) break
        if (!opened && (!next.trim() || next.trimStart().startsWith('#'))) continue
        if (!opened) break
      }
    }
    // A conditional keyword may stand alone while its predicate occupies following logical lines.
    // Keep that predicate with the conditional so its guards are not mistaken for top-level commands.
    const conditional = conditionalOpener(text)
    if (conditional) {
      const terminator = ['if', 'elif'].includes(conditional[1]) ? 'then' : 'do'
      const hasTerminator = (candidate) => conditionalHasTerminator(candidate, terminator)
      while (!hasTerminator(text) && i + 1 < lines.length) text += `\n${lines[++i].text}`
    }
    pushLogicalLine({ text, lineOffset })
    // An inline compound closer can be followed by another top-level command on the same logical
    // line. Keep the compound command opaque, but analyze that trailing command separately.
    if (conditional) {
      let trailing = inlineCompoundParts(text).trailing
      while (trailing) {
        pushLogicalLine({ text: trailing, lineOffset })
        trailing = inlineCompoundParts(trailing).trailing
      }
    }
  }
  return logical
}

function substitutionFallsThrough(suffix) {
  const trimmed = suffix.trimStart()
  if (!trimmed) return false
  const { separators } = scanGuardOperators(trimmed)
  const semicolon = separators.find(({ operator }) => operator === ';')
  const fallback = separators.find(({ operator }) => operator === '||')
  if (semicolon && (!fallback || semicolon.at < fallback.at)) return true
  if (!fallback) return false
  const next = separators.find(({ at }) => at > fallback.at)
  const branch = trimmed.slice(fallback.at + 2, next?.at)
  return !branchReachesExit(branch)
}

function substitutionAssignmentTerminates(prefix, suffix) {
  return (
    !commandMasksSubstitution(prefix) &&
    assignmentRedirectionsBeforeGuard(suffix) &&
    !substitutionFallsThrough(suffix)
  )
}

// Redirections are part of an assignment command, not a command that can mask its status. Keep
// this deliberately narrow: consume only leading redirections before the `||` guard, leaving any
// command or list operator between the assignment and the guard as a fallthrough.
function assignmentRedirectionsBeforeGuard(suffix) {
  const redirection =
    /^(?:\d+)?(?:&>>|&>|>>|<<|<>|>&|<&|>|<)(?:\s*(?:"(?:[^"\\]|\\.)*"|'[^']*'|\\.|[^\s;|&()])+)?\s*/
  let remainder = suffix.trimStart()
  while (true) {
    if (/^\|\|/.test(remainder)) return true
    const match = remainder.match(redirection)
    if (!match) return false
    remainder = remainder.slice(match[0].length).trimStart()
  }
}

function substitutionIsFollowedInSameCommand(suffix) {
  const { substitutions, separators } = scanGuardOperators(suffix)
  const firstSeparator = separators[0]?.at ?? Infinity
  return (
    commandMasksSubstitution(suffix.slice(0, firstSeparator)) ||
    substitutions.some(({ prefix }) => prefix.length < firstSeparator)
  )
}

function isStructuralCloser(text) {
  return /^(?:;;|;&|;;&|fi|done|esac|else|elif|\}|\))(?:[;\s]|$)/.test(text)
}

function isImplicitStatusTerminator(text) {
  return /^(?:return|exit)(?:\s*(?:;|#.*)?$)/.test(text)
}

// A direct return/exit makes the remainder of its function command list unreachable. Keep this
// deliberately narrow: a terminator in a conditional, an AND/OR list, a pipeline, or a background
// command does not prove that later option updates cannot run.
function unconditionalTerminator(text) {
  if (
    /(?:^|[;\s])(?:if|then|else|elif|while|until|for|select|case|do|done|fi|esac)(?:[;\s]|$)/.test(
      text,
    )
  )
    return null
  const { separators, pipelines } = scanGuardOperators(text)
  let start = 0
  for (let index = 0; index <= separators.length; index += 1) {
    const separator = separators[index]
    const end = separator?.at ?? text.length
    const previous = separators[index - 1]?.operator
    const hasPipeline = pipelines.some(({ at }) => at >= start && at < end)
    if (
      /^(?:return|exit)\b/.test(commandWord(text.slice(start, end))) &&
      !hasPipeline &&
      !['&&', '||', '&'].includes(previous) &&
      (!separator || separator.operator === ';')
    )
      return { end }
    if (!separator) break
    start = separator.at + separator.operator.length
  }
  return null
}

function commandMasksSubstitution(prefix) {
  const state = { quote: null, substDepth: 0, substQuote: null, backtick: false, paramDepth: 0 }
  return commandWordIndex(tokenizeShellLine(prefix, state)) !== -1
}

function stripLeadingCaseArmPattern(text) {
  let quote = null
  let parenDepth = 0
  for (let i = 0; i < text.length; i++) {
    const ch = text[i]
    if (ch === '\\') {
      i += 1
      continue
    }
    if (quote) {
      if (ch === quote) quote = null
      continue
    }
    if (ch === "'" || ch === '"') {
      quote = ch
      continue
    }
    if (ch === '(') {
      parenDepth += 1
      continue
    }
    if (ch === ')' && parenDepth === 0) return text.slice(i + 1).trimStart()
    if (ch === ')') parenDepth -= 1
  }
  return text
}

function bareGuardFallsThrough(
  text,
  options,
  hasLaterCommand,
  enclosingContinues,
  inCaseArm = false,
  errexitIgnored = false,
) {
  const source = text.trimStart().replace(/^\{\s*/, '')
  const { separators } = scanGuardOperators(source)
  let segmentOptions = { ...options }
  let start = 0
  const andFailureReachesTerminator = (from) => {
    for (let index = from + 1; index < separators.length; index += 1) {
      const separator = separators[index]
      if (separator.operator === '&&') continue
      if (separator.operator === '||')
        return branchReachesExit(source.slice(separator.at + separator.operator.length))
      return false
    }
    return false
  }
  for (let i = 0; i <= separators.length; i++) {
    const separator = separators[i]
    const segment = source.slice(start, separator?.at)
    const rawCommand = segment.trim()
    const commandSource = inCaseArm ? stripLeadingCaseArmPattern(rawCommand) : rawCommand
    const command = commandSource
      .trim()
      .replace(/^(?:\{\s*)+/, '')
      .replace(/^(?:!|time)\s+/, '')
    const isGuard = isGuardCommand(command)
    const errexitExempt =
      (separator?.operator === '&&' && isGuard) ||
      (separator?.operator === '&' && isGuard) ||
      (/^!\s+/.test(commandSource) && isGuardCommand(commandSource.replace(/^!\s+/, ''))) ||
      (scanGuardOperators(rawCommand).pipelines.length > 0 && !segmentOptions.pipefail)
    const fallsThrough =
      (!separator && (hasLaterCommand || enclosingContinues)) ||
      ((separator?.operator === ';' || separator?.operator === '&') &&
        (() => {
          const remainder = source.slice(separator.at + 1).trim()
          return (
            (separator.operator === '&' && (hasLaterCommand || enclosingContinues)) ||
            (remainder && !remainder.startsWith('#') && !isStructuralCloser(remainder)) ||
            (separator.operator === ';' && !remainder && enclosingContinues)
          )
        })()) ||
      (separator?.operator === '&&' &&
        (hasLaterCommand ||
          enclosingContinues ||
          separators.slice(i + 1).some(({ operator }) => operator === ';')) &&
        !andFailureReachesTerminator(i))
    if (
      isGuard &&
      (!segmentOptions.errexit || errexitExempt || compoundErrexitExempt(text)) &&
      fallsThrough
    )
      return true
    if (!separator) break
    segmentOptions = shellOptionStateAfter(segment, segmentOptions)
    if (errexitIgnored) segmentOptions.errexit = false
    start = separator.at + separator.operator.length
  }
  return false
}

// `errexit` does not apply to a command on the non-final side of an AND list.
// Keep checking that narrow case even when the enclosing block starts with `set -e`.
function isFunctionDeclaration(text) {
  return /^\s*(?:function\s+[A-Za-z_][A-Za-z0-9_]*(?:\s*\(\s*\))?|[A-Za-z_][A-Za-z0-9_]*\s*\(\s*\))\s*(?:\{|\(|$)/.test(
    text,
  )
}

function conditionalOpener(text) {
  return text.match(
    /^\s*(?:(?:(?:function\s+[A-Za-z_][A-Za-z0-9_]*|[A-Za-z_][A-Za-z0-9_]*\s*\(\s*\))\s*)?\{\s*)?(if|elif|while|until|for|select)\b/,
  )
}

function embeddedConditionalStart(text) {
  const match = /(?:^|;)\s*(if|while|until|for|select)\b/.exec(text)
  if (!match) return -1
  return match.index + match[0].lastIndexOf(match[1])
}

function caseOpener(text) {
  return /^\s*(?:(?:(?:function\s+[A-Za-z_][A-Za-z0-9_]*|[A-Za-z_][A-Za-z0-9_]*\s*\(\s*\))\s*)?\{\s*)?case\b/.test(
    text,
  )
}

function staticCaseWord(text) {
  const word = text.trim()
  if (/^[A-Za-z0-9_./:-]+$/.test(word)) return word
  if (/^'[^']*'$/.test(word)) return word.slice(1, -1)
  return null
}

function staticCaseSelector(text) {
  const match = /\bcase\s+([^\s]+)\s+in\b/.exec(text)
  return match ? staticCaseWord(match[1]) : null
}

function staticCaseArmMatches(text, selector) {
  if (selector === null) return null
  const header = /\bcase\s+[^\s]+\s+in\b([\s\S]*)/.exec(text)
  const arm = /^\s*([^()]*)\)/.exec(header ? header[1] : text)
  if (!arm) return null
  const patterns = arm[1].split('|').map((pattern) => pattern.trim())
  if (patterns.includes('*')) return true
  const literals = patterns.map(staticCaseWord)
  return literals.every((pattern) => pattern !== null) ? literals.includes(selector) : null
}

// A conditional's predicate always runs, while its body may not. Keep option tracking on the
// former only: applying `set` or `shopt` from an unselected branch leaks state into the command
// list after the conditional, but ignoring the predicate loses an effect Bash definitely applies.
function conditionalPredicate(text, conditional) {
  const keyword = conditional[1]
  const terminator = keyword === 'if' || keyword === 'elif' ? 'then' : 'do'
  const keywordAt = conditional.index + conditional[0].lastIndexOf(keyword)
  const predicateAt = keywordAt + keyword.length
  // Shell grammar places `then`/`do` after a command separator. This narrow lexical split matches
  // the conditional opener recognized above without treating a similarly named command argument as
  // a terminator.
  const close = new RegExp(`(?:^|[;\\n])\\s*${terminator}\\b`).exec(text.slice(predicateAt))
  return text.slice(predicateAt, predicateAt + (close?.index ?? text.length))
}

function staticallySelectedConditionalBody(text, conditional, bodies) {
  if (conditional[1] !== 'if' || bodies.length === 0) return null
  const status = knownCommandStatus(conditionalPredicate(text, conditional))
  if (status === true) return bodies.findIndex(({ branch }) => branch === 'then')
  if (status === false) return bodies.findIndex(({ branch }) => branch === 'else')
  return null
}

function compoundErrexitExempt(text) {
  const trimmed = text.trimStart()
  const close =
    trimmed[0] === '{'
      ? matchingBrace(trimmed, 0)
      : trimmed[0] === '('
        ? matchingParen(trimmed, 0)
        : -1
  return close !== -1 && compoundListOperatorAfterRedirections(trimmed.slice(close + 1))
}

function compoundListOperatorAfterRedirections(text) {
  const redirection =
    /^(?:\d+)?(?:&>>|&>|>>|<<|<>|>&|<&|>|<)(?:\s*(?:"(?:[^"\\]|\\.)*"|'[^']*'|\\.|[^\s;|&()])+)?\s*/
  let remainder = text.trimStart()
  while (true) {
    if (/^(?:&&|\|\||\|)/.test(remainder)) return true
    const match = remainder.match(redirection)
    if (!match) return false
    remainder = remainder.slice(match[0].length).trimStart()
  }
}

function testedCompoundCloser(text) {
  const match = text.match(/^\s*(?:fi|done|esac)\b|^\s*[})]/)
  return match !== null && compoundListOperatorAfterRedirections(text.slice(match[0].length))
}

function inlineSubshellBody(text) {
  const trimmed = text.trimStart()
  if (!trimmed.startsWith('(')) return null
  const close = matchingParen(trimmed, 0)
  if (close === -1 || trimmed.slice(close + 1).trim()) return null
  return trimmed.slice(1, close)
}

function caseArmTerminator(text) {
  // `;&` and `;;&` deliberately continue into the next case arm. Only `;;` ends the selected
  // arm's flow and therefore gives subsequent arms an independent scope.
  return scanGuardOperators(text).separators.some(({ operator }) => operator === ';;')
}

function conditionalBranchDelimiter(text) {
  const { separators } = scanGuardOperators(text)
  let start = 0
  for (const separator of separators) {
    start = separator.at + separator.operator.length
    if (/^\s*(?:else|elif)\b/.test(text.slice(start))) return start
  }
  return -1
}

function incrementScopedBranch(scope) {
  const match = /^(.*\.)(\d+)$/.exec(scope)
  return match ? `${match[1]}${Number(match[2]) + 1}` : scope
}

function conditionalBranchSuffix(text) {
  const delimiter = conditionalBranchDelimiter(text)
  if (delimiter === -1) return null
  const match = /^\s*(?:else|elif)\b([\s\S]*)$/.exec(text.slice(delimiter))
  if (!match) return null
  return { prefix: text.slice(0, delimiter), suffix: match[1] }
}

function caseArmSuffix(text) {
  const terminator = [...scanGuardOperators(text).separators]
    .reverse()
    .find(({ operator }) => operator === ';;')
  if (!terminator) return null
  const suffix = text.slice(terminator.at + terminator.operator.length)
  const pattern = /^\s*(?:[^()\\]|\\.)*\)\s*([\s\S]*)$/.exec(suffix)
  if (!pattern) return null
  return { prefix: text.slice(0, terminator.at + terminator.operator.length), suffix: pattern[1] }
}

function functionDefinitions(logical) {
  const scopes = new Map()
  for (const line of logical) {
    if (line.functionScope === null) continue
    const lines = scopes.get(line.functionScope) ?? []
    lines.push(line)
    scopes.set(line.functionScope, lines)
  }
  const definitions = new Map()
  for (const lines of scopes.values()) {
    if (lines.some((line) => line.inSubshell)) continue
    if (lines.some((line) => isFunctionDeclaration(line.text) && !line.definitionReachable))
      continue
    const source = lines.map(({ text }) => text).join('\n')
    const declaration = source.match(
      /^\s*(?:function\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(\s*\))?|([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\))\s*([({])/,
    )
    if (!declaration) continue
    const name = declaration[1] ?? declaration[2]
    const opener = declaration.index + declaration[0].lastIndexOf(declaration[3])
    const closer =
      declaration[3] === '{' ? matchingBrace(source, opener) : matchingParen(source, opener)
    if (closer === -1) continue
    definitions.set(name, {
      body: source.slice(opener + 1, closer),
      lineOffset: lines[0].lineOffset + source.slice(0, opener + 1).split('\n').length - 1,
    })
  }
  return definitions
}

function functionDeclarationName(text) {
  const declaration = text.match(
    /^\s*(?:function\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(\s*\))?|([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\))\s*[({]/,
  )
  return declaration?.[1] ?? declaration?.[2] ?? null
}

// Split a command list only when it invokes a known function. The general guard analysis needs
// the whole list (for example, to see `test -f x || exit 1`), while function calls need their
// exact AND/OR position: only a non-final list member suppresses errexit in Bash.
function splitInvokedFunctionLines(logical, definitions) {
  return logical.flatMap((line) => {
    if (line.invokedFunctionLineSplit || conditionalOpener(line.text)) return [line]
    if (isFunctionDeclaration(line.text)) {
      const open = line.text.indexOf('{')
      const close = open === -1 ? -1 : matchingBrace(line.text, open)
      const suffix = close === -1 ? '' : line.text.slice(close + 1)
      if (!suffix.trim()) return [line]
      return [
        { ...line, text: line.text.slice(0, close + 1) },
        ...splitInvokedFunctionLines([{ ...line, text: suffix }], definitions),
      ]
    }
    const { separators } = scanGuardOperators(line.text)
    if (separators.length === 0) return [line]
    const segments = []
    let start = 0
    for (const separator of separators) {
      segments.push({
        text: line.text.slice(start, separator.at),
        nextSeparator: separator.operator,
      })
      start = separator.at + separator.operator.length
    }
    segments.push({ text: line.text.slice(start), nextSeparator: null })
    if (!segments.some(({ text }) => definitions.has(functionCallCommandWord(text)))) return [line]
    return segments
      .filter(({ text }) => text.trim())
      .map(({ text, nextSeparator }, index, invokedSegments) => ({
        ...line,
        text,
        nextSeparator,
        previousSeparator: index === 0 ? null : invokedSegments[index - 1].nextSeparator,
        previousText: index === 0 ? null : invokedSegments[index - 1].text,
        invokedFunctionLineSplit: true,
      }))
  })
}

function annotateFunctionScopes(logical) {
  let braceDepth = 0
  let parenDepth = 0
  let pendingFunction = false
  let nextScope = 0
  const functions = []
  const groups = []
  const conditionals = []
  const cases = []

  for (const line of logical) {
    const trimmed = line.text.trim()
    if (caseOpener(trimmed))
      cases.push({
        id: ++nextScope,
        arm: 0,
        selector: staticCaseSelector(trimmed),
        armMatches: null,
      })
    const caseArmMatches = staticCaseArmMatches(trimmed, cases.at(-1)?.selector ?? null)
    if (caseArmMatches !== null) cases.at(-1).armMatches = caseArmMatches
    line.functionScope = functions.at(-1)?.id ?? null
    line.groupScope = groups.map(({ id }) => id).join('/')
    line.inSubshell = groups.some(({ kind }) => kind === 'paren')
    line.definitionReachable =
      conditionals.every(
        ({ predicateStatus, branch }) => predicateStatus !== false || branch > 0,
      ) && cases.every(({ armMatches }) => armMatches !== false)
    const closesConditional = /^fi(?:[;\s]|$)/.test(trimmed)
    const closesLoop = /^done(?:[;\s]|$)/.test(trimmed)
    const closesCase = /^esac(?:[;\s]|$)/.test(trimmed)
    if (/^(?:else|elif)\b/.test(trimmed) && conditionals.at(-1)?.closer === 'fi') {
      const current = conditionals.at(-1)
      current.branch += 1
      if (/^elif\b/.test(trimmed)) current.sawElif = true
      if (/^else\b/.test(trimmed) && current.predicateStatus === false && !current.sawElif)
        current.selectedBranch = current.branch
    }
    line.conditionalScope = conditionals.map(({ id, branch }) => `${id}.${branch}`).join('/')
    line.caseScope = cases.map(({ id, arm }) => `${id}.${arm}`).join('/')
    const { braceDelta, parenDelta } = scanGuardOperators(line.text)
    const declaration = isFunctionDeclaration(line.text)
    const opensFunction = declaration || (pendingFunction && (braceDelta > 0 || parenDelta > 0))
    // An entire brace-bodied declaration can sit on one logical line. It never executes while
    // declared, but its body still needs a temporary function scope so `set` cannot leak outward.
    const inlineFunction = declaration && braceDelta === 0 && /\{/.test(line.text)
    pendingFunction = declaration && braceDelta === 0 && parenDelta === 0 && !inlineFunction
    const priorBraceDepth = braceDepth
    const priorParenDepth = parenDepth
    braceDepth += braceDelta
    parenDepth += parenDelta
    for (let depth = priorBraceDepth + 1; depth <= braceDepth; depth++)
      groups.push({ id: ++nextScope, kind: 'brace', depth })
    for (let depth = priorParenDepth + 1; depth <= parenDepth; depth++)
      groups.push({ id: ++nextScope, kind: 'paren', depth })
    if (opensFunction && braceDelta > 0) {
      const id = ++nextScope
      functions.push({ id, bodyKind: 'brace', bodyDepth: braceDepth })
      line.functionScope = id
    } else if (opensFunction && parenDelta > 0) {
      const id = ++nextScope
      functions.push({ id, bodyKind: 'paren', bodyDepth: parenDepth })
      line.functionScope = id
    } else if (inlineFunction) {
      line.functionScope = ++nextScope
    }
    while (
      functions.length > 0 &&
      ((functions.at(-1).bodyKind === 'brace' && braceDepth < functions.at(-1).bodyDepth) ||
        (functions.at(-1).bodyKind === 'paren' && parenDepth < functions.at(-1).bodyDepth))
    )
      functions.pop()
    while (groups.at(-1)?.kind === 'brace' && groups.at(-1).depth > braceDepth) groups.pop()
    while (groups.at(-1)?.kind === 'paren' && groups.at(-1).depth > parenDepth) groups.pop()
    const conditional = conditionalOpener(trimmed)
    const closer = conditional?.[1] === 'if' || conditional?.[1] === 'elif' ? 'fi' : 'done'
    const closesInlineConditional =
      closer && new RegExp(`(?:^|[;\\n])\\s*${closer}(?:[;\\s]|$)`).test(trimmed)
    if (conditional && conditional[1] !== 'elif' && !closesInlineConditional)
      conditionals.push({
        id: ++nextScope,
        branch: 0,
        closer,
        predicateStatus: knownCommandStatus(conditionalPredicate(line.text, conditional)),
        selectedBranch:
          knownCommandStatus(conditionalPredicate(line.text, conditional)) === true ? 0 : null,
        sawElif: false,
      })
    if (closesConditional) line.closedConditional = conditionals.pop()
    if (closesLoop) line.closedConditional = conditionals.pop()
    if (caseArmTerminator(trimmed) && cases.length > 0) {
      cases.at(-1).arm += 1
      cases.at(-1).armMatches = null
    }
    if (closesCase) cases.pop()
    if (conditionalBranchDelimiter(trimmed) !== -1 && conditionals.at(-1)?.closer === 'fi') {
      const current = conditionals.at(-1)
      current.branch += 1
      if (/^\s*elif\b/.test(trimmed)) current.sawElif = true
      if (/^\s*else\b/.test(trimmed) && current.predicateStatus === false && !current.sawElif)
        current.selectedBranch = current.branch
    }
    // A command after a compound closer belongs to the surrounding command list, not the branch
    // that just ended. Its option changes must therefore update the outer shell state.
    if (closesConditional || closesLoop || closesCase) {
      line.conditionalScope = conditionals.map(({ id, branch }) => `${id}.${branch}`).join('/')
      line.caseScope = cases.map(({ id, arm }) => `${id}.${arm}`).join('/')
    }
  }
  return logical
}

function matchingParen(line, open) {
  let depth = 0
  let quote = null
  for (let i = open; i < line.length; i++) {
    const ch = line[i]
    if (quote === "'") {
      if (ch === "'") quote = null
      continue
    }
    if (ch === '\\') {
      i += 1
      continue
    }
    if (quote === '"') {
      if (ch === '"') quote = null
      continue
    }
    if (ch === "'" || ch === '"') {
      quote = ch
      continue
    }
    if (ch === '#' && (i === 0 || /[\s;|&(){}]/.test(line[i - 1]))) {
      const end = line.indexOf('\n', i + 1)
      if (end === -1) return -1
      i = end
      continue
    }
    if (ch === '(') depth += 1
    else if (ch === ')' && --depth === 0) return i
  }
  return -1
}

// Find branch bodies and a trailing top-level command after an inline conditional or loop. The
// raw-regexp version treated a matching word in quoted output or a comment as a closer; this small
// lexer follows the shell's quote/comment boundaries and tracks nested compound commands before
// accepting the closer.
function inlineCompoundParts(text) {
  const closers = []
  const bodies = []
  let bodyStart = null
  let bodyBranch = null
  const addBody = (end) => {
    if (bodyStart === null) return
    const source = text
      .slice(bodyStart, end)
      .replace(/^\s*;\s*/, '')
      .replace(/\s*;\s*$/, '')
    if (source)
      bodies.push({
        text: source,
        lineOffset: text.slice(0, bodyStart).split('\n').length - 1,
        branch: bodyBranch,
      })
    bodyStart = null
    bodyBranch = null
  }
  let commandStart = true
  for (let i = 0; i < text.length;) {
    const ch = text[i]
    if (ch === '\\') {
      i += 2
      commandStart = false
      continue
    }
    if (ch === "'" || ch === '"') {
      const quote = ch
      i += 1
      while (i < text.length) {
        if (text[i] === '\\' && quote === '"') i += 2
        else if (text[i++] === quote) break
      }
      commandStart = false
      continue
    }
    if (ch === '$' && text[i + 1] === '(') {
      const end = matchingParen(text, i + 1)
      if (end === -1) return { bodies, trailing: null }
      i = end + 1
      commandStart = false
      continue
    }
    if (ch === '#' && (i === 0 || /[\s;|&(){}]/.test(text[i - 1]))) {
      const end = text.indexOf('\n', i + 1)
      if (end === -1) return { bodies, trailing: null }
      i = end + 1
      commandStart = true
      continue
    }
    if (/\s/.test(ch)) {
      commandStart ||= ch === '\n'
      i += 1
      continue
    }
    if (ch === ';' || ch === '|' || ch === '&') {
      commandStart = true
      i += 1
      continue
    }
    if (ch === '(' || ch === ')' || ch === '{' || ch === '}') {
      commandStart = ch === '{'
      i += 1
      continue
    }
    const start = i
    while (i < text.length && !/[\s;|&(){}'"\\]/.test(text[i])) i += 1
    const word = text.slice(start, i)
    if (commandStart) {
      if (word === 'if') closers.push('fi')
      else if (['while', 'until', 'for', 'select'].includes(word)) closers.push('done')
      else if (word === 'case') closers.push('esac')
      else if ((word === 'then' || word === 'do') && closers.length === 1) {
        bodyStart = i
        bodyBranch = word === 'then' ? 'then' : null
      } else if ((word === 'else' || word === 'elif') && closers.length === 1) {
        addBody(start)
        if (word === 'else') {
          bodyStart = i
          bodyBranch = 'else'
        }
      } else if (word === closers.at(-1)) {
        closers.pop()
        let after = i
        while (/[ \t]/.test(text[after])) after += 1
        if (closers.length === 0) {
          addBody(start)
          const suffix = text.slice(after).trimStart()
          return {
            bodies,
            trailing:
              text[after] === ';'
                ? text.slice(after + 1).trimStart()
                : /^(?:&&|\|\|)/.test(suffix)
                  ? suffix
                  : null,
          }
        }
      }
    }
    commandStart = false
  }
  return { bodies, trailing: null }
}

function leadingCompoundCloserTrailing(text) {
  const match = text.match(/^(\s*(?:fi|done|esac)\b|\s*\})([ \t]*;)([\s\S]*)$/)
  if (!match) return null
  return { closer: match[1], suffix: match[3].trimStart() }
}

function sameOrOuterScope(scope, laterScope) {
  return (
    scope === laterScope || (scope && (laterScope === '' || scope.startsWith(`${laterScope}/`)))
  )
}

function matchingBacktick(line, open) {
  for (let i = open + 1; i < line.length; i++) {
    if (line[i] === '\\') {
      i += 1
      continue
    }
    if (line[i] === '`') return i
  }
  return -1
}

/**
 * Report recognised shell guards whose failure branch falls through. Returns
 * [{ lineOffset, guard }] with line offsets relative to `body`.
 */
export function findInertGuards(body) {
  const masked = maskHeredocBodies(body.split('\n')).join('\n')
  const findings = []
  const inspect = (
    source,
    lineOffset,
    inheritedErrexit = false,
    enclosingContinues = false,
    inheritedErrexitEnabled = false,
    inheritedPipefail = false,
    inheritedErrexitIgnored = false,
    inheritedDefinitions = new Map(),
    activeDefinitions = new Set(),
  ) => {
    let options = {
      errexit: inheritedErrexit,
      inheritErrexit: inheritedErrexitEnabled,
      pipefail: inheritedPipefail,
    }
    let logical = annotateFunctionScopes(guardLogicalLines(source))
    let localDefinitions = functionDefinitions(logical)
    let allDefinitions = new Map([...inheritedDefinitions, ...localDefinitions])
    logical = annotateFunctionScopes(splitInvokedFunctionLines(logical, allDefinitions))
    localDefinitions = functionDefinitions(logical)
    allDefinitions = new Map([...inheritedDefinitions, ...localDefinitions])
    // Function declarations only take effect once execution reaches them. Keep the complete map
    // for structural splitting, but resolve calls with this sequential environment.
    const definitions = new Map(inheritedDefinitions)
    // A multiline compound's closer carries the AND/OR-list status-test exemption for its whole
    // body. The closer itself still has the just-closing scope, so retain that scope and apply it
    // to every nested line while the compound is inspected.
    const testedCompoundScopes = logical
      .filter(({ text }) => testedCompoundCloser(text))
      .map(({ conditionalScope, caseScope, groupScope }) => ({
        conditionalScope,
        caseScope,
        groupScope,
      }))
    const isInTestedCompoundScope = (conditionalScope, caseScope, groupScope) =>
      testedCompoundScopes.some(
        (tested) =>
          scopeIsOuter(tested.conditionalScope, conditionalScope) &&
          scopeIsOuter(tested.caseScope, caseScope) &&
          scopeIsOuter(tested.groupScope, groupScope),
      )
    const invokedFunctions = new Set()
    const definitionsInOrder = new Map(inheritedDefinitions)
    for (const { text, functionScope } of logical) {
      if (functionScope === null)
        for (const { name } of functionCalls(text, definitionsInOrder)) invokedFunctions.add(name)
      if (!isFunctionDeclaration(text)) continue
      const name = functionDeclarationName(text)
      if (name && localDefinitions.has(name))
        definitionsInOrder.set(name, localDefinitions.get(name))
    }
    for (const definition of localDefinitions.values()) {
      for (const { text } of guardLogicalLines(definition.body)) {
        for (const { name } of functionCalls(text, allDefinitions)) invokedFunctions.add(name)
      }
    }
    // An uninvoked function is still checked for a bare guard that it itself makes ineffective
    // (for example, one followed by `return 0`), but it has no caller-provided shell options yet.
    // Functions that are invoked below are checked there instead, with their call-site state.
    for (const [name, definition] of localDefinitions) {
      if (invokedFunctions.has(name)) continue
      inspect(
        definition.body,
        lineOffset + definition.lineOffset,
        false,
        false,
        false,
        false,
        false,
        definitions,
      )
    }
    const scopedOptions = new Map()
    const scopeIsOuter = (outer, inner) =>
      outer === '' || outer === inner || inner.startsWith(`${outer}/`)
    const optionsForScope = (functionScope, conditionalScope, caseScope, fallback) => {
      let closest = fallback
      let closestDepth = -1
      for (const scoped of scopedOptions.values()) {
        if (
          scoped.functionScope !== functionScope ||
          !scopeIsOuter(scoped.conditionalScope, conditionalScope) ||
          !scopeIsOuter(scoped.caseScope, caseScope)
        )
          continue
        const depth = scoped.conditionalScope.length + scoped.caseScope.length
        if (depth > closestDepth) {
          closest = scoped.options
          closestDepth = depth
        }
      }
      return closest
    }
    const updateOptions = (functionScope, conditionalScope, caseScope, next) => {
      if (!conditionalScope && !caseScope) {
        if (functionScope === null) options = next
        return
      }
      scopedOptions.set(`${functionScope}|${conditionalScope}|${caseScope}`, {
        functionScope,
        conditionalScope,
        caseScope,
        options: next,
      })
    }
    const reportInertPredicateGuard = (predicate, predicateOptions, localOffset) => {
      // An explicit `||` fallback is conditional syntax, not a falling-through guard.
      if (
        bareGuardFallsThrough(
          predicate,
          { ...predicateOptions, errexit: false },
          false,
          false,
          false,
          true,
        )
      )
        findings.push({ lineOffset: lineOffset + localOffset, guard: predicate.trim() })
    }
    for (let index = 0; index < logical.length; index++) {
      const {
        text,
        lineOffset: localOffset,
        functionScope,
        conditionalScope,
        caseScope,
        groupScope,
        nextSeparator,
        previousSeparator,
        previousText,
        closedConditional,
      } = logical[index]
      if (isFunctionDeclaration(text)) {
        const name = functionDeclarationName(text)
        if (name && localDefinitions.has(name)) definitions.set(name, localDefinitions.get(name))
      }
      // A declaration does not execute its body. Analyze that body only where the function is
      // invoked, because Bash supplies the caller's option state at that point rather than the
      // state that happened to be active while defining it.
      if (functionScope !== null) continue
      let lineOptions = optionsForScope(functionScope, conditionalScope, caseScope, options)
      const lineErrexitIgnored =
        inheritedErrexitIgnored || isInTestedCompoundScope(conditionalScope, caseScope, groupScope)
      const conditional = conditionalOpener(text)
      const { operators, substitutions } = scanGuardOperators(text)
      const hasLaterCommand = logical
        .slice(index + 1)
        .some(
          ({
            text: later,
            functionScope: laterScope,
            conditionalScope: laterConditionalScope,
            caseScope: laterCaseScope,
          }) => {
            const trimmed = later.trim()
            return (
              functionScope === laterScope &&
              sameOrOuterScope(conditionalScope, laterConditionalScope) &&
              sameOrOuterScope(caseScope, laterCaseScope) &&
              trimmed !== '' &&
              !trimmed.startsWith('#') &&
              !isImplicitStatusTerminator(trimmed) &&
              !isStructuralCloser(trimmed)
            )
          },
        )
      const subshell = inlineSubshellBody(text)
      if (subshell !== null) {
        inspect(
          subshell,
          lineOffset + localOffset,
          lineOptions.errexit,
          enclosingContinues || hasLaterCommand,
          lineOptions.inheritErrexit,
          lineOptions.pipefail,
          lineErrexitIgnored,
          definitions,
          activeDefinitions,
        )
        continue
      }
      const embeddedConditional = embeddedConditionalStart(text)
      if (embeddedConditional > 0) {
        const compound = inlineCompoundParts(text.slice(embeddedConditional))
        if (compound.bodies.length > 0) {
          const prefixOptions = shellOptionStateAfter(
            text.slice(0, embeddedConditional),
            lineOptions,
          )
          const embeddedOpener = conditionalOpener(text.slice(embeddedConditional))
          const predicate = conditionalPredicate(text.slice(embeddedConditional), embeddedOpener)
          for (const inner of scanGuardOperators(predicate).substitutions) {
            const innerOptions = shellOptionStateAfter(inner.prefix, prefixOptions)
            // A predicate tests the assignment's status, so Bash ignores errexit in its command
            // substitutions even when inherit_errexit is enabled.
            inspect(
              inner.body,
              lineOffset + localOffset,
              innerOptions.errexit && innerOptions.inheritErrexit,
              false,
              innerOptions.inheritErrexit,
              innerOptions.pipefail,
              true,
              definitions,
              activeDefinitions,
            )
          }
          reportInertPredicateGuard(predicate, prefixOptions, localOffset)
          let outerOptions = prefixOptions
          const testedCompound = /^\s*(?:&&|\|\|)/.test(compound.trailing ?? '')
          const selectedBody = staticallySelectedConditionalBody(
            text.slice(embeddedConditional),
            embeddedOpener,
            compound.bodies,
          )
          for (const [bodyIndex, body] of compound.bodies.entries()) {
            const bodyOptions = inspect(
              body.text,
              lineOffset + localOffset + body.lineOffset,
              prefixOptions.errexit,
              enclosingContinues || hasLaterCommand,
              prefixOptions.inheritErrexit,
              prefixOptions.pipefail,
              inheritedErrexitIgnored || testedCompound,
              definitions,
              activeDefinitions,
            )
            if (bodyIndex === selectedBody) outerOptions = bodyOptions
          }
          updateOptions(functionScope, conditionalScope, caseScope, outerOptions)
          if (compound.trailing && !/^\s*(?:&&|\|\|)/.test(compound.trailing)) {
            const trailingOptions = inspect(
              compound.trailing,
              lineOffset + localOffset,
              outerOptions.errexit,
              enclosingContinues,
              outerOptions.inheritErrexit,
              outerOptions.pipefail,
              inheritedErrexitIgnored,
              definitions,
              activeDefinitions,
            )
            updateOptions(functionScope, conditionalScope, caseScope, trailingOptions)
          }
          continue
        }
      }
      if (conditional) {
        // A predicate executes before its selected body. Retain its option changes in that branch,
        // but never promote branch-local state to the enclosing command list.
        const predicate = conditionalPredicate(text, conditional)
        let predicateOptions = lineOptions
        for (const { text: predicateSegment, executes, guaranteed } of commandListSegments(
          predicate,
        )) {
          if (!executes) continue
          for (const { name } of functionCalls(predicateSegment, definitions)) {
            if (activeDefinitions.has(name)) continue
            const predicateDefinition = definitions.get(name)
            // Bash ignores errexit throughout a function whose status is tested by a conditional,
            // but does not disable the option. The function still runs in the caller's shell and
            // can change its options when its predicate-list segment definitely executes.
            const functionOptions = inspect(
              predicateDefinition.body,
              lineOffset + predicateDefinition.lineOffset,
              predicateOptions.errexit,
              enclosingContinues,
              predicateOptions.inheritErrexit,
              predicateOptions.pipefail,
              true,
              definitions,
              new Set([...activeDefinitions, name]),
            )
            if (guaranteed) predicateOptions = functionOptions
          }
          if (guaranteed)
            predicateOptions = shellOptionStateAfter(predicateSegment, predicateOptions)
        }
        for (const inner of scanGuardOperators(predicate).substitutions) {
          const innerOptions = shellOptionStateAfter(inner.prefix, predicateOptions)
          // A predicate tests the assignment's status, so Bash ignores errexit in its command
          // substitutions even when inherit_errexit is enabled.
          inspect(
            inner.body,
            lineOffset + localOffset,
            innerOptions.errexit && innerOptions.inheritErrexit,
            false,
            innerOptions.inheritErrexit,
            innerOptions.pipefail,
            true,
            definitions,
            activeDefinitions,
          )
        }
        reportInertPredicateGuard(predicate, predicateOptions, localOffset)
        let outerOptions = predicateOptions
        const conditionalInvocation =
          nextSeparator === '&&' ||
          nextSeparator === '||' ||
          scanGuardOperators(text).separators.some(
            ({ operator }) => operator === '&&' || operator === '||',
          )
        const compoundBodies = inlineCompoundParts(text).bodies
        const selectedBody = staticallySelectedConditionalBody(text, conditional, compoundBodies)
        for (const [bodyIndex, body] of compoundBodies.entries()) {
          const bodyOptions = inspect(
            body.text,
            lineOffset + localOffset + body.lineOffset,
            predicateOptions.errexit,
            enclosingContinues || hasLaterCommand,
            predicateOptions.inheritErrexit,
            predicateOptions.pipefail,
            lineErrexitIgnored || conditionalInvocation,
            definitions,
            activeDefinitions,
          )
          if (bodyIndex === selectedBody) outerOptions = bodyOptions
        }
        updateOptions(functionScope, conditionalScope, caseScope, outerOptions)
        continue
      }
      for (const { name, pipeline, finalPipelineSegment } of functionCalls(text, definitions)) {
        const definition = definitions.get(name)
        const skippedFunctionInvocation =
          (previousSeparator === '&&' && /^\s*false\s*$/.test(previousText ?? '')) ||
          (previousSeparator === '||' && /^\s*(?:true|:)\s*$/.test(previousText ?? ''))
        if (skippedFunctionInvocation || activeDefinitions.has(name)) continue
        // Bash ignores errexit throughout a function invoked from an AND/OR list or a non-final
        // pipeline segment. A direct call, however, runs in the current shell, so option changes
        // made by its body carry forward.
        // `guardLogicalLines` splits a predicate list at `&&`/`||` when a preceding function
        // declaration shares its line. Retain the conditional context for the later segment.
        const predicateListInvocation =
          previousSeparator === '&&' && Boolean(conditionalOpener(previousText ?? ''))
        const conditionalInvocation =
          (pipeline && !finalPipelineSegment) ||
          nextSeparator === '&&' ||
          nextSeparator === '||' ||
          predicateListInvocation ||
          /^\s*!\s+/.test(text) ||
          scanGuardOperators(text).separators.some(
            ({ operator }) => operator === '&&' || operator === '||',
          )
        const functionOptions = inspect(
          definition.body,
          lineOffset + definition.lineOffset,
          lineOptions.errexit && !conditionalInvocation,
          enclosingContinues || hasLaterCommand,
          lineOptions.inheritErrexit,
          lineOptions.pipefail,
          lineErrexitIgnored || conditionalInvocation,
          definitions,
          new Set([...activeDefinitions, name]),
        )
        // A function on an AND/OR RHS may never run, and a pipeline or backgrounded command runs
        // in a child shell.
        // Literal predecessors are the exception: `true && fn` and `false || fn` definitely invoke
        // the function in this shell, so its option changes persist for later commands.
        const guaranteedInvocation =
          !previousSeparator ||
          previousSeparator === ';' ||
          (previousSeparator === '&&' && /^\s*(?:true|:)\s*$/.test(previousText ?? '')) ||
          (previousSeparator === '||' && /^\s*false\s*$/.test(previousText ?? ''))
        if (guaranteedInvocation && !pipeline && nextSeparator !== '&') {
          lineOptions = functionOptions
          updateOptions(functionScope, conditionalScope, caseScope, functionOptions)
        }
      }
      for (const inner of substitutions) {
        const innerOptions = shellOptionStateAfter(inner.prefix, lineOptions)
        const outerAssignmentTerminates = substitutionAssignmentTerminates(
          inner.prefix,
          inner.suffix,
        )
        const outerAssignmentIsStatusTested = scanGuardOperators(inner.suffix).separators.some(
          ({ operator }) => operator === '&&' || operator === '||',
        )
        inspect(
          inner.body,
          lineOffset + localOffset,
          // Bash clears `-e` on entry to a command substitution unless inherit_errexit is enabled.
          // A `set -e` in the substitution itself is tracked while inspecting that substitution.
          innerOptions.errexit && innerOptions.inheritErrexit,
          enclosingContinues ||
            (!outerAssignmentTerminates && hasLaterCommand) ||
            commandMasksSubstitution(inner.prefix) ||
            substitutionIsFollowedInSameCommand(inner.suffix) ||
            substitutionFallsThrough(inner.suffix),
          innerOptions.inheritErrexit,
          innerOptions.pipefail,
          lineErrexitIgnored || outerAssignmentIsStatusTested,
          definitions,
          activeDefinitions,
        )
      }
      if (
        bareGuardFallsThrough(
          text,
          lineErrexitIgnored ? { ...lineOptions, errexit: false } : lineOptions,
          hasLaterCommand,
          enclosingContinues,
          Boolean(caseScope),
          lineErrexitIgnored,
        )
      ) {
        findings.push({ lineOffset: lineOffset + localOffset, guard: text.trim() })
      }
      for (const { at, start } of operators) {
        const left = text.slice(start, at)
        const right = text.slice(at + 2)
        if (!guardCommand(left) && !/^\s*(?:\{|exit\b|return\b)/.test(right)) continue
        if (isDocumentationPlaceholderFallback(right)) continue
        if (fallbackAssignsValue(right)) continue
        const guardOptions = shellOptionStateAfter(text.slice(0, start), lineOptions)
        const fallbackTerminates =
          branchReachesExit(right) ||
          (guardOptions.errexit && !lineErrexitIgnored && branchFailsUnderErrexit(right))
        if (!fallbackTerminates || enclosingContinues)
          findings.push({ lineOffset: lineOffset + localOffset, guard: text.trim() })
      }
      if (closedConditional?.closer === 'fi' && closedConditional.selectedBranch !== null) {
        const selectedScope = [
          ...(conditionalScope ? conditionalScope.split('/') : []),
          `${closedConditional.id}.${closedConditional.selectedBranch}`,
        ].join('/')
        const selected = scopedOptions.get(`${functionScope}|${selectedScope}|${caseScope}`)
        if (selected) lineOptions = selected.options
      }
      // A function declaration does not execute its body. Its `set`/`shopt` calls are scoped to
      // that function during analysis and cannot alter state for later top-level commands.
      // Conditional and case option changes stay scoped to their selected branch. They can affect
      // later commands in that branch, but must not leak after the compound command closes.
      const terminator = unconditionalTerminator(text)
      updateOptions(
        functionScope,
        conditionalScope,
        caseScope,
        shellOptionStateAfter(terminator ? text.slice(0, terminator.end) : text, lineOptions),
      )
      if (terminator) break
      // The lexical scope attached to a logical line is its scope at the line's start. When an
      // `else` or `;; next-pattern)` appears mid-line, option changes in its suffix belong to the
      // next branch/arm rather than that starting scope.
      const branchSuffix = conditionalBranchSuffix(text)
      if (branchSuffix && conditionalScope) {
        updateOptions(
          functionScope,
          incrementScopedBranch(conditionalScope),
          caseScope,
          shellOptionStateAfter(branchSuffix.suffix, lineOptions),
        )
      }
      const armSuffix = caseArmSuffix(text)
      if (armSuffix && caseScope) {
        const armOptions = shellOptionStateAfter(armSuffix.prefix, lineOptions)
        updateOptions(
          functionScope,
          conditionalScope,
          incrementScopedBranch(caseScope),
          shellOptionStateAfter(armSuffix.suffix, armOptions),
        )
      }
    }
    return options
  }
  inspect(masked, 0)
  return findings
}

// Quote- and escape-aware word splitter. Records, per word, whether it carries a glob character
// that the SHELL would expand — i.e. one that is neither quoted nor backslash-escaped. See (f).
//
// `state.quote` persists across logical lines for the same reason the heredoc scanner's does: a
// quoted string may span newlines, and its payload is literal. Tokenizing each line from a clean
// state reads that payload as commands and reports a `multi-glob` failure on a valid block.
// Append a substitution's text to `word`, tracking its own nesting and quoting in `state` so an
// unbalanced one RESUMES on the next line. `rm -f $(printf '%s\n'` … `prefix) a*.log b*.tmp` is one
// invocation split by a newline; ending the word at the newline leaves the second line as its own
// segment with no `rm` in it to check. Returns the index of the last character consumed.
function consumeSubstitution(line, from, word, state) {
  let i = from
  // Resuming one the previous line left open, or opening a fresh one? On a fresh open `from` is the
  // index of the `(`, whose body starts after it; on a resume the whole line from `from` is body.
  // Getting this wrong drops the first character of every resumed line.
  const resuming = state.substDepth > 0
  const innerStart = resuming ? from : from + 1
  state.substBody ??= ''
  let closed = false
  for (; i < line.length; i++) {
    const c = line[i]
    word.raw += c
    if (state.substQuote === "'") {
      if (c === "'") state.substQuote = null
      continue
    }
    if (c === '\\' && i + 1 < line.length) {
      word.raw += line[++i]
      continue
    }
    if (state.substQuote === '"') {
      if (c === '"') state.substQuote = null
      continue
    }
    if (c === "'" || c === '"') {
      state.substQuote = c
      continue
    }
    if (c === '(') state.substDepth += 1
    else if (c === ')' && --state.substDepth === 0) {
      closed = true
      break
    }
  }
  // The body of a CLOSED substitution is its own command list. Pathname expansion happens inside the
  // subshell too, so `result=$(rm -f a*.log b*.tmp)` carries the same unmatched-glob hazard as the
  // bare form — consuming it as one opaque word analyzes neither. Recorded for separate analysis;
  // deliberately NOT folded into the outer word's globs, which the outer command never expands. Only
  // a substitution that closed on this line is recorded, so the recursion always sees a complete
  // body and can never manufacture a finding from a half-parsed one.
  // A substitution may span physical lines — `x=$(rm -f $(printf x` / `) a*.log b*.tmp)` is ONE `rm`
  // to bash, because the newline falls inside the nested substitution. Recording only the line the
  // outer one closes on discards the `rm` and the recursion sees two bare patterns, missing the
  // removal. So the body accumulates across lines and is only handed over once it closes.
  if (closed) {
    ;(word.subs ??= []).push(state.substBody + line.slice(innerStart, i))
    state.substBody = ''
  } else {
    state.substBody += `${line.slice(innerStart, i)}\n`
  }
  return i
}

// The legacy backtick form of the same thing. It does not nest, so a single flag suffices; carried
// in `state` so a substitution left open at end of line keeps the command and its globs in one word.
function consumeBacktick(line, from, word, state) {
  let i = from
  state.backtickBody ??= ''
  let closed = false
  for (; i < line.length; i++) {
    const c = line[i]
    word.raw += c
    if (c === '\\' && i + 1 < line.length) {
      word.raw += line[++i]
      continue
    }
    if (c === '`') {
      state.backtick = false
      closed = true
      break
    }
  }
  // Same multi-line accounting as `$(…)`: a backtick substitution left open carries to the next line.
  if (closed) {
    ;(word.subs ??= []).push(state.backtickBody + line.slice(from, i))
    state.backtickBody = ''
  } else {
    state.backtickBody = `${state.backtickBody ?? ''}${line.slice(from, i)}\n`
  }
  return i
}

function tokenizeShellLine(line, state = { quote: null, substDepth: 0, substQuote: null }) {
  state.substDepth ??= 0
  state.substQuote ??= null
  state.backtick ??= false
  // How many `${` are open around the character being read. Carried in `state`, like `substDepth`,
  // because an expansion is not ended by whitespace OR by a newline — `${…}` swallows both. Measured
  // on bash 5.3.9: `rm -f ${x#` + newline + `*} a*.log b*.tmp` is ONE `rm` receiving `-f`, the
  // expansion, and two pathname globs, and `echo ${x:-a;b}` / `${x:-a|b}` / `${x:-a(b}` all pass
  // `bash -n`. Resetting per word cost both directions: `rm -f ${x# *} a*.log` reported `*}` and
  // `a*.log` as two globs when bash expands only one (a FALSE POSITIVE), and the multi-line form
  // above was analyzed as two unrelated lines and found nothing.
  state.paramDepth ??= 0
  const tokens = []
  let current = null
  // A `[` only makes a word a glob once its `]` arrives, and only within the same word.
  let openBracket = false
  // The brace group being read, and how many `{` are open inside the current word. A group that
  // never closes before the word ends is literal text to bash, so it is simply dropped.
  let braceDepth = 0
  let braceOpen = null
  const flush = () => {
    if (current) tokens.push(current)
    current = null
    openBracket = false
    braceDepth = 0
    braceOpen = null
  }
  const start = () => {
    if (!current) current = { raw: '', glob: false, operator: false, globAt: [], braces: [] }
    return current
  }

  let quote = state.quote
  let startIndex = 0
  // Resume whatever the previous line left open: a substitution mid-word, or a quoted string.
  if (state.substDepth > 0) startIndex = consumeSubstitution(line, 0, start(), state) + 1
  else if (state.backtick) startIndex = consumeBacktick(line, 0, start(), state) + 1
  else if (quote) start()
  // An expansion still open from the previous line means this line is its continuation, not a fresh
  // command: opening the word here keeps `#` from being read as a comment introducer inside it.
  else if (state.paramDepth > 0) start()
  for (let i = startIndex; i < line.length; i++) {
    const ch = line[i]

    if (quote === "'") {
      current.raw += ch
      if (ch === "'") quote = null
      continue
    }
    if (quote === '"') {
      current.raw += ch
      if (ch === '\\' && i + 1 < line.length) {
        current.raw += line[++i]
        continue
      }
      // A substitution resumes command parsing even INSIDE double quotes, and the outer quotes do
      // not suppress pathname expansion within it: `result="$(rm -f a*.log b*.tmp)"` carries the
      // same unmatched-glob abort as the bare form. Consuming the whole quoted span first hid it.
      if (ch === '$' && line[i + 1] === '(') {
        i = consumeSubstitution(line, i + 1, current, state)
        continue
      }
      if (ch === '`') {
        state.backtick = true
        i = consumeBacktick(line, i + 1, current, state)
        continue
      }
      if (ch === '"') quote = null
      continue
    }

    if (ch === '\\') {
      start().raw += ch
      if (i + 1 < line.length) current.raw += line[++i]
      continue
    }
    if (ch === "'" || ch === '"') {
      start().raw += ch
      quote = ch
      continue
    }
    // Whitespace ends a word only OUTSIDE an expansion. `${x# *}` is a single word to bash, so
    // splitting on the space handed the pattern's `*` to the glob rule as a word of its own.
    if ((ch === ' ' || ch === '\t') && state.paramDepth === 0) {
      flush()
      continue
    }
    if (ch === '#' && current === null) break // comment runs to end of line
    // `$(…)`, and likewise the process substitutions `<(…)` / `>(…)`, are part of the surrounding
    // WORD rather than top-level operators — the shell passes each as one argument to the same
    // command. Splitting on their parens would end the `rm` segment of
    // `rm -f $(prefix) a*.log b*.tmp` before either glob, hiding the very multi-glob form the rule
    // exists to catch. Consumed whole, nesting- and quote-aware; glob characters INSIDE are not the
    // outer command's to expand, so they are deliberately not marked (a false negative, never a
    // false positive). A bare `(` stays a subshell operator.
    if (ch === '(' && current !== null && /[$<>]$/.test(current.raw)) {
      i = consumeSubstitution(line, i, current, state)
      continue
    }
    if (ch === '`') {
      start().raw += ch
      state.backtick = true
      i = consumeBacktick(line, i + 1, current, state)
      continue
    }
    // Legacy `$[…]` arithmetic: its `*` is multiplication, not a pathname pattern. Consumed without
    // glob accounting, exactly as `$((…))` already is by the substitution branch above.
    if (ch === '[' && current !== null && current.raw.endsWith('$')) {
      let depth = 0
      for (; i < line.length; i++) {
        current.raw += line[i]
        if (line[i] === '[') depth += 1
        else if (line[i] === ']' && --depth === 0) break
      }
      continue
    }
    // Same containment as whitespace: these are ordinary characters inside an expansion, where
    // `${x:-a;b}`, `${x:-a|b}` and `${x:-a(b}` all pass `bash -n`. Emitting an operator token there
    // would split one word into segments and let a later one be read as a command word.
    const descriptorRedirection =
      ch === '&' && (line[i - 1] === '>' || line[i - 1] === '<' || line[i + 1] === '>')
    if (
      state.paramDepth === 0 &&
      !descriptorRedirection &&
      (ch === ';' || ch === '|' || ch === '&' || ch === '(' || ch === ')')
    ) {
      flush()
      let op = ch
      while ((ch === '|' || ch === '&' || ch === ';') && line[i + 1] === ch) op += line[++i]
      tokens.push({ raw: op, glob: false, operator: true })
      continue
    }
    start().raw += ch
    // Inside an unterminated `${…}` these characters belong to PARAMETER expansion — an array
    // subscript (`${a[*]}`) or a pattern operator (`${v#*}`) — and are never expanded against the
    // filesystem, so they cannot abort the line and must not be counted. A glob after the closing
    // `}` still is one.
    // The `$` must be unescaped to open one, and that is a question of backslash PARITY, not of the
    // single preceding character: in `rm -f \\${x[*]}` the first backslash escapes the second, so the
    // `$` is live and the `*` is an array subscript. A one-character lookbehind reads that even run
    // as an escape, counts the subscript as a glob, and rejects valid shell — a false positive, the
    // one direction this rule must never fail in. An ODD run means the `$` is literal.
    // Expansions NEST — `${x#${y}*}` is one expansion whose pattern contains another — so the depth
    // is counted rather than scanning back to the first `}`. Reading that inner brace as the closer
    // put the trailing `*` back at top level and counted it as a glob, rejecting valid shell.
    if (ch === '{' && /(?:^|[^\\])(?:\\\\)*\$$/.test(current.raw.slice(0, -1))) {
      state.paramDepth += 1
      continue
    }
    if (state.paramDepth > 0) {
      if (ch === '}') state.paramDepth -= 1
      continue
    }
    const idx = current.raw.length - 1
    if (ch === '*' || ch === '?') {
      current.glob = true
      current.globAt.push(idx)
    }
    // A bracket expression is a glob the shell expands, so an unmatched one aborts the line under
    // zsh/fish exactly as `*` does. Only a CLOSED `[…]` counts.
    else if (ch === '[') openBracket = true
    else if (ch === ']' && openBracket) {
      current.glob = true
      current.globAt.push(idx)
      openBracket = false
    }
    // Brace expansion runs BEFORE pathname expansion, so ONE word can produce several patterns.
    // Measured on bash 5.3.9 with `rm` stubbed on PATH and only `a1.log` on disk, `rm -f {a,b}*.log`
    // hands `rm` the argv `-f a1.log b*.log` — two pathname patterns, one of them unmatched — and
    // under zsh the same word aborts the whole removal with `no matches found: b*.log`, which is
    // precisely the failure (f) exists to catch. Counting the source TOKEN once let that form pass.
    // Only an unquoted, unescaped group holding a top-level comma expands: `{a}` stays literal to
    // bash, and `${x}` is parameter expansion. Both are already excluded here, because the quote,
    // escape and `paramDepth` branches above consumed every such character before this point.
    // Spans are recorded rather than expanded in place so the glob marks keep their offsets. A
    // NESTED group counts as one alternative and a `{1..3}` sequence as none — both undercount, the
    // false-NEGATIVE direction this rule is allowed to fail in.
    if (ch === '{') {
      if (braceDepth === 0) braceOpen = { start: idx, partStart: idx + 1, parts: [] }
      braceDepth += 1
    } else if (ch === ',' && braceDepth === 1) {
      braceOpen.parts.push([braceOpen.partStart, idx])
      braceOpen.partStart = idx + 1
    } else if (ch === '}' && braceDepth > 0) {
      braceDepth -= 1
      if (braceDepth === 0) {
        if (braceOpen.parts.length > 0) {
          braceOpen.parts.push([braceOpen.partStart, idx])
          current.braces.push({ start: braceOpen.start, end: idx, parts: braceOpen.parts })
        }
        braceOpen = null
      }
    }
  }
  flush()
  // A heredoc or here-string DELIMITER is not subject to pathname expansion. Measured on bash 5.3.9
  // with `rm` stubbed on PATH and `EOFxyz` on disk, the block `rm -f a*.log <<EOF*` / body / `EOF*`
  // passes `bash -n`, terminates on the LITERAL `EOF*`, and hands `rm` only `-f a1.log a2.log` — the
  // delimiter never matched `EOFxyz`. Counting its `*` reported that valid block as a two-glob
  // removal: a false positive, the one direction this rule must never fail in. `<`/`>` targets ARE
  // pathname-expanded and stay counted; only `<<`, `<<-` and `<<<` are exempt. The operator may
  // carry the word (`<<EOF*`) or precede it (`<< EOF*`), and may take an fd (`0<<EOF*`). Only the
  // glob MARK is cleared — the word stays in place, so segment structure is untouched.
  for (let k = 0; k < tokens.length; k++) {
    if (tokens[k].operator || !/^\d*<</.test(tokens[k].raw)) continue
    const delimiter = /^\d*<<[-<]?$/.test(tokens[k].raw) ? tokens[k + 1] : tokens[k]
    if (delimiter && !delimiter.operator) delimiter.glob = false
  }
  state.quote = quote
  return tokens
}

// Shell quote removal over one word, enough to recover the command NAME. bash resolves `\rm`,
// `'rm'`, `"rm"`, `"/bin/rm"` and `/bin/"rm"` all to the `rm` executable — measured with `rm`
// stubbed on PATH, `\rm -f a*.log b*.tmp` and `"rm" -f a*.log b*.tmp` print identical argv with both
// patterns expanded, and under zsh with `b*.tmp` unmatched the `\rm` form aborts with
// `no matches found` (status 1), the exact failure this rule exists to catch. Comparing the RAW
// token read those as `\rm` and `rm"` and let the removal through.
//
// Quote removal ONLY, never expansion: `$RM` and `"$RM"` both survive as `$RM`, which matches
// nothing. So this widens the rule by exactly the words whose LITERAL text spells a removal.
function removeQuotes(word) {
  let out = ''
  for (let i = 0; i < word.length; i++) {
    const ch = word[i]
    if (ch === '\\') {
      if (i + 1 < word.length) out += word[++i]
      continue
    }
    // `$'…'` is an ANSI-C quoted word, not an expansion: bash decodes its escapes and then removes
    // the quotes, so `$'rm' -f a*.log b*.tmp` runs the `rm` executable with BOTH patterns expanded —
    // measured with `rm` stubbed on PATH, argv comes back `[-f] [a1.log] [a2.log] [b1.tmp] [b2.tmp]`.
    // Stripping only the quotes left `$rm`, which matches nothing and reported none. Decoding is
    // reused rather than skipped so `$'\x72m'` resolves too. This is quote removal still: a bare
    // `$rm`, `${rm}` or `"$RM"` has no quote to remove and survives as itself, matching nothing.
    if (ch === '$' && word[i + 1] === "'") {
      let j = i + 2
      let body = ''
      while (j < word.length && word[j] !== "'") {
        // A backslash escapes the next character, the closing quote included: `$'a\'b'` is one word.
        if (word[j] === '\\' && j + 1 < word.length) body += word[j++]
        body += word[j++]
      }
      out += decodeAnsiC(body)
      // Leave `i` ON the closing quote (or past the end); the loop's own step clears it.
      i = j
      continue
    }
    // Single quotes are literal THROUGH backslashes: `'\rm'` names a command `\rm`, not `rm`.
    if (ch === "'") {
      while (++i < word.length && word[i] !== "'") out += word[i]
      continue
    }
    // Inside double quotes a backslash escapes only `$`, backtick, `"` and `\`; before anything else
    // bash KEEPS it (`"\rm"` is `\rm`). Dropping it unconditionally would turn a command named
    // `\rm` into a reported `rm` — a false positive.
    if (ch === '"') {
      while (++i < word.length && word[i] !== '"') {
        if (word[i] === '\\' && '$`"\\'.includes(word[i + 1] ?? '')) i += 1
        out += word[i]
      }
      continue
    }
    out += ch
  }
  return out
}

// The pathname patterns one WORD hands the command, after brace expansion. Usually the word itself;
// `{a,b}*.log` is two, and `{a*,b}.log` is one because only the first alternative carries a pattern.
// Returns the expanded words that are patterns, so a finding names what the shell would really try
// to match. A word carrying no glob at all contributes nothing, which is also what keeps the
// heredoc-delimiter exemption above (it clears `glob`) working unchanged.
function globPatterns(token) {
  if (!token.glob) return []
  const raw = token.raw
  const globAt = new Set(token.globAt || [])
  const span = (from, to) => {
    let glob = false
    for (let k = from; k < to; k++) if (globAt.has(k)) glob = true
    return { text: raw.slice(from, to), glob }
  }
  let words = [{ text: '', glob: false }]
  let cursor = 0
  for (const group of token.braces || []) {
    // A cartesian product is exponential in the number of groups. Past a sane bound the count is
    // already far over the threshold, so falling back to the source word only under-reports.
    if (words.length * group.parts.length > 64) return [raw]
    const lead = span(cursor, group.start)
    const next = []
    for (const word of words) {
      for (const [from, to] of group.parts) {
        const part = span(from, to)
        next.push({
          text: word.text + lead.text + part.text,
          glob: word.glob || lead.glob || part.glob,
        })
      }
    }
    words = next
    cursor = group.end + 1
  }
  const tail = span(cursor, raw.length)
  return words.filter((word) => word.glob || tail.glob).map((word) => word.text + tail.text)
}

function commandWordIndex(segment) {
  let i = 0
  let afterWrapper = false
  while (i < segment.length) {
    const raw = segment[i].raw
    if (LEADING_WORDS.has(raw) || /^[A-Za-z_][A-Za-z0-9_]*=/.test(raw)) {
      if (WRAPPER_WORDS.has(raw)) afterWrapper = true
      i += 1
      continue
    }
    // `function name { … }` is a DECLARATION, not an invocation: its command word lives in the body.
    // The keyword and the name it introduces are consumed together, after which the `{` is stepped
    // over as an ordinary leading word and the removal inside is reached. (The `name() { … }` form
    // already works — its parens are operators, so the body starts a fresh segment.)
    if (raw === 'function') {
      i += 2
      continue
    }
    // A redirection may precede the command word (`2>/dev/null rm -f a*.log b*.tmp`). When the
    // operator carries its target it is one word; when it does not, the target is the next word.
    if (/^\d*[<>]/.test(raw)) {
      i += /^\d*[<>]+$/.test(raw) ? 2 : 1
      continue
    }
    // Only a wrapper's OWN options are skipped; `rm`'s are the invocation this rule is looking at.
    // An option taking an ARGUMENT (`sudo -u ci rm …`) still hides the command: skipping one word
    // after every option would swallow the `rm` in `command -p rm`, and telling those apart needs a
    // per-wrapper option table. That case is an accepted false NEGATIVE, never a false positive.
    if (afterWrapper && raw.startsWith('-')) {
      i += 1
      continue
    }
    return i
  }
  return -1
}

const WRAPPER_OPTIONS_WITH_ARGUMENTS = new Map([
  ['env', new Set(['-u', '--unset', '-C', '--chdir', '-S', '--split-string'])],
  [
    'sudo',
    new Set([
      '-u',
      '--user',
      '-g',
      '--group',
      '-h',
      '--host',
      '-p',
      '--prompt',
      '-r',
      '--role',
      '-t',
      '--type',
      '-C',
      '--close-from',
      '-T',
      '--command-timeout',
      '-R',
      '--chroot',
      '-D',
      '--chdir',
    ]),
  ],
  ['exec', new Set(['-a'])],
])

function wrapperOptionConsumesNext(name, option) {
  const optionsWithArguments = WRAPPER_OPTIONS_WITH_ARGUMENTS.get(name)
  if (!optionsWithArguments) return false
  if (optionsWithArguments.has(option)) return true
  if (!option.startsWith('-') || option.startsWith('--') || option.length < 3) return false

  const shortOptions = option.slice(1)
  for (let i = 0; i < shortOptions.length; i += 1) {
    const shortOption = `-${shortOptions[i]}`
    if (!optionsWithArguments.has(shortOption)) continue
    // Remaining characters are this option's attached argument; only a trailing option consumes the
    // next shell word (`sudo -Eu root`, `env -iu NAME`).
    return i === shortOptions.length - 1
  }
  return false
}

// Resolve the command wrappers actually invoke. Unlike the glob rule's deliberately conservative
// resolver, this safety rule must step options that consume arguments (`env -u NAME gh …`) because
// command substitutions in later body fields execute before the wrapper starts.
function effectiveCommandWordIndex(segment) {
  let i = 0
  while (i < segment.length) {
    const raw = segment[i].raw
    const word = removeQuotes(raw)
    const name = word.split('/').pop()
    if (/^[A-Za-z_][A-Za-z0-9_]*=/.test(word)) {
      i += 1
      continue
    }
    if (name === 'function') {
      i += 2
      continue
    }
    if (/^\d*[<>]/.test(raw)) {
      i += /^\d*[<>]+$/.test(raw) ? 2 : 1
      continue
    }
    if (LEADING_WORDS.has(name) && !WRAPPER_WORDS.has(name) && name !== 'time') {
      i += 1
      continue
    }
    if (WRAPPER_WORDS.has(name) || name === 'time') {
      i += 1
      while (i < segment.length) {
        const option = removeQuotes(segment[i].raw)
        if (option === '--') {
          i += 1
          break
        }
        if (!option.startsWith('-')) break
        if (name === 'command' && (option === '-v' || option === '-V')) return -1
        // `env -S` splits its operand into the command and its arguments. A multi-word operand
        // must be tokenized by ghApiArgumentStart; with a one-word operand, outer tokens still
        // provide the command's arguments.
        if (name === 'env' && (option === '-S' || option === '--split-string')) {
          const splitOperand = segment[i + 1]?.raw || ''
          return /\s/.test(removeQuotes(splitOperand)) ? i : i + 1
        }
        // GNU env also accepts its split string attached to the option. Return the option word so
        // ghApiArgumentStart can tokenize its value into the effective command and arguments.
        if (name === 'env' && /^(?:--split-string=|-S.+)/.test(option)) return i
        i += wrapperOptionConsumesNext(name, option) ? 2 : 1
      }
      continue
    }
    return i
  }
  return -1
}

const GH_OPTIONS_WITH_ARGUMENTS = new Set(['--hostname', '--repo', '-R'])

// `gh` accepts global options before its subcommand (`gh --hostname github.com api …`). Keep this
// deliberately small and explicit: these are the global options that consume the next shell word,
// while other flags can be stepped one word at a time until the `api` subcommand is found.
function ghApiArgumentStart(segment) {
  const at = effectiveCommandWordIndex(segment)
  if (at === -1) return null

  const effectiveWord = removeQuotes(segment[at].raw)
  let splitString = null
  if (effectiveWord === '-S' || effectiveWord === '--split-string') {
    splitString = segment[at + 1]?.raw
  } else {
    const attached = /^(?:--split-string=|-S)(.+)$/.exec(segment[at].raw)
    if (attached) splitString = attached[1]
  }
  if (splitString) {
    const splitTokens = tokenizeShellLine(removeQuotes(splitString), {
      quote: null,
      substDepth: 0,
      substQuote: null,
      backtick: false,
      paramDepth: 0,
    })
    const splitGhApi = ghApiArgumentStart(splitTokens)
    if (!splitGhApi) return null
    return {
      ...splitGhApi,
      // Quotes inside the split string are parsed by env, not the outer shell. An inner single
      // quote therefore cannot suppress a substitution that the outer double-quoted operand has
      // already evaluated.
      splitStringInterpolation:
        splitGhApi.splitStringInterpolation || hasActiveShellInterpolation(splitString),
    }
  }
  if (effectiveWord.split('/').pop() !== 'gh') return null

  for (let i = at + 1; i < segment.length; i += 1) {
    const option = removeQuotes(segment[i].raw)
    if (option === 'api') return { segment, argsAt: i + 1 }
    if (!option.startsWith('-') || option === '--') return null
    if (GH_OPTIONS_WITH_ARGUMENTS.has(option)) {
      i += 1
      continue
    }
    if (/^(?:--hostname|--repo)=/.test(option) || /^-R.+/.test(option)) continue
  }
  return null
}

// `gh api` accepts `-i` (include) alongside `-f`/`-F`, including the compact
// `-ifbody=…` spelling. Keep the accepted prefix intentionally narrow: `i` is
// the API subcommand's boolean short option, whereas an arbitrary short-option
// cluster could put an argument-taking flag before `f` and make its suffix an
// argument rather than a body field.
function ghClusteredBodyField(option) {
  return /^-i+([fF])(?:(body=.*))?$/.exec(option)
}

// `gh api -F body=@- <file` receives its body from the redirected file rather than a heredoc.
// Return the shell word naming that file so the file-body checker can trace it back to its producer.
function stdinRedirectionSource(segment) {
  for (let i = 0; i < segment.length; i += 1) {
    const raw = segment[i].raw
    const attached = /^(?:0)?<(?!(?:<|\())(.+)$/.exec(raw)
    if (attached) return attached[1]
    if (/^(?:0)?<$/.test(raw)) return segment[i + 1]?.raw ?? null
  }
  return null
}

// A `<<` only supplies stdin to the command in its own command segment, and
// only when it redirects fd 0 (implicit or explicit). A heredoc on `cat` in
// `cat <<EOF; gh api … body=@-` — or `3<<EOF` on gh itself — must not make the
// gh stdin-body form unsafe.
function segmentHasStdinHeredoc(segment) {
  return segment.some(({ raw, operator }) => !operator && /^(?:0)?<<-?(?!<)/.test(raw))
}

/**
 * Report `rm`/`find` invocations carrying more than one UNQUOTED glob. Returns
 * [{ lineOffset, command, globs }] with `lineOffset` the 0-based index of the first physical line
 * of the (continuation-joined) logical line within `body`.
 */
export function findMultiGlobRemovals(body) {
  const findings = []
  // Carried across logical lines; heredoc bodies were already blanked, so their literal text — not
  // shell — cannot desynchronise it.
  const state = { quote: null, substDepth: 0, substQuote: null, backtick: false, paramDepth: 0 }

  const analyze = (tokens, lineOffset) => {
    let segment = []
    const segments = []
    for (const token of tokens) {
      if (token.operator) {
        segments.push(segment)
        segment = []
        continue
      }
      segment.push(token)
    }
    segments.push(segment)

    for (const seg of segments) {
      const at = commandWordIndex(seg)
      if (at === -1) continue
      // Quote removal comes BEFORE the basename split: on the raw token `"/bin/rm"` popped to `rm"`
      // and was silently skipped.
      const command = removeQuotes(seg[at].raw).split('/').pop()
      if (command !== 'rm' && command !== 'find') continue
      // Every unquoted pattern counts, INCLUDING one spelled like an option. `rm -f -- -* a*.tmp`
      // aborts under zsh on `-*` (`no matches found`) exactly as any other unmatched pattern does —
      // the abort is pathname expansion failing, and the shell has no idea the word looks like a
      // flag. Skipping leading-`-` words hid that whole class. Options that carry no glob character
      // are still not globs, so they never reach here anyway.
      //
      const globs = seg.slice(at + 1).flatMap(globPatterns)
      if (globs.length > 1) findings.push({ lineOffset, command, globs })
    }

    // Then the command lists nested inside substitutions, which the tokenizer holds as opaque words.
    // Reported against the line the substitution opens on, since that is where a reader must look.
    // Recursing re-enters this same function, so an `rm` nested several levels down is still found.
    for (const token of tokens) {
      for (const inner of token.subs || []) {
        for (const nested of findMultiGlobRemovals(inner)) findings.push({ ...nested, lineOffset })
      }
    }
  }

  // A word still open at end of line — an unbalanced substitution, a quoted string, or an unclosed
  // `${…}` — means the command and its arguments span the newline. Hold the tokens until it closes
  // so the invocation is analyzed whole, and report against the line the command word sits on.
  let pending = null
  for (const { text, lineOffset } of joinContinuations(maskHeredocBodies(body.split('\n')))) {
    const tokens = tokenizeShellLine(text, state)
    if (pending) pending.tokens.push(...tokens)
    else pending = { tokens, lineOffset }
    if (state.quote || state.substDepth > 0 || state.backtick || state.paramDepth > 0) continue
    analyze(pending.tokens, pending.lineOffset)
    pending = null
  }
  if (pending) analyze(pending.tokens, pending.lineOffset)
  return findings
}

// True when a backtick, `$(`, or process substitution remains active after shell quoting.
// Single/ANSI-C quotes and an escaping backslash make those bytes literal. Double quotes still
// allow backticks and `$()`, but leave `<()` / `>()` as literal text.
function hasActiveShellInterpolation(raw) {
  let quote = null
  for (let i = 0; i < raw.length; i++) {
    const char = raw[i]
    if (quote === 'single') {
      if (char === "'") quote = null
      continue
    }
    if (quote === 'ansi') {
      if (char === '\\') i += 1
      else if (char === "'") quote = null
      continue
    }
    if (char === '\\') {
      i += 1
      continue
    }
    if (quote === 'double') {
      if (char === '"') quote = null
      else if (char === '`' || (char === '$' && raw[i + 1] === '(')) return true
      continue
    }
    if (char === "'") quote = 'single'
    else if (char === '$' && raw[i + 1] === "'") {
      quote = 'ansi'
      i += 1
    } else if (char === '"') quote = 'double'
    else if (
      char === '`' ||
      (char === '$' && raw[i + 1] === '(') ||
      ((char === '<' || char === '>') && raw[i + 1] === '(')
    ) {
      return true
    }
  }
  return false
}

/**
 * Report unsafe inline `gh api` body fields as [{ lineOffset, option }]. Markdown containing a
 * backtick or `$(...)` must reach GitHub through a file (or shell-literal quotes), never expansion.
 * `-f`/`--raw-field` do not dereference `@file`, so reject that form too.
 */
export function findUnsafeGhBody(body) {
  const findings = []
  const state = { quote: null, substDepth: 0, substQuote: null, backtick: false, paramDepth: 0 }

  const analyze = (tokens, lineOffset) => {
    let segment = []
    let pipedStdinHeredoc = false
    const segments = []
    const addSegment = () => {
      const stdinHeredoc = pipedStdinHeredoc || segmentHasStdinHeredoc(segment)
      segments.push({ segment, stdinHeredoc })
      pipedStdinHeredoc = stdinHeredoc
    }
    for (const token of tokens) {
      if (token.operator) {
        // `>|` is noclobber redirection, not a pipeline. The tokenizer keeps shell operators
        // deliberately small and emits its `|` separately, so restore the compound redirection.
        if (token.raw === '|' && segment.at(-1)?.raw === '>') {
          segment.at(-1).raw = '>|'
          continue
        }
        addSegment()
        segment = []
        if (token.raw !== '|') {
          pipedStdinHeredoc = false
        }
      } else {
        segment.push(token)
      }
    }
    addSegment()

    for (const { segment, stdinHeredoc } of segments) {
      const ghApi = ghApiArgumentStart(segment)
      if (!ghApi) continue
      for (let i = ghApi.argsAt; i < ghApi.segment.length; i++) {
        const rawOption = ghApi.segment[i].raw
        const option = removeQuotes(rawOption)
        let field = null
        let normalizedOption = null
        let combinedField = false
        let combinedPrefix = null
        if (
          option === '-f' ||
          option === '-F' ||
          option === '--field' ||
          option === '--raw-field'
        ) {
          field = ghApi.segment[++i]?.raw
          normalizedOption = option
        } else {
          const combined =
            /^(--field|--raw-field)=(body=.*)$/.exec(option) || /^(-f|-F)=?(body=.*)$/.exec(option)
          if (combined) {
            normalizedOption = combined[1]
            field = rawOption
            combinedField = true
            combinedPrefix = /^(?:--field|--raw-field|-[fF])=?/
          } else {
            const clustered = ghClusteredBodyField(option)
            if (!clustered) continue
            normalizedOption = `-${clustered[1]}`
            if (clustered[2]) {
              field = rawOption
              combinedField = true
              combinedPrefix = /^-i+[fF]/
            } else {
              field = ghApi.segment[++i]?.raw
            }
          }
        }
        if (!field) continue
        const normalizedField = removeQuotes(field)
        const normalizedBodyField = combinedField
          ? normalizedField.replace(combinedPrefix, '')
          : normalizedField
        if (
          (combinedField || normalizedField.startsWith('body=')) &&
          (hasActiveShellInterpolation(field) || ghApi.splitStringInterpolation)
        ) {
          findings.push({ lineOffset, option: normalizedOption })
        }
        if (
          (normalizedOption === '-f' || normalizedOption === '--raw-field') &&
          normalizedBodyField.startsWith('body=@')
        ) {
          findings.push({ lineOffset, option: normalizedOption, reason: 'raw-file' })
        }
        if (
          (normalizedOption === '-F' || normalizedOption === '--field') &&
          normalizedBodyField === 'body=@-' &&
          stdinHeredoc
        ) {
          findings.push({ lineOffset, option: normalizedOption })
        }
      }
    }

    // Substitutions are opaque words to the outer command, but their contents are command lists
    // with their own `gh api` invocations. The outer line offset is the first physical line of the
    // accumulated command, and the recursive result is relative to the substitution body.
    for (const token of tokens) {
      for (const inner of token.subs || []) {
        for (const nested of findUnsafeGhBody(inner)) {
          findings.push({ ...nested, lineOffset: lineOffset + nested.lineOffset })
        }
      }
    }
  }

  // Preserve a physical newline inside an open shell word. Tokenizing each line separately would
  // split a multi-line quoted body into unrelated tokens and hide substitutions after line one.
  let pending = null
  for (const { text, lineOffset } of joinContinuations(maskHeredocBodies(body.split('\n')))) {
    const tokens = tokenizeShellLine(text, state)
    if (pending) {
      pending.text += `\n${text}`
    } else if (state.quote || state.substDepth > 0 || state.backtick || state.paramDepth > 0) {
      pending = { text, lineOffset }
    }

    if (pending) {
      if (state.quote || state.substDepth > 0 || state.backtick || state.paramDepth > 0) continue
      const completeState = {
        quote: null,
        substDepth: 0,
        substQuote: null,
        backtick: false,
        paramDepth: 0,
      }
      analyze(tokenizeShellLine(pending.text, completeState), pending.lineOffset)
      pending = null
    } else {
      analyze(tokens, lineOffset)
    }
  }
  if (pending) {
    const pendingState = {
      quote: null,
      substDepth: 0,
      substQuote: null,
      backtick: false,
      paramDepth: 0,
    }
    analyze(tokenizeShellLine(pending.text, pendingState), pending.lineOffset)
  }
  return findings
}

/**
 * Report heredocs that write a file later supplied as `gh api -F body=@...`.
 *
 * `@file` protects GitHub submission, not the command that produced the file: an unquoted
 * heredoc expands Markdown's backticks and `$(...)`, while any fixed delimiter can collide with
 * a line in the reply. Restrict this to files actually used as a `gh api` body file so ordinary
 * shell documentation keeps its existing heredoc affordances.
 */
export function findUnsafeGhFileBodyWrites(body) {
  const bodyFiles = new Map()
  const addBodyFile = (bodyFile) => {
    const key = bodyFile.variable ? `variable:${bodyFile.variable}` : `path:${bodyFile.path}`
    bodyFiles.set(key, bodyFile)
  }
  const scan = (source) => {
    const scanState = {
      quote: null,
      substDepth: 0,
      substQuote: null,
      backtick: false,
      paramDepth: 0,
    }

    for (const { text } of joinContinuations(maskHeredocBodies(source.split('\n')))) {
      const tokens = tokenizeShellLine(text, scanState)
      collectBodyFiles(tokens)
    }
  }

  const collectBodyFiles = (tokens) => {
    let segment = []
    const segments = []
    for (const token of tokens) {
      if (token.operator) {
        segments.push(segment)
        segment = []
      } else {
        segment.push(token)
      }
    }
    segments.push(segment)

    for (const segment of segments) {
      const ghApi = ghApiArgumentStart(segment)
      if (!ghApi) continue
      for (let i = ghApi.argsAt; i < ghApi.segment.length; i++) {
        const option = removeQuotes(ghApi.segment[i].raw)
        let field = null
        let acceptsFile = false
        if (
          option === '-f' ||
          option === '-F' ||
          option === '--field' ||
          option === '--raw-field'
        ) {
          field = ghApi.segment[++i]?.raw
          acceptsFile = option === '-F' || option === '--field'
        } else {
          const combined =
            /^(--field|--raw-field)=(body=@.+)$/.exec(option) ||
            /^(-f|-F)=?(body=@.+)$/.exec(option)
          if (combined) {
            acceptsFile = combined[1] === '--field' || combined[1] === '-F'
            field = combined[2]
          } else {
            const clustered = ghClusteredBodyField(option)
            if (clustered) {
              acceptsFile = clustered[1] === 'F'
              field = clustered[2] || ghApi.segment[++i]?.raw
            }
          }
        }
        if (!acceptsFile || !field) continue
        const normalizedField = removeQuotes(field).replace(
          /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g,
          (_match, variable) => `$${variable}`,
        )
        const stdinSource =
          normalizedField === 'body=@-' ? stdinRedirectionSource(ghApi.segment) : null
        const bodySource = stdinSource ? `body=@${removeQuotes(stdinSource)}` : normalizedField
        const match = /^body=@\$([A-Za-z_][A-Za-z0-9_]*)$/.exec(bodySource)
        if (match) {
          addBodyFile({ variable: match[1] })
          continue
        }
        // Quotes are removed before comparing a shell word to its destination, so a quoted
        // pathname may contain spaces or simple variable expansions. Command substitutions stay
        // excluded because their runtime path cannot be correlated statically.
        const pathname =
          /^body=@((?:[^$`]|\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*(?::-[^$`{}]*)?\})+)$/.exec(
            bodySource,
          )
        if (pathname) addBodyFile({ path: pathname[1] })
      }
    }

    // Command substitutions are opaque to their enclosing command, but they can contain an
    // independent `gh api -F body=@...` invocation. Scan them with their own shell state so a
    // heredoc producer elsewhere in the fenced block is associated with that body file too.
    for (const token of tokens) {
      for (const inner of token.subs || []) scan(inner)
    }
  }

  scan(body)

  // A heredoc only produces the output of its own command, or of a command later in the same
  // pipeline. `cat <<EOF; printf ... >"$REPLY_BODY"` is safe: the later redirection is a separate
  // command, even though both tokens share a physical line. Conversely, keep `cat <<EOF | tee
  // "$REPLY_BODY"` unsafe because the pipeline carries the heredoc's output into the body file.
  const pipelineGroups = (tokens) => {
    const groups = []
    let group = []
    let segment = []
    for (const token of tokens) {
      if (!token.operator) {
        segment.push(token)
        continue
      }
      // See the matching normalization in `findUnsafeGhBody`: `>|` is a redirection, not a pipe.
      if (token.raw === '|' && segment.at(-1)?.raw === '>') {
        segment.at(-1).raw = '>|'
        continue
      }
      group.push(segment)
      segment = []
      if (token.raw !== '|') {
        groups.push(group)
        group = []
      }
    }
    group.push(segment)
    groups.push(group)
    return groups
  }

  const findings = []
  const teeWritesBodyFile = (segment, bodyFile) => {
    const commandAt = effectiveCommandWordIndex(segment)
    if (commandAt === -1 || removeQuotes(segment[commandAt].raw).split('/').pop() !== 'tee') {
      return false
    }

    let options = true
    for (let i = commandAt + 1; i < segment.length; i += 1) {
      const destination = removeQuotes(segment[i].raw)
      if (options && destination === '--') {
        options = false
        continue
      }
      if (options && destination.startsWith('-')) continue
      if (
        bodyFile.variable ? destination === `$${bodyFile.variable}` : destination === bodyFile.path
      ) {
        return true
      }
    }
    return false
  }
  const ddWritesBodyFile = (segment, bodyFile) => {
    const commandAt = effectiveCommandWordIndex(segment)
    if (commandAt === -1 || removeQuotes(segment[commandAt].raw).split('/').pop() !== 'dd') {
      return false
    }

    for (let i = commandAt + 1; i < segment.length; i += 1) {
      const match = /^of=(.*)$/.exec(removeQuotes(segment[i].raw))
      if (!match) continue
      const destination = match[1]
      if (
        bodyFile.variable ? destination === `$${bodyFile.variable}` : destination === bodyFile.path
      ) {
        return true
      }
    }
    return false
  }
  // Compound-command groups can span logical lines. Keep their state here rather than recreating
  // it for each line: an unquoted heredoc inside `{ ... }` or `( ... )` still produces the file
  // when the closing group redirects to it later. Track the heredoc line so a finding points to
  // the unsafe producer, not its redirection.
  const collectUnsafeWrites = (source, parentLineOffset = 0) => {
    const compoundGroups = []
    const redirectedCompoundGroups = (tokens, lineOffset) => {
      const redirected = []
      for (let i = 0; i < tokens.length; i += 1) {
        const token = tokens[i]
        const atCommandBoundary =
          i === 0 || tokens[i - 1].operator || ['then', 'else', 'do'].includes(tokens[i - 1].raw)
        const groupOpen =
          (!token.operator && token.raw === '{') ||
          (token.operator && token.raw === '(') ||
          (!token.operator && token.raw === 'if' && atCommandBoundary)
        const groupClose =
          (!token.operator && token.raw === '}') ||
          (token.operator && token.raw === ')') ||
          (!token.operator && token.raw === 'fi' && atCommandBoundary)
        if (groupOpen) {
          compoundGroups.push({
            close: token.raw === '{' ? '}' : token.raw === 'if' ? 'fi' : ')',
            heredocLineOffset: null,
          })
          continue
        }
        if (!token.operator && segmentHasStdinHeredoc([token])) {
          for (const group of compoundGroups) group.heredocLineOffset ??= lineOffset
          continue
        }
        if (groupClose && compoundGroups.length > 0 && compoundGroups.at(-1).close === token.raw) {
          const group = compoundGroups.pop()
          if (group.heredocLineOffset === null) continue
          const downstream = []
          for (let j = i + 1; j < tokens.length; j += 1) {
            if (tokens[j].operator && tokens[j].raw !== '|') break
            downstream.push(tokens[j])
          }
          redirected.push({
            lineOffset: group.heredocLineOffset,
            downstream,
          })
        }
      }
      return redirected
    }
    const tokenState = {
      quote: null,
      substDepth: 0,
      substQuote: null,
      backtick: false,
      paramDepth: 0,
    }
    let substitutionStartLineOffset = null
    for (const { text, lineOffset } of joinContinuations(maskHeredocBodies(source.split('\n')))) {
      const absoluteLineOffset = parentLineOffset + lineOffset
      const startsSubstitution = tokenState.substDepth === 0
      const normalizedText = text.replace(
        /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g,
        (_match, variable) => `$${variable}`,
      )
      const normalizedTokens = tokenizeShellLine(normalizedText, tokenState)
      if (startsSubstitution && tokenState.substDepth > 0) {
        substitutionStartLineOffset = absoluteLineOffset
      }
      const redirectedGroups = redirectedCompoundGroups(normalizedTokens, absoluteLineOffset)
      for (const bodyFile of bodyFiles.values()) {
        const target = bodyFile.variable
          ? new RegExp(
              String.raw`>\|?\s*(?:"\$${bodyFile.variable}"|\$${bodyFile.variable})(?=\s|$)`,
            )
          : new RegExp(
              String.raw`>\|?\s*["']*${bodyFile.path
                .split('')
                .map((character) => character.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
                .join(String.raw`["']*`)}["']*(?=\s|$)`,
            )
        for (const group of pipelineGroups(normalizedTokens)) {
          // A brace group may carry a heredoc from an earlier logical line. Its closing redirection
          // has no local heredoc token, but `redirectedGroups` has already tied it to that producer.
          const heredocAt = group.findIndex(segmentHasStdinHeredoc)
          if (heredocAt === -1 && redirectedGroups.length === 0) continue
          // Pipelines only flow left to right. A destination before the heredoc cannot receive its
          // expansion, while a redirection or `tee` in the heredoc's command or a later one can.
          const downstream = heredocAt === -1 ? [] : group.slice(heredocAt)
          const downstreamText = downstream
            .flat()
            .map(({ raw }) => raw)
            .join(' ')
          const redirectedGroup = redirectedGroups.find((group) => {
            const downstreamText = group.downstream.map(({ raw }) => raw).join(' ')
            return (
              target.test(downstreamText) ||
              pipelineGroups(group.downstream).some((pipeline) =>
                pipeline.some(
                  (segment) =>
                    segment.length > 0 &&
                    (teeWritesBodyFile(segment, bodyFile) || ddWritesBodyFile(segment, bodyFile)),
                ),
              )
            )
          })
          if (
            (heredocAt !== -1 &&
              (target.test(downstreamText) ||
                downstream.some(
                  (segment) =>
                    teeWritesBodyFile(segment, bodyFile) || ddWritesBodyFile(segment, bodyFile),
                ))) ||
            redirectedGroup
          ) {
            findings.push({
              lineOffset: redirectedGroup?.lineOffset ?? absoluteLineOffset,
              ...bodyFile,
            })
            break
          }
        }
      }

      // A command substitution has its own command list and can write a body file through an
      // unquoted heredoc. Analyze it as a producer too, not only as a `gh api` consumer.
      for (const token of normalizedTokens) {
        for (const inner of token.subs || []) {
          collectUnsafeWrites(inner, substitutionStartLineOffset ?? absoluteLineOffset)
        }
      }
      if (tokenState.substDepth === 0) substitutionStartLineOffset = null
    }
  }
  collectUnsafeWrites(body)
  return findings
}

/** Every `**\/*.md` under a root. Strictly broader than a SKILL.md/references shape restriction. */
export function findSkillMarkdownFiles(root, deps = {}) {
  const fsImpl = deps.fs || fs
  const results = []
  const walk = (dir) => {
    for (const entry of fsImpl.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name)
      if (entry.isDirectory()) walk(full)
      else if (entry.name.endsWith('.md')) results.push(full)
    }
  }
  walk(root)
  return results.sort()
}

function defaultHasBash() {
  return !spawnSync('bash', ['-c', 'exit 0']).error
}

async function runPool(items, limit, worker) {
  const results = new Array(items.length)
  let next = 0
  const runners = Array.from({ length: Math.min(limit, items.length) }, async () => {
    for (;;) {
      const i = next++
      if (i >= items.length) return
      results[i] = await worker(items[i], i)
    }
  })
  await Promise.all(runners)
  return results
}

// Pinned invocation: plain `bash -n <file>`. See (e).
function makeBashRunner(tmpDir) {
  return (script, index) =>
    new Promise((resolve) => {
      const file = path.join(tmpDir, `block-${index}.sh`)
      fs.writeFileSync(file, `${script}\n`)
      execFile('bash', ['-n', file], (error, _stdout, stderr) => {
        const message = String(stderr || '')
          .split('\n')
          .map((l) => l.split(`${file}: `).join('').trim())
          .filter(Boolean)[0]
        resolve({ ok: !error, message: message || 'bash -n exited non-zero with no message' })
      })
    })
}

/**
 * Walk both authoring roots and return [{ file, line, kind, message }]. `kind` is one of
 * `unterminated` / `heredoc` / `syntax` / `multi-glob` / `inert-guard` /
 * `gh-body-interpolation` / `gh-body-file-interpolation`, plus `missing-bash` when the gate fails
 * closed.
 */
export async function checkSkillShellInRepo(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  const env = deps.env || process.env
  const warn = deps.warn || ((m) => process.stderr.write(`${m}\n`))
  const findings = []
  const pending = []
  const stats = { total: 0, plain: 0, blockquoted: 0, byRoot: {}, skipped: false }

  for (const root of SKILL_ROOTS) {
    const absRoot = path.join(repoRoot, root)
    stats.byRoot[root] = 0
    if (!fsImpl.existsSync(absRoot)) continue
    for (const file of findSkillMarkdownFiles(absRoot, deps)) {
      const rel = path.relative(repoRoot, file)
      for (const block of extractFencedBlocks(fsImpl.readFileSync(file, 'utf8'))) {
        if (!block.terminated) {
          findings.push({
            file: rel,
            line: block.startLine,
            kind: 'unterminated',
            message: `UNTERMINATED fence (opened at ${rel}:${block.startLine})`,
          })
          continue
        }
        if (!SHELL_INFO_STRINGS.has(block.info)) continue

        stats.total += 1
        stats.byRoot[root] += 1
        if (block.prefix.includes('>')) stats.blockquoted += 1
        else stats.plain += 1

        const openHeredoc = findUnterminatedHeredoc(block.body)
        if (openHeredoc) {
          findings.push({
            file: rel,
            line: block.startLine + 1 + openHeredoc.lineOffset,
            kind: 'heredoc',
            message: `here-document delimited by end-of-file (wanted \`${openHeredoc.delim}\`) — bash -n exits 0 for this on every version tested, and the rest of the block is swallowed as payload`,
          })
        }

        for (const glob of findMultiGlobRemovals(block.body)) {
          findings.push({
            file: rel,
            line: block.startLine + 1 + glob.lineOffset,
            kind: 'multi-glob',
            message: `${glob.command} carries ${glob.globs.length} unquoted globs on one line (${glob.globs.join(' ')}) — an unmatched glob aborts the whole line under zsh/fish, silently skipping the others`,
          })
        }

        for (const ghBody of findUnsafeGhBody(block.body)) {
          findings.push({
            file: rel,
            line: block.startLine + 1 + ghBody.lineOffset,
            kind: 'gh-body-interpolation',
            message:
              ghBody.reason === 'raw-file'
                ? `gh api ${ghBody.option} does not dereference body=@file; use -F body=@"$REPLY_BODY"`
                : `gh api ${ghBody.option} body= contains shell-interpolated Markdown; use a delimiter-free literal temp-file write and -F body=@"$REPLY_BODY"`,
          })
        }

        for (const fileWrite of findUnsafeGhFileBodyWrites(block.body)) {
          const fileLabel = fileWrite.variable ? `$${fileWrite.variable}` : fileWrite.path
          findings.push({
            file: rel,
            line: block.startLine + 1 + fileWrite.lineOffset,
            kind: 'gh-body-file-interpolation',
            message: `body file ${fileLabel} is written with a heredoc; use a delimiter-free literal write before -F body=@"${fileLabel}"`,
          })
        }

        for (const guard of findInertGuards(block.body)) {
          findings.push({
            file: rel,
            line: block.startLine + 1 + guard.lineOffset,
            kind: 'inert-guard',
            message: `${guard.guard} can fall through after failure; add set -e or exit/return in its failure branch`,
          })
        }

        pending.push({
          file: rel,
          line: block.startLine,
          script: normalizePlaceholders(block.body),
        })
      }
    }
  }

  const hasBash = deps.hasBash || defaultHasBash
  if (pending.length > 0 && !hasBash()) {
    const message = `check-skill-shell: bash not found on PATH — cannot verify ${pending.length} shell blocks`
    if (env.BOSS_SKILL_SHELL_OPTIONAL === '1') {
      // The opt-out must never let the caller print a line claiming the blocks were verified.
      stats.skipped = true
      if (deps.onStats) deps.onStats(stats)
      warn(`${message} (skipped: BOSS_SKILL_SHELL_OPTIONAL=1)`)
      // It waives `bash -n` ONLY. `unterminated` and `multi-glob` are decided without ever running
      // bash, so returning [] here would let an absent interpreter silently swallow findings the
      // gate had already made — the exact exit-0-with-a-real-defect class this gate exists for.
      return findings
    }
    if (deps.onStats) deps.onStats(stats)
    findings.push({ file: '', line: 0, kind: 'missing-bash', message })
    return findings
  }

  if (deps.onStats) deps.onStats(stats)

  if (pending.length > 0) {
    const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'check-skill-shell-'))
    const run = deps.runBashCheck || makeBashRunner(tmpDir)
    try {
      const results = await runPool(pending, deps.concurrency || CONCURRENCY, (item, i) =>
        run(item.script, i),
      )
      results.forEach((result, i) => {
        if (result.ok) return
        findings.push({
          file: pending[i].file,
          line: pending[i].line,
          kind: 'syntax',
          message: result.message,
        })
      })
    } finally {
      fs.rmSync(tmpDir, { recursive: true, force: true })
    }
  }

  findings.sort((a, b) => a.file.localeCompare(b.file) || a.line - b.line)
  return findings
}

async function main() {
  const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
  let stats = { total: 0, plain: 0, blockquoted: 0, byRoot: {}, skipped: false }
  const findings = await checkSkillShellInRepo(repoRoot, { onStats: (s) => (stats = s) })

  if (findings.length > 0) {
    console.error(
      'check-skill-shell: shell blocks in the skill tree failed the gate (bash parser, not POSIX sh):',
    )
    for (const f of findings) {
      const where = f.file ? `${f.file}:${f.line}` : 'check-skill-shell'
      console.error(`  - ${where} [${f.kind}] ${f.message}`)
    }
    process.exit(1)
  }

  const roots = SKILL_ROOTS.map((r) => `${r} ${stats.byRoot[r] ?? 0}`).join(', ')
  if (stats.skipped) {
    // Never claim verification that did not happen — that is the exact class this gate exists for.
    console.log(
      `check-skill-shell: ${stats.total} fenced shell blocks found but NOT verified ` +
        `(BOSS_SKILL_SHELL_OPTIONAL=1, bash absent) — ${roots}.`,
    )
    return
  }
  console.log(
    `check-skill-shell: ${stats.total} fenced shell blocks parse clean via bash -n ` +
      `(${stats.plain} plain, ${stats.blockquoted} blockquoted) — ${roots}. ` +
      'Bash parser, not POSIX sh validation.',
  )
}

if (isMainModule(import.meta.url)) await main()
