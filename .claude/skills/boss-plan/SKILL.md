---
name: pre-flight-checks
description: Creates a structured task breakdown using bd before starting implementation. Use when planning complex tasks or multi-step features.
---

# Pre-Flight Checks: Task Planning Workflow

> ⚠️ **CRITICAL: PLANNING ONLY**
>
> This skill creates a task breakdown. You **MUST NOT** start implementing tasks.
> After creating the plan, **STOP** and ask the user to review before proceeding.

> 🛑 **CRITICAL: HANDOFF TASKS REQUIRE SPECIAL HANDLING**
>
> **NEVER close a `[HANDOFF]` task directly with `bd close`.**
> When you reach a `[HANDOFF]` task during execution:
>
> 1. **INVOKE** the `/boss-handoff` skill (it runs post-flight checks and creates documentation)
> 2. **STOP** and wait for explicit user approval
> 3. **DO NOT CONTINUE** until the user says "yes", "continue", or similar

"Pre-flight checks" is the task decomposition process that happens BEFORE you start coding. Like a pilot's checklist before takeoff, this ensures you have a clear flight plan with defined checkpoints.

---

## When to Use This Skill

Use pre-flight checks when:

- Starting a non-trivial implementation
- A plan has multiple steps or phases
- Work could take more than 10 minutes
- You need user approval at key milestones

---

## Flight ID Isolation

When working from a plan file in `docs/plans/`, derive the **Flight ID** from the filename to isolate tasks:

```
Plan file: docs/plans/2026-02-06-1229-user-profile.md
Flight ID: fp-2026-02-06-1229-user-profile
Label:     flight:fp-2026-02-06-1229-user-profile
```

**All tasks created for this plan MUST include the flight label:**

```bash
bd create --title="Add UserProfile type" --type=task --priority=2 --labels "flight:fp-2026-02-06-1229-user-profile"
```

This ensures tasks from different flight plans don't mix when running `bd ready` or `bd list`.

---

## Core Principles

### 1. Small, Discrete Tasks

Each task should be completable in under 2 minutes. If it would take longer:

- Split it into sub-tasks
- Each sub-task should be independently completable
- Use dependencies (`bd dep add`) to establish order

**Why?** Small tasks keep context lightweight and focused. Long-running tasks accumulate context that becomes stale and increases risk of errors.

### 2. Logical Groupings with Handoffs (MANDATORY)

Group related tasks into "flight legs" - coherent units of work that form natural stopping points. You **MUST** insert a `[HANDOFF]` task at the end of each flight leg that:

- Pauses development
- Documents completed work
- Requests user review before continuing

**MANDATORY RULES:**

- Every flight leg **MUST** end with a `[HANDOFF]` task
- A flight leg **CANNOT** have more than 5 tasks (excluding the handoff)
- The **final task** in any breakdown **MUST** be a `[HANDOFF]`

**Why?** Handoffs prevent runaway agent behavior and keep the user in control. They also create natural checkpoints for context recovery.

### 3. Task Granularity Guidelines

| Task Size    | Example                       | Action                          |
| ------------ | ----------------------------- | ------------------------------- |
| ~30 seconds  | Add an import statement       | Single task                     |
| ~1-2 minutes | Implement a small function    | Single task                     |
| ~5 minutes   | Create a component with props | Split into 2-3 tasks            |
| ~10+ minutes | Build a full feature          | Split into multiple flight legs |

---

## Workflow

### Step 1: Analyze the Plan

Review the implementation plan and identify:

- Distinct implementation steps
- Natural grouping boundaries (flight legs)
- Dependencies between steps

### Step 2: Create Task Breakdown

For each step, create a bd issue **with the flight label**:

```bash
# Derive Flight ID from plan filename (e.g., docs/plans/2026-02-06-1229-user-profile.md)
# Flight ID: fp-2026-02-06-1229-user-profile

# Create tasks for first flight leg
bd create --title="Add UserProfile type to types file" --type=task --priority=2 --labels "flight:fp-2026-02-06-1229-user-profile"
bd create --title="Create UserProfile component skeleton" --type=task --priority=2 --labels "flight:fp-2026-02-06-1229-user-profile"
bd create --title="Implement UserProfile display logic" --type=task --priority=2 --labels "flight:fp-2026-02-06-1229-user-profile"
bd create --title="Add UserProfile to exports" --type=task --priority=2 --labels "flight:fp-2026-02-06-1229-user-profile"
bd create --title="[HANDOFF] Run /boss-handoff skill and STOP - DO NOT CONTINUE" --type=task --priority=2 --labels "flight:fp-2026-02-06-1229-user-profile"
```

