---
name: take-off
description: Executes pre-existing bd tasks from a plan file, writing handoffs to docs/handoffs/ and clearing context at each checkpoint. Use when bd tasks are already created and ready to work.
---

# Take-Off: Execute Pre-Existing Tasks with Mandatory Handoffs

"Take-off" is the execution phase that happens AFTER pre-flight checks. Like a pilot following their flight plan, you execute the pre-created bd tasks with mandatory stops at each checkpoint.

---

## When to Use This Skill

Use take-off when:

- bd tasks have already been created (via `/pre-flight-checks` or manually)
- You have a plan file path to reference
- You're ready to start executing tasks
- You need structured execution with mandatory human checkpoints

---

## Required Input

This skill expects:

1. **Plan file path** - The path to the plan document (e.g., `docs/plans/2026-02-06-1229-user-profile.md`)
2. **Pre-existing bd tasks** - Tasks should already be created with `[HANDOFF]` tasks as checkpoints

**Deriving the Flight ID:**

From the plan filename, derive the Flight ID by prefixing with `fp-`:

```
Plan file: docs/plans/2026-02-06-1229-user-profile.md
Flight ID: fp-2026-02-06-1229-user-profile
```

This Flight ID is used to filter tasks with `--label "flight:fp-..."`.

If bd tasks don't exist, use `/pre-flight-checks` first to create them.

---

## Core Rules

### MANDATORY HANDOFF AT CHECKPOINT

When you reach a `[HANDOFF]` task, you **MUST**:

1. **STOP all implementation work immediately**
2. **Write the handoff to a file** in `docs/handoffs/`
3. **Output the continue command** for `/resume-handoff`
4. **Run `/clear`** to free context for the next flight leg

**This is non-negotiable.** Do NOT:

- Continue to the next task after a handoff
- Skip the handoff because "it's almost done"
- Output the handoff to chat instead of writing to file
- Batch multiple handoffs together

---

## Phase 1: Initialize

### 1.1 Read the Plan

Read the plan file provided by the user:

```bash
cat docs/plans/[plan-name].md
```

Note the **Planning Doc** path - you'll need this for handoffs.

### 1.2 Check Task State

Filter tasks by flight label to see only tasks for this flight plan:

```bash
bd ready --label "flight:fp-..."                     # See available tasks for this flight
bd list --status=in_progress --label "flight:fp-..."  # Check for incomplete claimed tasks
bd list --status=open --label "flight:fp-..."         # See all open tasks for this flight
```

### 1.3 Verify Handoffs Exist

Confirm `[HANDOFF]` tasks are present:

```bash
bd list --status=open | grep -i handoff
```

If no handoffs exist, **STOP** and inform the user. Handoffs are required for this workflow.

### 1.4 Present Flight Plan and Begin

Show the user the execution plan, then proceed immediately to Flight Leg 1:

```
## Take-Off: [Feature Name]

**Planning Doc:** docs/plans/[plan-name].md
**Flight ID:** fp-[plan-name]
**Branch:** [current branch]

### Flight Plan

**Flight Leg 1:**
- beads-001: [Task 1]
- beads-002: [Task 2]
- beads-003: [HANDOFF] Review Flight Leg 1

**Flight Leg 2:**
- beads-004: [Task 3]
- beads-005: [Task 4]
- beads-006: [HANDOFF] Review Flight Leg 2

[Continue for all flight legs...]

Starting Flight Leg 1 now.
```

**Proceed directly to Phase 2 — no approval needed.**

---

## Phase 2: Execute Flight Leg

### 2.1 Find Next Task

Filter by flight label to see only tasks for this flight:

```bash
bd ready --label "flight:fp-..."
```

### 2.2 Check if Handoff

**If the next task is a `[HANDOFF]` task:**

- Go to **Phase 3: Handoff** immediately
- Do NOT skip ahead to other tasks

**If it's a regular task:**

- Continue to 2.3

### 2.3 Claim the Task

```bash
bd update <id> --status=in_progress
```

### 2.4 Implement

- Read relevant files first
- Make minimal, focused changes
- Follow existing patterns
- Update plan checkboxes if applicable

### 2.5 Commit (Optional)

For substantial changes, commit after completing:

```bash
git add [files]
git commit -m "feat(scope): implement [task description]"
```

### 2.6 Close the Task

```bash
bd close <id>
```

### 2.7 Post-Flight Checks (Before Handoff)

Before proceeding to a `[HANDOFF]` task, run `/post-flight-checks` to verify the flight leg's work. This runs quality gates, plans and executes verification tests based on the plan, and iterates until all checks pass.

### 2.8 Repeat

Return to **2.1** until you reach a `[HANDOFF]` task.

---

## Phase 3: Handoff (Write to File and Clear Context)

When you reach a `[HANDOFF]` task, you **MUST** write the handoff to a file and clear context.

### 3.1 Gather Information

```bash
git status                    # Current state
git diff --name-only HEAD~N   # Files changed (adjust N)
bd list --status=in_progress  # Should be empty except handoff
bd ready                      # What's next after handoff
```

