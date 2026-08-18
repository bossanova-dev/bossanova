# Authoring proof recipes

`default.json` is the shipped recipe catalog; `schema.json` validates it. This file is the part the
schema cannot carry: what each capture field **actually** captures, and the mistakes that look
perfectly valid right up until the artifact lands.

Every claim below is true of the code as it stands, and names the file it is true of. If you change
that code, change this file in the same commit.

## What each capture field actually captures

The still path is `buildSpec` in `scripts/proof-playwright-runner.mjs`. It branches three ways, and
the branches are checked in this order: `cropToSelector`, then `selector`, then plain viewport.

**Which path a recipe takes is decided by `capture`, and the default is `video`, not still.** The
defaulting is `normalizeRecipe`'s (`scripts/proof-lib.mjs`), not the runner's: it fills an absent
`capture` with `"video"` before the runner ever sees the recipe. `buildSpec` is then the sole entry
point, and its guard is an allowlist of one — it forwards `capture: "video"` to `buildVideoSpec` in
the same file and keeps everything else on its own still path. So a recipe reaching `buildSpec` with
`capture` unset takes the **still** path; in the real pipeline none does. `buildVideoSpec` honours a
different, smaller set of fields. Read the
[video path](#the-video-path-honours-a-different-field-set) section before applying anything below
to a recipe that has not opted out of video.

### `cropToSelector` — a full-width TOP SLICE, not a box crop

The screenshot runs from the viewport origin `(0,0)` down to `box.y + box.height + 24`, clamped to
the viewport height:

```js
const height = Math.min(size.height, Math.ceil(box.y + box.height + 24))
await page.screenshot({ path, clip: { x: 0, y: 0, width: size.width, height } })
```

`x` and `width` are never narrowed. So **everything above the element is in frame** — app header,
chrome, sub-header, filter bar — and the only thing trimmed is the empty space _below_ the element
that `flex: 1` layouts leave behind. `captureStill` in the same file does the same for the video
path's per-step stills.

Two consequences worth spelling out:

- A recipe `description` claiming the still is "cropped to the `<element>`" **overstates what the
  artifact shows**. Say "top slice down to the `<element>`" instead.
- The inverse error costs more. A plan step of the form _"the existing still crops to
  `[data-testid=X]`, so it cannot show the header — add a dedicated header still"_ is **false**, and
  obeying it adds a recipe whose output is pixel-identical to the one already there. Before adding a
  recipe on that premise, re-read the geometry above.

### `selector` — only that element, and nothing else is reachable

`buildSpec`'s `selector` branch calls `target.screenshot()` on the located element. Anything outside
the element's own box — site chrome, a Docusaurus sidebar, surrounding page furniture — is
**structurally unobtainable** while `selector` is set, and a run only discovers that after a full
capture cycle.

The remedy is to **drop `selector`**, which sends the recipe down the plain-viewport branch, not to
widen the selector to some ancestor. `cropToSelector` wins when both are present.

All of that is true **on the still path only**. `buildVideoSpec` never reads `recipe.selector` — the
field appears exactly once in the runner, at `buildSpec`'s argument unpacking — so on the default
`video` capture `selector` excludes nothing and the surrounding chrome is captured regardless. Most
committed recipes carrying `selector` are video recipes, so do not reach for "drop `selector` to get
the chrome back" without first checking the recipe's `capture`.

### `viewport` — the 1024px judge ceiling

Widening the viewport past 1024px makes the evidence **harder** to read, not easier:

- `JUDGE_MAX_PX = 1024` in `scripts/proof-judge.mjs`, and `downscaleArgs` downscales any still whose
  long edge exceeds it before the fresh-context judge sees it.
- The runner sets no `deviceScaleFactor` — `schema.json` allows only `width` and `height` on
  `viewport`, and `buildSpec` passes exactly those to `page.setViewportSize`.

So a 1440-wide strip reaches the judge at roughly 0.71x, and a 14px glyph lands at about 10px.
Widen only when the subject is genuinely large (a wide table, a multi-column layout). For
small-control evidence — a button, an icon-only affordance, a keyboard hint — keep the width at or
below 1024.

### `fullPage` and `canvas`

`fullPage` is consulted **only** on the still path's plain-viewport branch; `cropToSelector` and
`selector` both take precedence and ignore it (`buildSpec`,
`scripts/proof-playwright-runner.mjs`), and the video path ignores it too.

`canvas` changes no capture geometry at all — the runner never reads it. It is an authoring marker:
a DOM audit cannot see canvas pixels, so a guard in `scripts/proof-lib.test.mjs` requires every
`canvas: true` recipe **in the shipped catalog** to use `privacy: "fixture"`. That guard reads
`default.json`; a recipe supplied through a consumer `BOSS_PROOF_CATALOG` is not covered by it.

### `audit.txt` and what a token match means

Every capture writes an `audit.txt` beside it, harvested by `collectProofAuditText`
(`scripts/proof-playwright-runner.mjs`). Its source follows the branch:

| Capture / branch            | Audit source                                                 |
| --------------------------- | ------------------------------------------------------------ |
| still — `cropToSelector`    | `body`, **scoped to nodes whose top edge is above the clip** |
| still — `selector`          | the selected element only                                    |
| still — plain viewport      | the whole `body`                                             |
| video (the default capture) | the whole `body`, unscoped, at the end of the step sequence  |

The crop-branch scoping exists so that an `audit.txt` token match means "**the node carrying the
token had its top edge above the clip**". Without it a token can sit below the clip — present in the
text, absent from the image — and any evidence gate matching `audit.txt` passes on an artifact that
never showed the subject.

Take that wording at face value, because it is narrower than it first reads. It is a test on **one
node's top edge**, not on visibility and not on the token's own position: a node admitted by it can
still carry text that renders below the clip, and it says nothing about horizontal position or about
whether the node was rendered at all. The caveats below enumerate every such case. The scoping
narrows the vacuous-gate hole; it does not close it.

**The scoping is a still-path property only.** `buildVideoSpec` harvests the whole `<body>` even
when the recipe sets `cropToSelector`, so a token match in a video recipe's `audit.txt` still does
not prove the token was on screen. That is deliberate: a video recipe's poster is a full-viewport
screenshot and its synthesised step sequence scrolls the page, so the artifact is not a single clip
to scope against.

The scoping **fails open**: a node whose top edge cannot be resolved (a text node has no
`getBoundingClientRect`; its parent element's box is used instead) is **included**. Dropping
unpositioned text would silently shrink `audit.txt`.

Failing open keeps the text but not its contiguity: an **element** whose rect cannot be resolved at
all is descended into rather than taken whole, so a phrase spanning inline children arrives split.
The alternative — taking such a subtree whole — would drag in its resolvable, below-clip descendants
too, which is a worse trade. In practice this costs nothing: every DOM `Element` implements
`getBoundingClientRect`, so this path is defensive only, and the case that actually looks
"unpositioned" — `display: none`, an all-zero rect — measures as fully in frame and **is** taken
whole.

One interaction to know about if you ever add a `privacy: "live"` recipe: `scripts/proof.mjs` fails
a live browser capture whose `audit.txt` is blank. A live **still** recipe cropping to a text-free
top strip could now trip that where the whole-`<body>` harvest would not have. The shipped catalog
has no `privacy: "live"` recipe, so nothing is affected today.

**Scoping must never become a second cause of token loss.** An element that is **entirely** above
the cut-off — `rect.bottom <= height` — is taken **whole**, as one contiguous string, exactly as the
unscoped path takes it. Only a **straddling** element is walked text node by text node. Without
that, `<p>Rotation <strong>history</strong></p>` would land in `audit.txt` as two separate lines and
a `Rotation history` evidence token would miss **even though every glyph was in frame** — inviting
precisely the wrong diagnosis ("the token was below the clip, so the recipe was vacuous").

Five consequences of measuring per node rather than per glyph, all deliberate:

- **The test is vertical and geometric, never a visibility test.** A `display:none` element returns
  an all-zero rect, so `top` and `bottom` are both `0` and its whole subtree measures as fully in
  frame: an inactive `CommandTabs` panel, a closed modal, a collapsed menu and a hidden toast all
  still reach `audit.txt`. Nor is anything excluded horizontally (content scrolled off to the right,
  or clipped by `overflow-x`), or above the viewport top (a `top: -9999px` skip link has
  `top < height` and passes). None of this is a regression — the whole-`<body>` harvest included all
  of it too — but it is the part of the vacuous-gate hole this change does **not** close.

- A container whose own top edge is at or below the cut-off is skipped **whole**, so a child lifted
  above it by absolute positioning, a negative margin or a transform is dropped even though it is in
  frame. The fail-open rule above covers an unresolvable rect, not this case.
- The mirror of that: a container taken whole because its own box ends above the cut-off carries any
  absolutely-positioned descendant that overflows **below** the clip, since such a descendant does
  not extend the parent's `getBoundingClientRect()`. Rarer than the phrase splitting it buys back,
  and still far narrower than the whole-`<body>` harvest this replaced.
- An element straddling the clip contributes **all of its own direct** text, because a text node is
  measured through its `parentElement`. That includes a direct text node sitting below the clip
  after an intervening child block — `<div>above…<p>…</p>below…</div>`.
- The attribute pass over `input, textarea, select, option, img, [aria-label], [title], [alt]`
  contributes, under a cut-off, the attribute values and `value`/`placeholder` only — never the
  matched element's whole subtree. A labelled container routinely straddles the clip, so pushing its
  `textContent` there would put the below-clip half straight back in.

## The video path honours a different field set

`buildVideoSpec` is not `buildSpec` with a camera attached. Reading down the field list above and
applying it to a `capture: "video"` recipe is the single easiest way to author a recipe that looks
right and captures the wrong thing:

- `recipe.selector` is **never read** — that field appears once in the whole runner, in `buildSpec`.
- `recipe.fullPage` is **never read**.
- `steps[].selector` and `steps[].fullPage` are **different fields** and the video path does read
  both: a step's `selector` drives its `click`/`type`/`select`/`wait` target, and
  `fullPage: true` on a `scroll` step drives the whole-page scroll. Grepping for the bare words
  `selector` or `fullPage` in the runner therefore hits the step fields, not these two.
- `cropToSelector` **is** read, but only for the per-step stills (`captureStill`), and it falls back
  to the surface's `defaultCropToSelector` (`scripts/proof-surfaces.mjs`) when the recipe omits it.
  The poster PNG and the webm are always full-viewport.
- `audit.txt` is harvested from the whole `<body>`, unscoped — see the table above.

`normalizeRecipe` (`scripts/proof-lib.mjs`) also synthesises a `goto`/`wait`/`scroll` step sequence
for a route-only recipe, so a recipe with no `steps` still becomes a video.

## A `goto`-only recipe captures the DEFAULT tab of a `CommandTabs` block

The CLI tab carries the `default` flag in `services/docs/src/components/CommandTabs.tsx`. A docs
proof whose evidence is the Chat or MCP side of a documented command therefore needs the
`steps`/video recipe form with an explicit `click` on that tab — never a bare `route`. A `goto`-only
recipe for that evidence looks perfectly valid and silently captures the wrong tab.

## A recipe `description` must not promise text the chosen viewport hides

Check the owning CSS media queries before writing evidence tokens into a narrow-viewport recipe.
Real example: the `.kbd-hint` rule in `services/web/src/index.css` is `display: none` under a
`max-width: 640px` breakpoint, so a 390px-wide recipe can never render the hint tokens its own
description names.

No gate catches this. The schema does not cross-check a description against CSS, and the capture
succeeds — it just proves something other than what the description claims.

## A staged fixture must hold the same facts the real server holds

A fixture that answers every request with success renders the success state regardless of whether
the feature works. The capture then proves only that the page renders.

**Falsify every new recipe once.** Break the thing the recipe claims to prove, confirm the capture
visibly changes, then restore. A recipe that looks identical with its subject broken is not
evidence.

## Which recipe ids reach the parity snapshot

`computeProofSurfaceSignature` in `scripts/skill-parity/capture-baseline.mjs` records `tui` and
`web` as plain **booleans**, plus a `recipes` list carrying only **browser** (marketing/docs) surface
ids — `classifySurfaces` in `scripts/proof-surfaces.mjs` filters `web` out of that list by design.

So adding a **web** or **tui** recipe is expected **not** to drift
`scripts/skill-parity/baseline/proof-surface.snapshot.json`. Do not regenerate the baseline for one:
a green `node scripts/skill-parity/parity-gate.mjs` is the correct outcome. The same statement lives
in a comment beside `computeProofSurfaceSignature`; keep the two in sync.