**CRITICAL: Handoff Task Format**

All handoff tasks MUST use this exact title format:

```
[HANDOFF] Run /boss-handoff skill and STOP - DO NOT CONTINUE
```

This ensures the agent knows to:

1. Invoke the `/boss-handoff` skill
2. **STOP completely** after the handoff
3. **NOT continue** to the next flight leg without user approval

### Step 3: Set Up Dependencies

Establish the execution order:

```bash
# Assuming tasks are beads-001 through beads-005
bd dep add beads-002 beads-001  # Component depends on type
bd dep add beads-003 beads-002  # Logic depends on skeleton
bd dep add beads-004 beads-003  # Export depends on implementation
bd dep add beads-005 beads-004  # Handoff depends on all prior work
```

### Step 4: STOP - Request User Approval

**DO NOT proceed to execute tasks.** This skill is planning-only.

1. Present the complete task breakdown to the user
2. Show the handoff points clearly marked
3. Ask: "Does this flight plan look correct? Ready to begin implementation?"
4. **Wait for explicit user approval** before any implementation work

---

## Execution (After User Approval)

> **Note:** The following steps are for AFTER the user approves the plan.
> Do NOT proceed here until you have explicit approval.

> 🛑 **ONE FLIGHT LEG AT A TIME - NO EXCEPTIONS**
>
> You **MUST** only execute tasks within the **current flight leg** (up to and including the next `[HANDOFF]` task).
> After completing a flight leg and its handoff, you **MUST STOP** and wait for user approval.
> You are **FORBIDDEN** from starting the next flight leg without explicit user permission.
> This applies even if the user previously approved the plan — each flight leg requires its own go-ahead.

### Step 5: Execute the Current Flight Leg Only

Work through tasks **only until the next `[HANDOFF]`**, filtering by flight label:

```bash
bd ready --label "flight:fp-2026-02-06-1229-user-profile"  # Find next available task for this flight
bd update <id> --status=in_progress                        # Claim it
# ... do the work ...
bd close <id>                                              # Mark complete
# Repeat ONLY for tasks in the current flight leg
# STOP when you reach a [HANDOFF] task
```

**DO NOT** look ahead to tasks in subsequent flight legs. Focus exclusively on the current flight leg.

### Step 6: Handle Handoff Tasks (MANDATORY STOP)

> 🛑 **MANDATORY STOP - NO EXCEPTIONS**
>
> When you reach a `[HANDOFF]` task, you **MUST STOP COMPLETELY**.
> You are **FORBIDDEN** from continuing to the next flight leg.
> This is **NON-NEGOTIABLE** - violation breaks user trust and control.

When you reach a `[HANDOFF]` task:

1. **STOP ALL WORK IMMEDIATELY** - Do NOT start the next flight leg under ANY circumstances
2. **Mark the handoff task in_progress** - `bd update <id> --status=in_progress`
3. **RUN POST-FLIGHT CHECKS (MANDATORY)** - Run `/boss-verify` to verify the flight leg's work before creating the handoff document. This runs quality gates, plans and executes spec-driven verification tests, and iterates until all checks pass.
   - **Do NOT proceed to handoff until all post-flight checks pass**

4. **Create handoff document** - Use `/boss-handoff` skill to generate a structured handoff with:
   - Completed tasks with bd issue IDs
   - Files changed with `file:line` references
   - Quality gate results (format/test pass status)
   - Learnings and issues encountered
   - Next steps from `bd ready`
5. **Present to user and ASK FOR REVIEW** - Explicitly ask "May I continue with the next flight leg?"
6. **WAIT FOR EXPLICIT APPROVAL** - Do NOT proceed until the user says "yes", "continue", "proceed", or similar

**❌ WRONG - Never do this:**

```
The handoff task is ready. Let me continue with the next phase...
```

**✅ CORRECT - Always do this:**

```
The flight leg is complete. I've created the handoff document.

May I continue with the next flight leg, or would you like to review the changes first?
```

**If you continue past a handoff without user approval, you are violating the core purpose of this workflow.**

