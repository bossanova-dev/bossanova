---
name: boss-resume
description: Resumes work from a handoff file in docs/handoffs/. Reads context, proceeds automatically to next handoff. Use when continuing work after a previous session's handoff checkpoint.
---

# Resume Handoff: Continue From Checkpoint

This skill guides you through resuming work from a handoff document. It ensures context is properly restored and work continues smoothly.

---

## When to Use This Skill

Use resume-handoff when:

- The user provides a handoff file path (e.g., `docs/handoffs/2026-02-09-1131-core-types.md`)
- You need to continue work after a `[HANDOFF]` checkpoint
- Resuming after context compaction or a new session
- The user says "continue", "resume", or references prior work

---

## Phase 1: Read & Analyze

### 1.1 Read the Handoff File

Read the handoff file from the path provided (e.g., `docs/handoffs/2026-02-09-1131-core-types.md`). Note these key fields:

- **Flight ID:** The unique identifier for this flight plan (carry this forward for bd commands)
- **Planning Doc:** The path to the original spec (carry this forward)
- **Branch:** The git branch for this work
- **bd Issues Completed:** What's already done
- **Files Changed:** Code that was modified
- **Learnings & Notes:** Patterns and discoveries to leverage
- **Issues Encountered:** Problems to be aware of
- **Next Steps:** The upcoming tasks

### 1.2 Verify Current State

Run these commands to understand current state, **filtering by the Flight ID from the handoff**:

```bash
git status                                             # Check for uncommitted changes
git branch --show-current                              # Confirm correct branch
bd ready --label "flight:<flight-id-from-handoff>"     # See available tasks for this flight
bd list --status=in_progress --label "flight:<flight-id>"  # Check for claimed but incomplete tasks
```

**Example with Flight ID `fp-2026-02-06-1430-user-profile`:**

```bash
bd ready --label "flight:fp-2026-02-06-1430-user-profile"
bd list --status=in_progress --label "flight:fp-2026-02-06-1430-user-profile"
```

### 1.3 Read Critical Files

Review the files mentioned in the handoff to refresh context:

- Files that were changed
- Files listed in "Resume Command" section

---

## Phase 2: Synthesize and Proceed

Present a brief summary, then proceed directly to execution — no approval needed:

```
## Resuming: [Work Description]

**Flight ID:** [flight ID from handoff]
**Planning Doc:** [path from handoff]
**Branch:** [current branch]

### Previous Progress
- [X completed tasks from handoff]

### Next Actions
1. [First task from bd ready]
2. [Subsequent tasks]
```

**Proceed directly to Phase 3 — no approval needed.**

---

## Phase 3: Execute the Current Flight Leg

> **ONE FLIGHT LEG AT A TIME**
>
> Execute tasks within the **current flight leg** (up to and including the next `[HANDOFF]` task).
> At the handoff, write the handoff to a file, output the continue command, and run `/clear`.

### 3.1 Find Next Task

Filter by the Flight ID from the handoff:

```bash
bd ready --label "flight:<flight-id-from-handoff>"
```

### 3.2 Claim the Task

```bash
bd update <id> --status=in_progress
```

### 3.3 Implement

- Read relevant files first
- Apply learnings from the handoff
- Make minimal, focused changes
- Follow existing patterns

### 3.4 Commit

- Stage changes for the completed task
- Write a conventional commit message
- Keep commits atomic

### 3.5 Close the Task

```bash
bd close <id>
```

### 3.6 Post-Flight Checks (Before Handoff)

Before proceeding to a `[HANDOFF]` task, run `/boss-verify` to verify the flight leg's work. This runs quality gates, plans and executes verification tests based on the plan, and iterates until all checks pass.

### 3.7 Repeat Within Flight Leg or Handoff

- **Regular task next (same flight leg)** -> Return to 3.1
- **`[HANDOFF]` task next** -> Use `/boss-handoff` to write the handoff to a file in `docs/handoffs/`, then output the continue command and run `/clear`

---

## Common Scenarios

### Clean Continuation

Everything matches the handoff. Proceed normally with Phase 3.

### Uncommitted Changes Found

```bash
git status  # Shows uncommitted work
```

Ask the user how to handle:

- Commit the changes?
- Stash them?
- Discard them?

### Diverged Codebase

Files have changed since the handoff. Review the differences:

```bash
git log --oneline -10  # Check recent commits
```

Reconcile any conflicts before proceeding.

### Incomplete Task in Progress

```bash
bd list --status=in_progress  # Shows claimed but unclosed task
```

Either complete it first or ask the user if it should be reset.

### Stale Handoff

Significant time has passed or major changes occurred. Recommend:

- Re-reading the planning doc
- Running `bd list --status=open` to see full scope
- Potentially creating a fresh task breakdown

---

## Guidelines

1. **Thorough Analysis** - Read the complete handoff; don't skim
2. **Verify Before Acting** - Confirm state matches expectations
3. **Leverage Learnings** - Apply patterns from the handoff notes
4. **Maintain Continuity** - Carry forward the planning doc path
5. **Validate Assumptions** - Check that referenced files still exist

---

## Checklist

Before starting:

- [ ] Read complete handoff file
- [ ] Noted Flight ID for bd command filtering
- [ ] Noted planning doc path
- [ ] Verified git branch and status
- [ ] Checked `bd ready --label "flight:..."` for current tasks
- [ ] Reviewed critical files
- [ ] Presented brief summary

During execution:

- [ ] One task at a time
- [ ] Using `--label` filter on all bd commands
- [ ] Commit after each task
- [ ] At `[HANDOFF]`: write to `docs/handoffs/`, output continue command, `/clear`
- [ ] **Only ONE flight leg per run** — handoff and clear at checkpoint

---

## Anti-Patterns

| Anti-Pattern                     | Problem                                | Fix                                                 |
| -------------------------------- | -------------------------------------- | --------------------------------------------------- |
| Skipping handoff review          | Lost context, repeated mistakes        | Read thoroughly first                               |
| **Multiple flight legs per run** | **Agent runs too long, context bloat** | **Execute ONE flight leg, then handoff and /clear** |
| **Not writing handoff to file**  | **Lost context across clears**         | **Write to `docs/handoffs/` ALWAYS**                |
| **Not running /clear**           | **Context bloat**                      | **ALWAYS /clear after writing handoff**             |
| Ignoring learnings               | Repeating solved problems              | Apply documented patterns                           |
| Not using flight label           | Tasks from other flights appear        | ALWAYS use `--label "flight:..."` on bd             |
| Forgetting Flight ID             | Lost task isolation                    | Carry it forward in next handoff                    |
| Forgetting planning doc          | Lost source of truth                   | Carry it forward in next handoff                    |
| Rushing through analysis         | Missed important context               | Take time in Phases 1-2                             |

---

## Related Skills

| Skill                | Relationship                            |
| -------------------- | --------------------------------------- |
| `/boss-plan`         | Create plan before implementation       |
| `/boss-create-tasks` | Create bd tasks from plan               |
| `/boss-verify`       | Verify flight leg before handoff        |
| `/boss-implement`    | Execute tasks, stopping at handoffs     |
| `/boss-handoff`      | Create handoff documents at checkpoints |
| `/boss-finalize`     | End session with commit and push        |
