---
name: file-a-flight-plan
description: Turns a preliminary task plan into a comprehensive implementation and testing plan with bd tasks and handoff checkpoints. Examines code, applies design skills, and creates self-verifying test steps.
---

# File a Flight Plan: From Rough Plan to Structured Execution

"Filing a flight plan" is the process of turning a preliminary idea or rough plan into a comprehensive, executable implementation plan with built-in testing and handoff checkpoints. Like a pilot filing their route before takeoff, this ensures every leg of the journey is mapped out — including how to verify you're on course.

---

## When to Use This Skill

Use file-a-flight-plan when:

- You have a rough task plan, feature request, or design document
- You need to produce a detailed implementation plan before coding
- The plan needs post-flight checks (the agent should be able to verify its own work)
- You want bd tasks with handoff checkpoints ready for `/take-off`

---

## Required Input

This skill expects a **preliminary plan** — any of:

- A rough description of what needs to be built
- A feature request or design document
- An existing plan file from `docs/plans/`
- A ticket or issue description

---

## Output

This skill produces:

1. **A comprehensive plan file** saved to `docs/plans/<YYYY-MM-DD-HHmm>-<feature-name>.md` (e.g., `docs/plans/2026-02-06-1229-user-profile.md`)
2. **A Flight ID** for task isolation across parallel work streams
3. **Post-flight checks** embedded in the plan so the agent can verify its own work via `/post-flight-checks`

This skill does **NOT** create bd tasks. That is `/pre-flight-checks`'s job. The workflow is:

```
/file-a-flight-plan → human approves → /pre-flight-checks → /take-off
```

---

## Flight ID

Each flight plan gets a unique **Flight ID** derived from its filename. This ID is used to isolate beads tasks per flight plan, preventing task confusion when running parallel work streams.

### Format

```
Plan file: docs/plans/2026-02-06-1229-user-profile.md
Flight ID: fp-2026-02-06-1229-user-profile
```

The Flight ID is `fp-` followed by the plan filename (without extension).

### Usage

The Flight ID is embedded in the plan document header and used by subsequent skills:

- `/pre-flight-checks` uses it to label all created tasks with `--labels "flight:fp-..."`
- `/take-off` and `/resume-handoff` use it to filter tasks with `--label "flight:fp-..."`
- `/handoff-task` includes it in handoff documents for continuity

---

## Phase 1: Understand the Scope

### 1.1 Read the Preliminary Plan

Read whatever the user provides — a file, a description, or a reference to existing work.

### 1.2 Identify Affected Areas

Identify which parts of the codebase are affected. For each area, note:

- The directory path
- Any relevant design conventions or skills
- How to test it (e.g., `make test`, curl, browser automation)

### 1.3 Load Relevant Design Skills

Load any project-specific design skills or conventions relevant to the affected code areas. Check `.claude/skills/` for applicable skills and read them to understand:

- Required patterns and conventions
- File structure and naming
- Type systems and shared code
- Anti-patterns to avoid

This is not optional. Design skills contain critical patterns that MUST inform the plan.

---

## Phase 2: Deep Code Examination

### 2.1 Map Existing Code

For each affected area, examine the relevant code:

- Find related files (search for similar feature names or patterns)
- Read existing implementations of similar features and mirror their patterns
- Check shared libraries for reusable code
- Review test patterns in existing test files
- Note configuration that needs updating (routes, exports, modules)

### 2.2 Identify Dependencies

Map out what depends on what:

- **Data flow:** Where does data come from? What data sources are involved?
- **Type flow:** What types need to be created or modified?
- **Import flow:** What files need to import the new code?
- **Test data:** What data exists to test against? What needs to be created?

### 2.3 Find Test Data and Endpoints

**This is critical for self-verification.** Identify:

- What API endpoints or commands can verify the work?
- What URLs can be loaded to verify rendering?
- What test data already exists (mocks, fixtures, seeds)?
- What queries or commands confirm correct output?

Search for existing test patterns in the codebase to understand how similar features are tested.

---

## Phase 3: Design the Test Strategy

For each affected area, design how the agent will **verify its own work**. This is the most important phase — every implementation task should have a corresponding way to test it.

### Testing Pattern Templates

Use these templates as starting points, adapting them to the project's actual tools and structure.

#### HTTP API Self-Test

