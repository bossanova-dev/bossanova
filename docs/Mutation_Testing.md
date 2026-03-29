# Mutation Testing

Mutation testing measures how well your tests actually catch bugs. It works by
introducing small changes (mutations) to your source code — flipping `<` to
`<=`, swapping `+` for `-`, negating conditions — and checking whether your
tests fail. If a test catches the change, the mutant is **killed**. If no test
notices, the mutant **survived**, which means there's a gap in your test suite.

We use [gremlins](https://github.com/go-gremlins/gremlins) to run mutation
tests across all Go modules in the workspace.

## Quick start

```bash
make mutate            # Full run across all 7 modules
make mutate-report     # Print summary
make mutate-survivors  # List surviving mutants
make mutate-fix        # Auto-generate tests for survivors via Claude Code
```

## Daily workflow

### 1. Run mutation tests

For day-to-day work on a feature branch, use the diff-scoped variant — it only
mutates code that changed relative to `main`, so it's fast:

```bash
make mutate-diff
```

For a full baseline (e.g. weekly or before a release):

```bash
make mutate
```

To run the complete loop — mutate, fix survivors, then verify — in one command:

```bash
make mutate-loop
```

### 2. Review results

```bash
make mutate-report
```

This prints per-package efficacy (% of mutants killed) and coverage (% of
mutants that were reachable by tests). Example output:

```
--- bossd--internal-session ---
  Efficacy:     100%
  Coverage:     93.29%
  Total:        139
  Killed:       139
  Lived:        0
  Not covered:  10
```

To see exactly which mutants survived:

```bash
make mutate-survivors
```

Output format: `[module--package] file:line MUTATION_TYPE`

```
[boss--internal-auth] oidc.go:99 ARITHMETIC_BASE
[boss--internal-auth] oidc.go:101 CONDITIONALS_NEGATION
[bossalib--machine] machine.go:161 ARITHMETIC_BASE
```

### 3. Fix surviving mutants

The fastest way to kill survivors is to let Claude Code write the tests:

```bash
make mutate-fix
```

This collects the survivor list and pipes it to Claude Code with a prompt that
instructs it to:

- Read each surviving mutant's source at the reported line
- Write or extend tests that would catch the mutation
- Follow existing test conventions (standard `testing` package, table-driven
  tests, hand-written mocks)
- Run `go test ./...` to verify each fix

You can also fix survivors manually — the `mutate-survivors` output tells you
exactly which file, line, and mutation type to target.

### 4. Verify the fix

Re-run mutation tests to confirm the survivors are now killed:

```bash
make mutate         # or make mutate-diff
make mutate-report  # check efficacy improved
```

## Mutation types

| Type                    | What it does               | Example             |
| ----------------------- | -------------------------- | ------------------- |
| `CONDITIONALS_BOUNDARY` | Shifts boundary operators  | `<` becomes `<=`    |
| `CONDITIONALS_NEGATION` | Negates conditions         | `==` becomes `!=`   |
| `ARITHMETIC_BASE`       | Swaps arithmetic operators | `+` becomes `-`     |
| `INCREMENT_DECREMENT`   | Flips increments           | `++` becomes `--`   |
| `INVERT_NEGATIVES`      | Removes negation           | `-x` becomes `x`    |
| `INVERT_LOGICAL`        | Flips logical operators    | `&&` becomes `\|\|` |

## Make targets reference

| Target                   | Purpose                             | Speed     |
| ------------------------ | ----------------------------------- | --------- |
| `make mutate-loop`       | Full cycle: mutate, fix, verify     | Long      |
| `make mutate`            | Full mutation run, all modules      | ~5-10 min |
| `make mutate-diff`       | Only code changed vs `main`         | Fast      |
| `make mutate-report`     | Human-readable summary              | Instant   |
| `make mutate-survivors`  | Machine-readable survivor list      | Instant   |
| `make mutate-fix`        | Auto-generate tests via Claude Code | Minutes   |
| `make mutate-bossalib`   | Single module: lib/bossalib         | Medium    |
| `make mutate-boss`       | Single module: services/boss        | Medium    |
| `make mutate-bossd`      | Single module: services/bossd       | Medium    |
| `make mutate-bosso`      | Single module: services/bosso       | Medium    |
| `make mutate-autopilot`  | Single module: plugins/autopilot    | Medium    |
| `make mutate-dependabot` | Single module: plugins/dependabot   | Medium    |
| `make mutate-repair`     | Single module: plugins/repair       | Medium    |

## How it works under the hood

The Go workspace (`go.work`) contains 7 modules. Gremlins doesn't support
`./...` in Go workspaces, so the Makefile iterates over each package within
each module individually. Results are written as JSON to `.mutate/` (gitignored).

The `mutate-report` and `mutate-survivors` targets parse these JSON files with
`jq` to produce human- and machine-readable output respectively.

## Key metrics

- **Test efficacy** — percentage of covered mutants that were killed. This is
  the primary quality signal. Target: >80%, ideally >95%.
- **Mutator coverage** — percentage of mutants that were reachable by tests.
  Low coverage means large parts of the code have no test coverage at all.

## Prerequisites

- [gremlins](https://github.com/go-gremlins/gremlins) — `go install github.com/go-gremlins/gremlins/cmd/gremlins@latest`
- [jq](https://jqlang.github.io/jq/) — for report parsing
- [Claude Code](https://claude.ai/claude-code) — for `make mutate-fix` (optional)
