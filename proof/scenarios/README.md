# Proof TUI scenarios

Deterministic, per-PR TUI proof scenarios (`*.scenario.json`) — the machine-checkable
counterpart to the free-form proof brief. A scenario drives the TUI through named scenes,
each a sequence of input steps plus `expect` assertions the settled screen must satisfy.

**This directory ships only `schema.json` and this README.** It is deliberately **not a
catalog** of scenarios.

## The per-PR contract

- A scenario is authored by the implementing agent **for one PR** and gates **only that PR**.
- The directory is **not** scanned and old scenario files are **never re-run automatically** —
  they are history, not a suite. (The BOS-115 / #958 deletion behavior stands: superseded
  scenario files are removed, not accumulated.)
- There are **no path rules** here (unlike `proof/recipes/`): a scenario is selected by the
  authoring/replay flow, not matched against changed files.
- The authoring loop is `proof.mjs scenario validate` (arriving in BOS-219); this ticket ships
  only the schema, validator, loader, and shared matcher.

## Worked example

A full, valid scenario exercising every step kind and every expectation form lives in the test
fixtures, not here (anti-catalog invariant, D3):

    scripts/testdata/scenario-fixtures/valid-full.json

## Document shape (v1)

```jsonc
{
  "version": 1,
  "title": "Session settings: archive-delay field",
  "fixture": {
    // optional; shape-only (content owned by BOS-217)
    "preset": "demo", // non-empty string; loader defaults to "demo"
    "seed": { "sessions": [] }, // opaque object, passed through
    "env": { "BOSS_X": "1" }, // string -> string map
  },
  "scenes": [
    // 1..4
    {
      "id": "scene-01", // optional; loader synthesizes scene-NN
      "title": "Open session settings",
      "steps": [
        // >= 1; each step is EXACTLY ONE op key (+ optional caption)
        { "key": "down", "caption": "Move to the session" },
        { "key": "enter" },
        { "waitFor": "Session settings", "timeoutMs": 10000 },
        { "type": "45" },
        { "waitMs": 500 },
        { "daemon": { "action": "push_output", "sessionId": "sess-aaa-111" } },
        { "expect": ["Archive delay", { "text": "45 minutes", "match": "normalized" }] },
      ],
    },
  ],
}
```

Step op keys: `key` · `type` · `waitFor` (+ `timeoutMs` 1..60000, default 10000) · `waitMs`
(1..10000) · `daemon` (object with a non-empty string `action`; the rest passes through) ·
`expect` (an expectation or an array of them). `caption` is an optional string on any step.
`waitFor` takes a bare string (normalized-mode match) or an expectation object.

## Expectation / matcher semantics

An expectation is a bare string, an object, or an `anyOf` object:

- **bare string** ⇒ `{ text, match: "normalized" }` shorthand.
- **object** ⇒ `{ text, match?, label? }` with `match` one of
  `literal | normalized | normalized-ci | regex` (default `normalized`).
- **anyOf** ⇒ `{ anyOf: [...], label? }`; entries are strings or `{ text, match }`; the group
  passes if **any** alternative matches.

Matching rules:

- `normalized` (the default) collapses every whitespace run — including newlines — to a single
  space and trims both sides, so terminal wrapping and padding never break a match. It stays
  **case-sensitive**: whitespace is rendering noise, letter case is content.
- `normalized-ci` is the explicit case-insensitive opt-in (for LLM-authored expectations).
- `literal` is an exact substring match.
- `regex` compiles with the `u` flag and tests against the **raw** screen text (the author
  controls whitespace via `\s+`); an invalid pattern is rejected at load time.
- `label`, when present, is a non-empty display string. `displayText = label ?? text` (anyOf
  joins its alternatives with `|`) is what downstream consumers — the judge, the gallery, and
  the brief anchors — show, so they surface human labels rather than raw regex source.

## What this schema does NOT check

Validation is **shape-only**. The schema and validator do not enumerate legal terminal keys,
`daemon` actions, or the `preset` / `seed` / `env` vocabularies — those are opaque here and are
owned by their respective loaders (BOS-217 for fixtures, BOS-219 for the live key/daemon
bridge). Unknown keys or actions fail loudly later, at replay.