```markdown
### Test: Verify [feature] endpoint works

**Method:** curl to API endpoint
**When:** After implementing the endpoint and restarting the server

\`\`\`bash
# Start server (adapt to project's dev command)
make dev &
until curl -s http://localhost:<port>/health > /dev/null 2>&1; do sleep 1; done

# Execute test request
curl -X POST http://localhost:<port>/api/<endpoint> \
 -H "Content-Type: application/json" \
 -d '{"key": "value"}'

# Expected: 200 OK with expected response shape
# Fail if: error response or unexpected data
\`\`\`
```

#### UI Self-Test

```markdown
### Test: Verify [page] renders correctly

**Method:** Playwright browser automation
**When:** After implementing the component

1. Navigate: `mcp__playwright__browser_navigate(url: "http://localhost:<port>/my-page")`
2. Snapshot: `mcp__playwright__browser_snapshot()` — verify key elements present
3. Console: `mcp__playwright__browser_console_messages(level: "error")` — no errors
4. Click: `mcp__playwright__browser_click(ref: "<button-ref>")` — verify interaction
5. Screenshot: `mcp__playwright__browser_take_screenshot()` — visual check

**Expected:** Page shows [specific content], no console errors, interactions work
```

#### Command-Line Self-Test

```markdown
### Test: Verify [feature] works correctly

**Method:** Run project test suite
**When:** After implementing the feature

\`\`\`bash
make test
# Expected: All tests pass
\`\`\`
```

#### Linting & Type Checking: Universal Self-Test

```markdown
### Test: Code passes quality gates

**Method:** make lint / make format / tsc
**When:** After each flight leg, before handoff

\`\`\`bash
make lint
make format
\`\`\`

**Expected:** No errors. Warnings are acceptable if pre-existing.
```

### Post-Flight Checks Guidance

Each flight leg's Post-Flight Checks section should describe what the agent needs to verify when it runs `/post-flight-checks` at the end of the leg. The checks should be:

- **Spec-driven**: Derived from what the flight leg is supposed to build
- **Concrete**: Exact commands, URLs, or steps — not vague "verify it works"
- **Automatable**: Prefer curl, Playwright, `make test`, CLI commands over manual inspection

The agent will use these checks during `/post-flight-checks` to plan and execute verification tests in a fix-and-retry loop before handing off.

---

## Phase 4: Write the Plan Document

Create a comprehensive plan file at `docs/plans/<YYYY-MM-DD-HHmm>-<feature-name>.md`.

The filename MUST include a timestamp with date, hours, and minutes. Generate it with:

```bash
date +"%Y-%m-%d-%H%M"
# Example output: 2026-02-06-1229
```

Example filenames:

- `docs/plans/2026-02-06-1229-user-profile.md`
- `docs/plans/2026-02-06-1430-daily-report-retention.md`

### Plan Document Template

```markdown
# [Feature Name] Implementation Plan

**Flight ID:** fp-<YYYY-MM-DD-HHmm>-<feature-name>

## Overview

[1-2 sentences describing the feature and its purpose]

## Affected Areas

- [ ] `<directory1>/` — [what changes]
- [ ] `<directory2>/` — [what changes]

## Design References

- Relevant design skills for affected areas
- Existing similar feature: `path/to/similar/code.ts`

---

## Flight Leg 1: [Phase Name]

### Tasks

- [ ] [Task 1 description]
  - Files: `path/to/file.ts`
  - Pattern: Follow [existing pattern] from `path/to/example.ts`
- [ ] [Task 2 description]
  - Files: `path/to/file.ts`
  - Details: [specific implementation notes]
- [ ] [Task 3 description]

### Post-Flight Checks for Flight Leg 1

- [ ] **Quality gates:** `make format && make test` — all pass
- [ ] **[Behavior verification]:** [curl command / Playwright steps / CLI check]
  - Expected: [specific expected outcome]
  - How to test: [exact commands or steps]
  - Fail if: [what failure looks like]

### [HANDOFF] Review Flight Leg 1

Human reviews: [what to look for]

---

## Flight Leg 2: [Phase Name]

### Tasks

- [ ] [Task 4 description]
- [ ] [Task 5 description]

### Post-Flight Checks for Flight Leg 2

- [ ] **[Behavior verification]:** [exact commands or steps]
  - Expected: [outcome]
  - How to test: [curl, Playwright, make test, etc.]

### [HANDOFF] Review Flight Leg 2

Human reviews: [what to look for]

---

## Flight Leg N: Final Verification

### Tasks

- [ ] Run full test suite: `make test`
- [ ] Run linter: `make lint`
- [ ] Verify no unused exports or dead code

### Post-Flight Checks for Final Verification

- [ ] **End-to-end test:** [comprehensive test that verifies the whole feature]
  - Steps: [detailed steps]
  - Expected: [complete expected outcome]

### [HANDOFF] Final Review

Human reviews: Complete feature before merge

---

## Rollback Plan

[How to undo these changes if needed]

## Notes

- [Important decisions made during planning]
- [Risks or concerns]
- [Dependencies on external systems]
```

