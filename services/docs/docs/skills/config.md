---
title: Skill Configuration
description: How .boss-skills.json wires the boss-* skills to a repository, the smallest config that turns the tracker-driven skills on, and what every section controls.
slug: /skills/config
---

# Skill Configuration

The `boss-*` skills read `.boss-skills.json` from the repo root. It names the
issue tracker, the build commands, and the review configuration.

## Do you need this file?

Not for every skill. `boss-review` and `boss-finalize` run in a repository that
carries no `.boss-skills.json` at all. The built-in `DEFAULT_CONFIG` covers Go,
web, database, and API changes.

Customize the config file when you want the tracker-driven skills, when
detection produces the wrong command for a key, or when the built-in review
catalogue misses a language your repository uses.

## Where these values come from

`team`, the `states` values, and the label values are display strings out of
your own tracker. Copy them from the tracker rather than inventing them.
`validateConfig` checks that each is a non-empty string and stops there, so a
state name matching no real workflow state passes validation and fails much
later, inside a tracker write.

## The minimum viable config

`boss-plan` requires four roles under `states`: `unplanned` and `planned` for
ticket selection and write-back, plus `inProgress` and `inReview` for the
active-backlog reads. The only tracker currently supported is
[Linear](https://linear.app/).

```json
{
  "adapters": { "tracker": "linear" },
  "trackerConfig": {
    "linear": {
      "mcpServer": "acme-linear",
      "team": "Acme",
      "states": {
        "unplanned": "Unplanned",
        "planned": "Todo",
        "inProgress": "In Progress",
        "inReview": "In Review"
      }
    }
  }
}
```

## Declaring the tracker MCP server

This is your tracker vendor's own MCP server, not the Bossanova session-control
server described in the [MCP guide](/guides/mcp), which exposes sessions, repos,
and cron jobs to an agent.

Claude Code reads `.mcp.json`:

```json
{
  "mcpServers": {
    "acme-linear": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer ${TRACKER_API_KEY}" }
    }
  }
}
```

Codex reads `.codex/config.toml`. Quote the table key: quoting is legal for
every key and required for a hyphenated one, so quoting always keeps the key
spelled the way `.boss-skills.json` spells it.

```toml
[mcp_servers."acme-linear"]
url = "https://mcp.example.com/mcp"
bearer_token_env_var = "TRACKER_API_KEY"
required = false
```

The key has to be identical to `trackerConfig.<tracker>.mcpServer`: same case,
same hyphens, no underscore substitution.

## Config reference

| Section             | What it controls                                                                                     | Absent, or present and wrong                                                                                                                                                                        |
| ------------------- | ---------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `lensMap`           | Path globs mapped to a review lens id and the skill that reviews it.                                 | Absent: the built-in Go, web, database, and API catalogue applies. An entry missing a non-empty `id`, `skill`, matcher, or `fallbackRubric` fails validation at load.                               |
| `commands`          | Command strings, read by name. Detection fills `build`, `lint`, `format`, and `test`.                | Absent: detection fills the four detectable keys from the repository's marker files and `command()` returns `null` for the rest. A non-string or empty value fails validation.                      |
| `test.manifestPath` | Path to this repository's test-command manifest.                                                     | Never defaulted and never detected. Absent: `manifestPath()` returns `null`. An empty string fails validation.                                                                                      |
| `reviewDefaults`    | Opportunistic review rounds, the delta-review file threshold, and the per-run dispatch cap.          | Absent: the two shipped rounds apply. A `rounds` array declared empty runs none. A malformed block fails validation. An out-of-range number warns and falls back to the shipped value.              |
| `reviewLedger`      | Repo-relative directory where `boss-review` writes its dispatch ledgers.                             | Absent: `.git/boss-review-ledgers`. An absolute path, a backslash separator, or a path climbing out of the repository fails validation.                                                             |
| `env`               | Headless-detection signals, and whether an absent TTY counts as headless.                            | Absent: the built-in signals apply. A signal without a string `var`, or a non-boolean `headlessWhenNoTty`, fails validation.                                                                        |
| `adapters`          | Which implementation the `tracker`, `publish`, and `sessionRunner` seams resolve to.                 | Absent: `linear`, `proof`, and `bossd`. An empty or non-string selection fails validation. A tracker selection with no matching `trackerConfig` block resolves to null and the skills self-disable. |
| `extensionRoots`    | Ordered repo-local skill roots scanned for `boss-*` extensions.                                      | Absent: `.claude/skills` then `.codex/skills`. An empty array fails validation. A root that does not exist is passed over in silence.                                                               |
| `trackerConfig`     | Per-tracker identity: `mcpServer`, `team`, and the `states`, `labels`, and `githubLabels` role maps. | Absent: the tracker-driven skills self-disable. A block missing `mcpServer` or `team` fails validation. A role naming no real tracker entity fails inside a later tracker write.                    |
| `publishConfig`     | Per-publisher `bucket` and `baseUrl` for proof artifacts.                                            | Absent: proof publishing has no destination. A present block missing either field fails validation.                                                                                                 |
| `planStorage`       | Where `boss-plan` stores the implementation plan it writes.                                          | Absent: `tracker-attachment`, the one accepted value. A `kind` of `r2` warns and is coerced back to it. Any other `kind` fails validation.                                                          |
| `planContract`      | The plan-description section contract, and the `planFile` heading floor below.                       | Absent: the shipped contract applies. A section with an unrecognised `required` class, or an empty `requiredHeadings`, fails validation.                                                            |
| `epicDefaults`      | `childWallClockMinutes`, the budget `boss-epic` gives one child before fail-isolating it.            | Absent: 360. A non-object block fails validation. A value outside `[1, 1440]` warns and falls back to 360.                                                                                          |
| `notesDefaults`     | `sampleRate`, the probability that a run performs its reporting phases at all.                       | Absent: `1`, so every run reports. A non-object block fails validation. A number outside `[0, 1]` warns and falls back to `1`.                                                                      |

## Failure modes

The third column above splits into four classes, distinguished by where the
failure shows up.

- **Silent self-disable.** An absent `trackerConfig`, or a `states` map missing
  one of the four planning roles, stops `boss-plan` at preflight. It prints one
  line and makes no tracker call, which is the intended behaviour in an
  unconfigured repository. `boss-build` stops quietly too, under a `NO_CHANGE`
  outcome, on a `states` map missing any of the three roles it drives.
  `boss-epic` is the exception: it stops loudly, naming both the adapter and the
  config it probed for a planned state.
- **Loud throw.** `labelName()` fails closed. Asking for a label role the config
  never mapped throws `skill-config: trackerConfig.<tracker>.labels.<role> must
be configured as a non-empty string` rather than resolving to nothing.
  Content-taxonomy labels that a repository legitimately leaves unmapped go
  through `optionalLabelName()`, which returns null instead.
- **Late failure.** A `states` or `labels` value that names no real workflow
  state or label passes `validateConfig`, because validation checks the type and
  not the tracker. The failure lands deep inside a later tracker write, after
  the skill has already done its work.
- **Connects, then fails.** An MCP server key that differs from
  `trackerConfig.<tracker>.mcpServer` connects, and a health listing reports it
  healthy, because neither one knows what name the config meant. The first tool
  invocation is the first thing to notice.

## Inheriting the plan structure

Bossanova uses the plan structure popularized by the [Compound
Engineering](https://github.com/everyinc/compound-engineering-plugin) skills.

### Overriding the plan structure

## Compound engineering

Install the [compound-engineering
plugin](https://github.com/EveryInc/compound-engineering-plugin) and keep
`reviewDefaults` as it ships. `reviewDefaults.rounds` names
`compound-engineering:ce-code-review` as a `kind: skill` round, and an absent
capability is a silent skip recorded in the run ledger and nowhere else. An
adopter without the plugin loses that review round and sees no warning about it.

`boss-plan` resolves its drafting through three tiers. Tier 1 is a repo-local
`boss-plan-*` extension declaring `role: draft`; tier 2 is the host's own
drafting command; tier 3 is the skill's inline drafting prompt, which depends on
no external skill. Tiers 2 and 3 run whenever no tier-1 dispatch succeeded,
rather than only when no extension is installed.

No `role: draft` extension ships to adopters, so an adopting repository drafts
at tier 2 or tier 3. Its plans still carry the compound-engineering headings,
because those come from the `planContract` defaults rather than from the
drafting tier.

## Precedence and discovery

Three layers merge, lowest first: `DEFAULT_CONFIG`, then detection, then the
config file. Arrays replace wholesale, plain objects shallow-merge per key, and
scalars replace. Detection is anchored at the directory holding the discovered
file, and it runs only for the detectable command keys that file leaves
undeclared, so a fully-configured repository reads no marker file at all.

`findConfigFile()` walks up from the working directory and takes the first
`.boss-skills.json` it finds. A config in a subdirectory therefore shadows one
at the repository root for every skill run at or below that subdirectory. `boss
init` warns when the file it writes shadows an ancestor. Nothing warns at
skill-run time, so a nested config added by hand afterwards goes unannounced.

## Tracker support today

`linear` is the only tracker adapter that ships. `BUILDERS` in
`skills-toolbox/tracker/adapter.mjs` holds one entry. A `trackerConfig` block
keyed under another tracker name still validates, and once `adapters.tracker`
names that same tracker the state and label lookups read it, but no adapter
backs it. Adapter resolution reads the `TRACKER` environment variable and falls
back to `linear`, and setting `TRACKER` to that name throws `unknown tracker:
<name>` until someone registers a builder at that seam.

## See also

- [Extension System](/skills/extensions) for repo-local add-ons to a core skill.
- [`boss init`](/reference/cli-reference) in the CLI reference, for the flags
  that bootstrap this file.
- `docs/skills/skill-config.md` in the Bossanova repository, for the
  field-by-field contract behind the table above.