> 🛑 **AFTER THE HANDOFF: YOUR TURN IS OVER**
>
> Once you complete a handoff and present it to the user, your current execution is **DONE**.
> You have completed **one flight leg**. Do NOT start the next one.
> The user will invoke `/boss-resume` or tell you to continue when they are ready.
> **There is no scenario where you should execute more than one flight leg in a single run.**

---

## Example: Full Pre-Flight Breakdown

**Plan:** `docs/plans/2026-02-06-1430-user-profile.md`
**Flight ID:** `fp-2026-02-06-1430-user-profile`

**Flight Leg 1: Core Implementation**

```bash
bd create --title="Add UserProfile type" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
bd create --title="Create UserProfile component" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
bd create --title="Add profile fetch hook" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
bd create --title="[HANDOFF] Run /boss-handoff skill and STOP - DO NOT CONTINUE" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
```

**Flight Leg 2: Testing**

```bash
bd create --title="Add UserProfile type tests" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
bd create --title="Add UserProfile component tests" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
bd create --title="Add hook tests" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
bd create --title="[HANDOFF] Run /boss-handoff skill and STOP - DO NOT CONTINUE" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
```

**Flight Leg 3: Integration**

```bash
bd create --title="Add UserProfile route" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
bd create --title="Add navigation link" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
bd create --title="[HANDOFF] Run /boss-handoff skill and STOP - DO NOT CONTINUE" --type=task --priority=2 --labels "flight:fp-2026-02-06-1430-user-profile"
```

**Working with this flight:**

```bash
bd ready --label "flight:fp-2026-02-06-1430-user-profile"        # See tasks for this flight only
bd list --status=open --label "flight:fp-2026-02-06-1430-user-profile"  # All open tasks for this flight
```

---

## Validation (REQUIRED)

Before presenting the plan to the user, verify **ALL** conditions:

- [ ] Every flight leg ends with a `[HANDOFF]` task
- [ ] No flight leg exceeds 5 tasks (excluding the handoff)
- [ ] The final task in the entire breakdown is a `[HANDOFF]`
- [ ] NO implementation work has been started

**If any check fails, fix the breakdown before proceeding.**

---

## Checklist

Before starting implementation:

- [ ] Derived Flight ID from plan filename (fp-<timestamp>-<name>)
- [ ] Analyzed plan and identified steps
- [ ] Created bd tasks for each discrete step **with `--labels "flight:fp-..."`**
- [ ] Each task is <2 minutes of work
- [ ] Tasks grouped into logical flight legs
- [ ] Handoff task added at end of each flight leg
- [ ] Dependencies set up with `bd dep add`
- [ ] User informed of the flight plan

---

## Anti-Patterns

| Anti-Pattern                    | Problem                                     | Fix                                              |
| ------------------------------- | ------------------------------------------- | ------------------------------------------------ |
| Starting work immediately       | Bypasses user approval                      | ALWAYS stop after planning, wait for approval    |
| Tasks too large                 | Context bloat, lost focus                   | Split into <2 minute chunks                      |
| No handoffs                     | Runaway agent, no checkpoints               | Add handoff every 3-5 tasks                      |
| **Skipping handoff**            | **CRITICAL VIOLATION - User loses control** | **ALWAYS stop at handoff tasks - NO EXCEPTIONS** |
| **Multiple flight legs**        | **Agent runs too long, user loses control** | **Execute ONE flight leg, then STOP and wait**   |
| **Skipping post-flight checks** | **Broken code at handoff**                  | **Run `/boss-verify`, fix until all pass**       |
| Serial execution without bd     | No tracking, lost on compaction             | Use bd for ALL task tracking                     |
| Starting without pre-flight     | No clear plan, scope creep                  | Always decompose first                           |
| Missing flight label            | Tasks mix with other flights                | ALWAYS add `--labels "flight:fp-..."`            |

---

## Related Skills

| Skill               | Relationship                                       |
| ------------------- | -------------------------------------------------- |
| `/boss-flight-plan` | Create comprehensive plan before pre-flight checks |
| `/boss-verify`      | Verify flight leg before handoff                   |
| `/boss-implement`   | Execute bd tasks after pre-flight checks           |
| `/boss-handoff`     | Handle handoff checkpoints during execution        |
| `/boss-resume`      | Resume work from a previous handoff                |
| `/boss-land`        | End session with commit and push                   |