---

## Phase 5: Present the Flight Plan

Present the complete plan to the user for approval:

```
## Flight Plan Filed: [Feature Name]

**Plan Document:** docs/plans/<YYYY-MM-DD-HHmm>-<feature-name>.md
**Flight ID:** fp-<YYYY-MM-DD-HHmm>-<feature-name>
**Branch:** [current branch]
**Total Flight Legs:** [N]
**Total Tasks:** [N] (including [N] handoffs)

### Summary

**Flight Leg 1: [Phase Name]** — [N] tasks
[Brief description of what gets built and how it's tested]

**Flight Leg 2: [Phase Name]** — [N] tasks
[Brief description]

...

### Test Strategy

| Area      | Method                | Tool           |
| --------- | --------------------- | -------------- |
| [area 1]  | [test method]         | [tool]         |
| [area 2]  | [test method]         | [tool]         |
| All       | Lint + type check     | make lint      |

### Next Steps

The plan is saved at `docs/plans/<YYYY-MM-DD-HHmm>-<feature-name>.md`.

To proceed with implementation:
1. Review and approve this plan
2. Run `/pre-flight-checks docs/plans/<YYYY-MM-DD-HHmm>-<feature-name>.md` to create bd tasks (will use Flight ID: fp-<YYYY-MM-DD-HHmm>-<feature-name>)
3. Run `/take-off docs/plans/<YYYY-MM-DD-HHmm>-<feature-name>.md` to begin execution

Please review the plan. Would you like any changes before we proceed?
```

**Wait for approval.** Do NOT proceed to implementation.

---

## Checklist

### Before Starting

- [ ] Preliminary plan received from user
- [ ] Affected areas identified

### During Planning

- [ ] Design skills read for each affected area
- [ ] Existing code examined for patterns
- [ ] Dependencies mapped
- [ ] Test data and endpoints identified
- [ ] Post-flight checks written for every flight leg
- [ ] Plan document written to `docs/plans/`

### Handoff to User

- [ ] Complete plan presented
- [ ] Test strategy summarized
- [ ] Next steps include `/pre-flight-checks` then `/take-off`
- [ ] User approval requested
- [ ] NOT proceeding until approved

---

## Anti-Patterns

| Anti-Pattern              | Problem                       | Fix                                             |
| ------------------------- | ----------------------------- | ----------------------------------------------- |
| Skipping design skills    | Misses required patterns      | ALWAYS read design skills for affected areas    |
| No post-flight checks     | Agent can't verify its work   | Every flight leg needs testable verification    |
| Vague test steps          | Agent won't know if it passed | Write exact commands with expected output       |
| Manual-only testing       | Agent can't test autonomously | Prefer curl, Playwright MCP, make test          |
| Too few handoffs          | Runaway implementation        | One handoff per logical phase                   |
| Too many tasks per leg    | Long gaps between reviews     | Keep flight legs to 3-5 tasks max               |
| Not reading existing code | Plan doesn't match reality    | Examine code BEFORE writing the plan            |
| Giant tasks               | Context bloat, errors         | Split to under 2 minutes each                   |

---

## Related Skills

| Skill                | Relationship                                          |
| -------------------- | ----------------------------------------------------- |
| `/pre-flight-checks` | Next step: creates bd tasks from the plan             |
| `/take-off`          | Executes the bd tasks created by `/pre-flight-checks` |
| `/handoff-task`      | Handoff format used at checkpoints during `/take-off` |
| `/resume-handoff`    | Resume work from a previous handoff checkpoint        |
| `/post-flight-checks`| Runs verification tests at end of each flight leg     |
| `/land-the-plane`    | End session with commit and push                      |

### Typical Flow

```
/file-a-flight-plan          ← YOU ARE HERE
  ├── Read preliminary plan
  ├── Identify affected areas
  ├── Read design skills for each area
  ├── Examine existing code deeply
  ├── Design post-flight checks for each leg
  ├── Write plan to docs/plans/
  └── Present flight plan for approval

Human approves plan

/pre-flight-checks            ← Next step
  ├── Create bd tasks from the plan
  ├── Set up dependencies
  └── Add [HANDOFF] checkpoints

/take-off                     ← Execution
  ├── Execute tasks flight leg by flight leg
  ├── Run /post-flight-checks
  └── STOP at each [HANDOFF] for human review
```
