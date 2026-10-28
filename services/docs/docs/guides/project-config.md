---
title: Project Config (.boss-project.json)
description: Export, compare and apply a repository’s daemon, cron and cloud configuration.
slug: /guides/project-config
---

# Project Config: `.boss-project.json`

A repository's boss setup (which checkout each daemon uses, the factory crons, the cloud
triggers and the session webhooks) normally lives only as rows in the daemon's and the
orchestrator's databases. `.boss-project.json` writes it down at the repository root so it
is reviewable in a pull request and replayed on another machine.

The file is not required: every command behaves exactly as before when it is absent.
`boss project export` writes it, `boss project diff` and `boss project apply` converge the
local daemon onto it (with `--cloud`, also your cloud triggers and the organization's session
webhooks), and `boss init` applies it when it is present.

Editors validate against the [JSON Schema](https://github.com/bossanova-dev/bossanova/blob/main/lib/bossalib/kit/projectconfig/schema/boss-project.schema.json)
(draft 2020-12). For editor validation, use the raw JSON URL in your file:

```json
{
  "$schema": "https://raw.githubusercontent.com/bossanova-dev/bossanova/main/lib/bossalib/kit/projectconfig/schema/boss-project.schema.json"
}
```

Boss itself decodes and validates the file without using the schema.

---

## Exporting

```bash
boss project export                  # print to stdout
boss project export --cloud          # include triggers and session webhooks (needs boss login)
boss project export --write          # write <main checkout>/.boss-project.json
boss project export --write --force  # overwrite an existing file
```

- The checkout (`--dir`, default `.`) must be registered with the local daemon; otherwise
  export fails and points at `boss init --register`. `--remote` and `--host` are refused.
- The `daemons` entry is the local daemon: its display name, its routing `daemonId` (read
  from the daemon's own startup record, trusted only when that record is bound to the
  socket this CLI talks to) and this checkout's path, with the home directory written as
  `~/`. When the id is unknown the entry is name-only and export warns.
- `--cloud` reads the organization from `--org`, else the repository's organization
  mapping, else your only organization. Triggers are your triggers on this repository's
  origin; webhooks are the organization's.
- The project slug is `--project`, else the existing file's `project`, else the
  repository name from the origin URL.
- Warnings go to stderr, so stdout is pipeable. `--write` warns when git ignores the file.
- Export refuses to print a file that would not validate (for example two cron jobs with
  the same name).
- Export never writes `repo.isTrusted`: trust is a local decision. Add the field deliberately
  when the file should manage trust; omitting it leaves each daemon's trust unchanged.

---

## Diff and apply

```bash
boss project diff                      # print the plan for this daemon (read-only)
boss project diff --exit-code          # exit 1 when not converged, 2 on error, 0 otherwise
boss project apply                     # converge this daemon onto the file
boss project apply --dry-run           # print the plan, write nothing
boss project apply --prune             # delete managed crons the file dropped
boss project apply --yes               # confirm repo trust grants, cron prompts and gates
boss project apply --allow-branch      # apply from a checkout off the default branch
boss project diff --cloud              # also plan cloud triggers, webhooks and other daemons' crons (needs boss login)
boss project apply --cloud             # also converge cloud triggers, webhooks and other daemons' crons
boss project apply --cloud --rotate-secrets   # also rotate the project's signing secrets
```

Both read `<top level of --dir>/.boss-project.json` and refuse `--remote` and `--host`.
`--json` prints the plan (and, for apply, the result: applied, failed and not-reached
changes). The plan lists one line per change that is not `unchanged`, then the warnings.

### This daemon

The daemon's routing id and display name come from the startup record bossd writes,
trusted only when it names the socket this CLI talks to. An entry pinned by `daemonId`
matches by id; otherwise a unique unpinned entry whose `name` is the display name matches.
When the record is missing or stale, matching falls back to the display name with a
warning, and pinned entries never match until bossd restarts.

- **Matched:** the repo lives at the entry's `path` (`~/` expanded), and the crons that
  name the entry (or every cron, when there is exactly one entry) run here.
- **Unmatched:** the repo lives at the checkout being applied, every cron is `skip`
  (`daemon-unmatched`), and a warning prints the `daemons` entry to add.
- **Several entries match:** one `conflict` (`ambiguous`) naming them; crons `skip`, while
  repo settings and secrets still converge at the checkout being applied.

### Actions

| Action      | Meaning                                                                             |
| ----------- | ----------------------------------------------------------------------------------- |
| `create`    | The daemon lacks the row; apply creates it (a cron with the project marker)         |
| `update`    | An own row (or the repo) differs in the fields named                                |
| `adopt`     | An unmarked row is taken over: it gains the marker, plus any field updates          |
| `disable`   | An own cron the file no longer runs here is disabled                                |
| `delete`    | Same, under `--prune`                                                               |
| `rotate`    | Replace an owned HTTP trigger or webhook signing secret (`--rotate-secrets`)        |
| `unchanged` | Nothing to do                                                                       |
| `conflict`  | Cannot be applied as written (another project's row, two candidates, a bad gate, …) |
| `skip`      | Deliberately not applied; the reason says why                                       |

`diff --exit-code` exits 0 only when every change is `unchanged`.

### The repo

Apply never re-registers. The path must be a checkout whose origin is the file's
`originUrl`; otherwise the repo is a `conflict` (`repo-path` or `origin-mismatch`) and the
crons `skip`. A registration at that path is the repo; the same origin registered at another
path is a `conflict` (`registered-elsewhere`); otherwise apply registers it. Each stated
`displayName`, `setupScript`, `sentryOrg` and `settings` key that differs is an `update`;
absent keys are left alone. `defaultBaseBranch` is set only when registering, so a
difference on an existing repo is a `skip` (`registration-only`).

`repo.isTrusted` manages whether the daemon trusts the repo's own configuration, including
agent project config and proof secrets. When stated, it plans a separate `repo.isTrusted`
`update` if the live trust differs, or `unchanged` if equal. When absent, live trust is left
alone. A new registration starts untrusted; `isTrusted: true` grants trust after registration,
subject to the confirmation below.

See [Repository trust](../reference/security-and-permissions.md#repository-trust)
for what this enables in Codex and Claude, when their trust settings are written,
and how to inspect the live value with `boss repo show` or `boss repo ls`.

Secrets are read from their `BOSS_*` variables only. A set variable that differs from the
stored value is an `update` rendered as `<secret>`; equal is `unchanged`; unset with a
stored value is `unchanged` with a warning; unset with nothing stored is a `skip`. Apply
only ever sets a secret: it never clears one and never prints a value.

### Crons

A cron is found by `name`. A factory cron with no row of its name also finds an unmarked
row whose prompt starts with the stage skill (`/boss-build` or `$boss-build`): exactly one
is adopted and renamed to the file's name; several are a `conflict`. Rows already carrying
this project's marker are never adopted under another name.

| Live row                             | Action                                |
| ------------------------------------ | ------------------------------------- |
| none                                 | `create`                              |
| this project's marker                | `update` or `unchanged`               |
| no marker                            | `adopt`                               |
| another project's marker             | `conflict` (`managed-by-other`)       |
| own row the file no longer runs here | `disable`, or `delete` with `--prune` |
| unmarked row the file does not name  | never touched, even with `--prune`    |

Only fields the file states are compared; a factory cron always compares its prompt, gate
and agent, and its omitted `schedule` is the stage default on create only. An omitted
`enabled` leaves the live state alone (a disabled row stays disabled, with a warning) and
creates the row enabled. A factory cron whose gate preflight fails is created disabled, and
an update to an existing one is a `skip` (`preflight-failed`). An unknown stage is a `skip`.
A gate compares by the command it executes, so a quoting, `$HOME` or `~` respelling of the
same command (for example an absolute, unquoted path to the stage's own gate script) is
`unchanged`.

A plain cron's `gateCommand` is a repo-relative script path: `./<path>` (run without a
shell) or `node <path>`; a bare `<path>` is stored as `./<path>`. Absolute paths, `..`
segments and characters outside `[A-Za-z0-9._/-]` are a `conflict` (`gate-path`).

Writes run repo, then secrets, then crons (create, update and adopt, then disable, then
delete, each by name). Each class stops at its first failed write and the next class still
runs; a failed registration leaves the crons unreached. A bossd too old to store the
ownership marker echoes rows without it: that cron is reported `daemon-too-old` and the cron
class stops.

### Trust

Before any write the checkout must be on the repo's default branch (the file's
`repo.defaultBaseBranch`, else the registered default, else origin HEAD), or pass
`--allow-branch` (a detached HEAD counts as off-branch). A plan that grants repo trust, creates a cron or
changes a cron's prompt or gate (or, with `--cloud`, creates a trigger or webhook, changes a
trigger's prompt or gate, or rotates a secret) is shown and confirmed on a terminal; elsewhere
it needs `--yes`, and without it nothing is written on any leg and the command exits non-zero.
Revoking repo trust (`isTrusted: false`), other repo settings, secrets, schedules and disables
need no confirmation. `diff` and `--dry-run` skip both
checks.

### Cloud triggers and webhooks

`--cloud` adds a leg to the plan rather than replacing the local one: the same run plans this
daemon, your cloud triggers, the organization's session webhooks and the crons the file places
on your other daemons, passes one trust gate, and prints one report. Writes run local first,
then triggers, then webhooks, then your other daemons' crons; each class stops at its first
failed write and the next class still runs. A file without a `cloud` section, or without a
`triggers` or `webhooks` array, leaves that class alone (see
[Crons on your other daemons](#crons-on-your-other-daemons)).

**Organization.** `cloud.organization`, else `--org`, else the repository's organization
mapping, else your only organization. `--org` disagreeing with `cloud.organization` is an
error before any request, and so is `--cloud` without `boss login`.

**Paid plan first.** The first cloud request lists your triggers, which runs the paid-plan
check. If it fails, the command exits non-zero having written nothing on any leg, the local
daemon included.

**Permissions.** Existing permissions apply:

- Triggers are yours alone. Only your own triggers in the organization are read and
  converged; another member's trigger is never read or edited. A trigger is adopted only when
  it is unmarked and watches this repository's origin.
- Session webhooks need an active owner of the organization. For anyone else every webhook
  is a `skip` (`owner-required`) while triggers and the local leg still converge, and the
  command exits non-zero.

**Identity; comparison.** A trigger is found by `name`, a webhook by `url`. The action
table for crons applies (create, update, adopt, `managed-by-other` conflict, disable or
`--prune` delete of own rows the file dropped; unmarked and foreign rows are never touched).
Fields compare through the same conversion export uses, with absent fields read as
the server's defaults, so an exported file applies and then diffs clean. An omitted `enabled`
leaves the live state alone, as for crons. A trigger's `type` cannot change in place: that is
a `conflict` (`type-change`). A webhook bosso marks `is_retire_only`, an endpoint whose creator is
no longer an owner, which allows only disabling or deleting while the organization does not share
sessions, comes without its URL and so has no identity to match: apply leaves it alone (warning
when it carries this project's marker), `--prune` never deletes it, and export leaves it out with
a warning.

| Reason                    | Meaning                                                                                                                                             |
| ------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `owner-required`          | Webhooks need an active organization owner                                                                                                          |
| `managed-by-other`        | The trigger or webhook carries another project's marker; both projects see the conflict                                                             |
| `managed-by-other-member` | Another member already has a trigger with this project's marker and name, or a webhook with its marker and URL that you cannot see (found on write) |
| `daemon-unmatched`        | A `specific-daemon` placement names an entry none of your daemons matches                                                                           |
| `ambiguous`               | Two of your daemons match the placement's entry, or two rows match one identity                                                                     |
| `secret-unresolved`       | The webhook's `secret` names an unset `BOSS_*` variable; it is not created without it                                                               |
| `create-via-cli`          | The write would mint a secret and the caller cannot show it (MCP)                                                                                   |
| `refused`                 | bosso refused the write (for example the organization's webhook cap); the detail says why                                                           |
| `server-too-old`          | bosso accepted a write without storing the marker; that class stops                                                                                 |

**Placement.** `{"mode": "specific-daemon", "daemon": "<name>"}` resolves through your own
daemons: an entry pinned by `daemonId` matches that daemon, otherwise a daemon whose hostname
is the entry's name.

**Secrets.** bosso mints a secret when it creates an HTTP trigger, when it creates a webhook
whose entry has no `secret`, and on a rotation without a resolvable reference. Each is printed
to stderr the moment it arrives, before the next write, and never appears on stdout, in
`--json` or in a log: store it then, because it is not shown again. A webhook `secret`
reference is resolved only on create. `--rotate-secrets` rotates every HTTP trigger and webhook
the file keeps that already carries this project's marker (a webhook to its resolved
reference, else a minted one).

Trigger creates, changes to a trigger's launch prompt or gate, webhook creates and every
rotation need confirmation, under the same rules as cron prompts below.

### Crons on your other daemons

With `--cloud`, the crons the file places on a `daemons[]` entry other than this daemon's are
converged on that machine through the cloud, aimed at its daemon id. This daemon's own crons
are always written over its socket by the local leg and never through the cloud. Once the
file has a `crons` key, every other entry is classified from your organization's daemon list
before any write, including one that no longer runs a cron, so the own-marker rows it dropped
are still disabled (or deleted under `--prune`); a failed daemon-list read aborts the apply:

| Entry matched by                                      | Result                                              |
| ----------------------------------------------------- | --------------------------------------------------- |
| this daemon                                           | not in the cloud leg (the local leg applies it)     |
| one of your daemons, online, serving the repo         | its crons are planned with the [cron rules](#crons) |
| one of your daemons that does not serve the repo      | `skip` (`repo-not-registered`)                      |
| one of your daemons, offline                          | `skip` (`daemon-offline`), nothing sent             |
| two or more of your daemons                           | `conflict` (`ambiguous`)                            |
| only another member's daemon                          | `skip` (`not-your-daemon`)                          |
| nothing connected to your account in the organization | `skip` (`daemon-unmatched`)                         |

Matching is the same as for this daemon: an entry pinned by `daemonId` matches by id,
otherwise a unique unpinned entry whose `name` is the daemon's hostname. The cron rules are
the local ones (adopt, `managed-by-other` conflict, disable or `--prune` delete of own rows),
applied to that daemon's rows for the repo. Factory crons resolve from the stage table
without any host path: a factory cron with no row there is a `skip` (`needs-local-apply`;
run `boss project apply` on that machine, since its gate embeds that host's paths), and an
existing row converges every field except the gate command, which stays as that host set it.

When this daemon's routing id is unknown (see [This daemon](#this-daemon)), it cannot be told
apart from the rest of the fleet, so every cron on another entry is a `skip`
(`local-daemon-unidentified`) until bossd restarts.

Writes run daemon by daemon in entry-name order, after triggers and webhooks. Each daemon
stops at its first failed write and the next daemon still runs: a daemon that turns out to
be unreachable is reported `daemon-offline`, and one too old to store the ownership marker
`daemon-too-old`. Repo registration and repo settings are never written on another daemon;
run `boss project apply` there for those. Any `skip` or `conflict` keeps
`diff --cloud --exit-code` at 1 and makes `apply --cloud` exit non-zero.

### `boss init`

With the file present, `boss init` loads and validates it and runs the branch check before
writing anything, then applies it last (after the config write and the factory cron step)
through the same engine, never pruning. `--cron`/`--agent`/`--update-crons` take the crons,
`--no-register` the registration and the credential flags the secrets for that run, each
with a warning. A refusal leaves init's other steps standing, and the stopped report names
the command that finishes the job. Without the file, `boss init` is unchanged.

### Over MCP

`export_project_config`, `diff_project_config` and `apply_project_config` run the same
engine for an agent, with the file's contents passed as the `config` argument (decoded and
validated like the file, before any request). The server picks the scope: the local
`bin/mcp` server plans this daemon (repo, settings, crons; `working_dir` is the checkout
used when no `daemons[]` entry matches), and the hosted gateway plans the cloud section and
the crons on your daemons (`organization_id`, else `cloud.organization`). The CLI's trust
rules hold except for the prompt: `apply_project_config` writes only with
`"dry_run": false`, which stands in for `--yes`, and locally needs the applied checkout on the
default branch unless `"allow_branch": true`. MCP never prunes, never resolves a `BOSS_*`
reference and never mints a signing secret, so those changes are skips (`secret-unresolved`,
`create-via-cli`) left for `boss project apply`; so is a factory cron with no live row,
whose gate embeds this host's paths. The result's `plan` is the `--json` plan. See
[MCP project config tools](./mcp.md#project-config).

Local MCP never grants repo trust, even with `"dry_run": false`: a grant is a `skip`
with reason `trust-via-cli`, left for `boss project apply` with terminal confirmation or
`--yes`. Revoking trust still applies over local MCP.

---

## Format

Keys are camelCase. Encoding is canonical: fields in the order below, two-space indent, one
trailing newline, arrays sorted by identity (`daemons`, `crons` and `triggers` by `name`,
`webhooks` by `url`; `eventTypes`, `payloadFields`, trigger `filters` and each cron's
`daemons` sorted too). Decoding then re-encoding an exported file is byte-identical.

### Decode rules

- An unknown key at any level is a warning naming its JSON path (`crons[1].retries`), and is
  dropped; the file still loads.
- `version` is required and must be `1`. A newer version is an error naming the supported
  one: upgrade boss.
- An unrecognised enum value (`factoryStage`, cron or trigger `concurrencyPolicy`,
  `mergeStrategy`, trigger `type`, placement `mode`) is a warning and marks that row
  unsupported: later tooling skips it rather than failing the whole file.
- Validation reports every problem at once, each prefixed by its JSON path.

### Absent versus empty

An absent `daemons`, `crons`, `cloud`, `cloud.triggers` or `cloud.webhooks` means the file
does not manage that class of resource. An explicit `[]` means it does, and the desired set is
empty.

For rows the file owns (crons, triggers, webhooks), an absent field means the
server default, except `enabled`, where absent means enablement is not managed. For the
shared `repo` row, every absent setting means "leave the live value alone". Export therefore
writes every observed repo setting except `isTrusted`, and only non-default row fields (`enabled` only as
`false`).

### Secrets

The file never contains a secret value. A secret is a reference to an environment variable:

```json
{ "env": "BOSS_LINEAR_API_KEY" }
```

A string literal, an extra key, or a name not matching `^BOSS_[A-Z0-9_]+$` is
a decode error, and the error never repeats the value. Export writes `BOSS_LINEAR_API_KEY`
and `BOSS_SENTRY_AUTH_TOKEN` references only when the live credential is set. Note that
`boss init` reads `LINEAR_API_KEY`, while the file references `BOSS_LINEAR_API_KEY`. Webhook
signing secrets are never exported; triggers carry none.

### Ownership marker

Rows a project config owns are marked `boss-project:<project>`, where `<project>` matches
`^[a-z0-9][a-z0-9-]{0,62}$`. Cron jobs carry it in bossd; triggers and session webhooks carry
it in the orchestrator, where one organization holds at most one trigger per marker and name
(across members) and one webhook per marker and URL.

---

## Fields

### Top level

| Key       | Required | Meaning                                          |
| --------- | -------- | ------------------------------------------------ |
| `$schema` | no       | Schema reference for editors; preserved          |
| `version` | yes      | `1`                                              |
| `project` | yes      | Project slug (see the ownership marker)          |
| `repo`    | yes      | The repository row                               |
| `daemons` | no       | Daemons that run crons, and their checkout paths |
| `crons`   | no       | Scheduled jobs                                   |
| `cloud`   | no       | Organization, triggers and session webhooks      |

### `repo`

| Key                 | Meaning                                                                                                                                                                                           |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `originUrl`         | Required; compared in canonical `https://<host>/<owner>/<repo>` form                                                                                                                              |
| `isTrusted`         | Boolean; absent leaves live trust alone. A grant needs terminal confirmation or `--yes` and is skipped over MCP; a revoke needs no confirmation. Never exported.                                  |
| `displayName`       | Display name                                                                                                                                                                                      |
| `defaultBaseBranch` | Base branch                                                                                                                                                                                       |
| `setupScript`       | Setup script                                                                                                                                                                                      |
| `sentryOrg`         | Sentry organization slug                                                                                                                                                                          |
| `settings`          | `canAutoMerge`, `canAutoMergeDependabot`, `mergeStrategy` (`merge`, `rebase`, `squash`), `canAutoRepair`, `shouldArchiveSessionsAfterMerge`, `canAutoDeleteBranches`, `shouldKeepBranchesCurrent` |
| `secrets`           | `linearApiKey`, `sentryAuthToken`, each a secret reference                                                                                                                                        |

### `daemons[]`

| Key        | Meaning                                                    |
| ---------- | ---------------------------------------------------------- |
| `name`     | Required, unique; matched to a daemon's display name       |
| `daemonId` | Pins the entry to one daemon's routing id; unique when set |
| `path`     | Checkout path on that daemon: absolute or `~/`-relative    |

A cron must name a daemon entry that has a `path`.

### `crons[]`

| Key                 | Meaning                                                                          |
| ------------------- | -------------------------------------------------------------------------------- |
| `name`              | Required, unique                                                                 |
| `factoryStage`      | `plan`, `build`, `verify` or `retro`: a symbolic factory cron                    |
| `agent`             | Agent runner; for a factory cron, absent lets apply pick one as `boss init` does |
| `model`             | Model id                                                                         |
| `prompt`            | Required for a plain cron; forbidden on a factory cron                           |
| `schedule`          | 5-field cron expression or descriptor; required for a plain cron                 |
| `timezone`          | IANA time zone; absent means daemon-local                                        |
| `gateCommand`       | Plain crons only: `./<path>` or `node <path>`, with a repo-relative path         |
| `runSetupCommand`   | Run the setup script first (default `true`)                                      |
| `zeroOutput`        | Zero-output job (default `false`)                                                |
| `concurrencyPolicy` | `skip` (default), `cancel-in-progress` or `allow-concurrent`                     |
| `enabled`           | Absent: not managed                                                              |
| `daemons`           | `daemons[]` names that run it; is omittable only when exactly one entry exists   |

A factory cron is symbolic: the stage defines its prompt and gate, so the same job exported
on two machines (different home directories, different `node` paths) yields the same entry.
Export classifies a job as factory only when its prompt is exactly the stage's prompt for its
agent; anything else (for example `/boss-build BOS-1`) exports as a plain cron, and a gate
command that is not portable is dropped with a warning that it might embed host paths. A plain
row without `gateCommand` leaves the live gate alone, so applying the file on the daemon it
came from adopts the job and keeps that gate; only a daemon that creates the row from the
file runs it ungated.

### `cloud`

| Key            | Meaning                             |
| -------------- | ----------------------------------- |
| `organization` | Organization id                     |
| `triggers`     | Inbound triggers on this repository |
| `webhooks`     | The organization's session webhooks |

### `cloud.triggers[]`

| Key                 | Meaning                                                                                          |
| ------------------- | ------------------------------------------------------------------------------------------------ |
| `name`              | Required, unique                                                                                 |
| `type`              | Required: `http` or `github`                                                                     |
| `http` / `github`   | The type's config (protojson `HTTPTriggerConfig` / `GitHubTriggerConfig`); only the matching arm |
| `launch`            | Required: protojson `TriggerLaunchSettings`                                                      |
| `filters`           | protojson `TriggerFilter` list; all must match                                                   |
| `payloadFields`     | Payload fields copied into the launch context                                                    |
| `gate`              | protojson `TriggerGate`; `scriptPath` is a clean relative path                                   |
| `placement`         | Required (below)                                                                                 |
| `concurrencyPolicy` | `skip-if-running` (default), `allow-parallel` or `cancel-in-progress`                            |
| `cooldownSeconds`   | Minimum gap between launches, `>= 0`                                                             |
| `enabled`           | Absent: not managed (created enabled)                                                            |

The protojson objects follow the [trigger API messages](https://github.com/bossanova-dev/bossanova/blob/main/proto/bossanova/v1/models.proto). Placement is one of:

```text
{ "mode": "first-available" }
{ "mode": "specific-daemon", "daemon": "<daemons[].name>" }
{ "mode": "specific-daemon", "daemonId": "<daemon id>" }
```

Export names the `daemons[]` entry when a trigger is pinned to the local daemon, and keeps the
raw id otherwise. Apply resolves a named entry through your own daemons (see
[Cloud triggers and webhooks](#cloud-triggers-and-webhooks)).

### `cloud.webhooks[]`

| Key           | Meaning                                 |
| ------------- | --------------------------------------- |
| `url`         | Required, absolute, unique              |
| `description` | Description                             |
| `eventTypes`  | Required, at least one                  |
| `enabled`     | Absent: not managed (created enabled)   |
| `secret`      | A secret reference, used only on create |

A webhook's payload `format` is not part of v1; export warns when a webhook uses a format
other than `raw`.

---

## Example

An export of a repository with two factory crons, one plain cron, three triggers and two
webhooks:

<!-- prettier-ignore -->
```json
{
  "version": 1,
  "project": "example",
  "repo": {
    "originUrl": "https://github.com/acme/example",
    "displayName": "Example",
    "defaultBaseBranch": "main",
    "setupScript": "make setup",
    "settings": {
      "canAutoMerge": true,
      "canAutoMergeDependabot": false,
      "mergeStrategy": "squash",
      "canAutoRepair": true,
      "shouldArchiveSessionsAfterMerge": true,
      "canAutoDeleteBranches": true,
      "shouldKeepBranchesCurrent": false
    }
  },
  "daemons": [
    {
      "name": "laptop",
      "daemonId": "d-laptop",
      "path": "~/code/example"
    }
  ],
  "crons": [
    {
      "name": "factory-build",
      "factoryStage": "build",
      "agent": "claude",
      "schedule": "*/15 * * * *"
    },
    {
      "name": "factory-plan",
      "factoryStage": "plan",
      "agent": "claude",
      "schedule": "*/15 * * * *"
    },
    {
      "name": "nightly-report",
      "agent": "codex",
      "model": "gpt-5",
      "prompt": "Summarise yesterday's merges.",
      "schedule": "0 6 * * *",
      "timezone": "Europe/London",
      "gateCommand": "node scripts/has-work.mjs",
      "runSetupCommand": false,
      "zeroOutput": true,
      "concurrencyPolicy": "allow-concurrent",
      "enabled": false
    }
  ],
  "cloud": {
    "organization": "org-1",
    "triggers": [
      {
        "name": "anywhere",
        "type": "github",
        "github": {
          "eventTypes": [
            "push"
          ]
        },
        "launch": {
          "isZeroOutput": false,
          "promptTemplate": "Review it.",
          "shouldRunSetupCommand": true
        },
        "placement": {
          "mode": "first-available"
        }
      },
      {
        "name": "local-pin",
        "type": "github",
        "github": {
          "eventTypes": [
            "pull_request.opened"
          ]
        },
        "launch": {
          "isZeroOutput": false,
          "promptTemplate": "Review it.",
          "shouldRunSetupCommand": true
        },
        "placement": {
          "mode": "specific-daemon",
          "daemon": "laptop"
        }
      },
      {
        "name": "remote-pin",
        "type": "http",
        "http": {},
        "launch": {
          "isZeroOutput": false,
          "promptTemplate": "Review it.",
          "shouldRunSetupCommand": true
        },
        "filters": [
          {
            "field": "body.env",
            "operator": "TRIGGER_FILTER_OPERATOR_EQUALS",
            "values": [
              "prod"
            ]
          }
        ],
        "payloadFields": [
          "body.ref"
        ],
        "gate": {
          "scriptPath": "scripts/gate.sh"
        },
        "placement": {
          "mode": "specific-daemon",
          "daemonId": "d-other"
        },
        "concurrencyPolicy": "cancel-in-progress",
        "cooldownSeconds": 30,
        "enabled": false
      }
    ],
    "webhooks": [
      {
        "url": "https://hooks.example.com/a",
        "description": "Team",
        "eventTypes": [
          "session.merged"
        ],
        "enabled": false
      },
      {
        "url": "https://hooks.example.com/b",
        "eventTypes": [
          "session.failed",
          "session.passing"
        ]
      }
    ]
  }
}
```