### 3.2 Write Handoff to File

Write the handoff document to `docs/handoffs/` using the naming convention:

```
docs/handoffs/YYYY-MM-DD-HHMM-descriptive-title.md
```

Example: `docs/handoffs/2026-02-09-1131-core-types-implementation.md`

Create the `docs/handoffs/` directory if it doesn't exist. The handoff file should contain:

```markdown
## Handoff: [Flight Leg Name]

**Date:** [Current date/time]
**Branch:** [Current git branch]
**Flight ID:** [fp-... - REQUIRED]
**Planning Doc:** [Path to plan file - REQUIRED]

### Tasks Completed This Flight Leg

- beads-XXX: [Task description]
- beads-XXX: [Task description]
- beads-XXX: [Task description]

### Files Changed

- `path/to/file.ts:12-45` - [Brief description]
- `path/to/another.ts:8` - [Brief description]

### Implementation Notes

- [Key decisions made]
- [Patterns followed]
- [Issues encountered and resolved]

### Current Status

- Tests: [pass/fail/not run]
- Lint: [pass/fail/not run]
- Build: [pass/fail/not run]

### Next Flight Leg

- beads-XXX: [Next task]
- beads-XXX: [Following task]
- beads-XXX: [HANDOFF] [Next handoff]
```

### 3.3 Close the Handoff Task

```bash
bd close <handoff-task-id>
```

### 3.4 Output Continue Command and Clear

After writing the handoff file, output the continue command and run `/clear`:

```
continue with: /resume-handoff docs/handoffs/YYYY-MM-DD-HHMM-descriptive-title.md
```

Then run `/clear` to clear the context window, freeing up context for the next flight leg.

**Do NOT continue executing tasks.** The next flight leg will be picked up by `/resume-handoff` in a fresh context.

---

## Complete Example

```
User: /take-off docs/plans/2026-02-06-1430-user-profile.md

Agent: Let me check the current state...

[Reads plan, derives Flight ID: fp-2026-02-06-1430-user-profile, checks bd tasks]

## Take-Off: User Profile Feature

**Planning Doc:** docs/plans/2026-02-06-1430-user-profile.md
**Flight ID:** fp-2026-02-06-1430-user-profile
**Branch:** feature/user-profile

### Flight Plan

**Flight Leg 1: Core Types**
- beads-001: Add UserProfile interface
- beads-002: Add UserProfileProps type
- beads-003: [HANDOFF] Review types

**Flight Leg 2: Component**
- beads-004: Create UserProfile component
- beads-005: Add display logic
- beads-006: [HANDOFF] Review component

Starting Flight Leg 1 now.

[Runs: bd ready --label "flight:fp-2026-02-06-1430-user-profile"]
[Implements beads-001, beads-002]
[Writes handoff to docs/handoffs/2026-02-06-1045-core-types.md]
[Closes handoff task beads-003]

continue with: /resume-handoff docs/handoffs/2026-02-06-1045-core-types.md

[Runs /clear]
```

---

## Checklist

### Before Starting

- [ ] Plan file path received
- [ ] bd tasks exist and include `[HANDOFF]` tasks
- [ ] Flight plan presented to user

### During Each Flight Leg

- [ ] Working one task at a time
- [ ] Closing tasks after completion
- [ ] Checking for handoff after each task

### At Each Handoff

- [ ] STOPPED implementation work
- [ ] Handoff written to `docs/handoffs/YYYY-MM-DD-HHMM-title.md`
- [ ] Flight ID included
- [ ] Planning doc path included
- [ ] Files changed listed
- [ ] Handoff task closed
- [ ] Output `continue with: /resume-handoff <handoff-file-path>`
- [ ] Run `/clear` to free context

---

## Anti-Patterns

| Anti-Pattern                | Problem                     | Fix                                      |
| --------------------------- | --------------------------- | ---------------------------------------- |
| Skipping handoff            | User loses control          | ALWAYS stop at handoffs                  |
| Not writing handoff to file | Lost context across clears  | Write to `docs/handoffs/` ALWAYS         |
| Not running /clear          | Context bloat               | ALWAYS /clear after handoff              |
| Batching handoffs           | Too much work without check | One handoff at a time                    |
| Missing planning doc        | Lost context                | Always include in handoff                |
| Missing Flight ID           | Lost task isolation         | Always include Flight ID in handoff      |
| Not filtering by flight     | Tasks from other flights    | Use `--label "flight:fp-..."` in bd cmds |
| No handoff tasks in plan    | No checkpoints              | Require handoffs or stop                 |

---

## Related Skills

| Skill                | When to Use                         |
| -------------------- | ----------------------------------- |
| `/pre-flight-checks`  | Create bd tasks if they don't exist |
| `/post-flight-checks` | Verify flight leg before handoff    |
| `/handoff-task`        | Detailed handoff document format    |
| `/resume-handoff`      | Resume from a handoff file          |
| `/land-the-plane`      | End session with commit and push    |
