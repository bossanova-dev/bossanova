---
name: boss-handoff
description: Writes a structured handoff document to docs/handoffs/ for flight leg checkpoints. Use when reaching a [HANDOFF] task during pre-flight-checks workflow.
---

# Handoff Task: Flight Leg Checkpoint

> **THIS SKILL ENDS WITH A FILE WRITE, CONTINUE COMMAND, AND /CLEAR**
>
> After completing this skill, you write the handoff to `docs/handoffs/`, output a continue command, and run `/clear`.
> The next flight leg will be picked up via `/boss-resume` in a fresh context.

"Handoff" is the checkpoint process that happens at the end of each flight leg. Like a pilot's handoff between control towers, this ensures clean transitions with full context preservation.

---

## When to Use This Skill

Use handoff-task when:

- You reach a `[HANDOFF]` task in your bd task list
- Completing a flight leg during pre-flight-checks workflow
- The user requests a status checkpoint
- Before context compaction to preserve state

### How to Detect a Handoff Task

When `bd ready` or `bd list` shows a task with `[HANDOFF]` in the title, you MUST:

1. **INVOKE this skill** - Do not just close the task with `bd close`
2. **Run post-flight checks** - This skill handles that for you
3. **Write the handoff to file** - This skill handles that for you
4. **Output continue command and /clear** - Do not continue executing tasks

**❌ VIOLATION - Never do this when you see a [HANDOFF] task:**

```bash
bd close beads-xxx   # WRONG - skips post-flight checks and documentation
```

**✅ CORRECT - Always invoke the skill:**

```
/boss-handoff
```

---

## MANDATORY: Post-Flight Checks FIRST

> **You MUST run `/boss-verify` BEFORE creating the handoff document.**
> Do NOT skip this step. Do NOT proceed if checks fail.

### Step 0: Run Post-Flight Checks

Before ANY other handoff steps, invoke `/boss-verify`. This will:

1. Read the plan/spec for the current flight leg
2. Run quality gates (`make format && make test`)
3. Plan and execute spec-driven verification tests
4. Fix issues and iterate until all checks pass
5. Declare confidence that the flight leg matches the spec

**Do NOT proceed** to the handoff document until `/boss-verify` passes.

---

## Handoff Document Structure

When creating a handoff, write the following structured document to a file in `docs/handoffs/`:

### Template

```
## Handoff: [Flight Leg Name]

**Date:** [Current date/time]
**Branch:** [Current git branch]
**Flight ID:** [fp-YYYY-MM-DD-HHmm-feature-name - REQUIRED]
**Planning Doc:** [Path to original plan, e.g., docs/plans/my-feature.md - REQUIRED]
**bd Issues Completed:** [List of closed issue IDs]

### Tasks Completed
- [Task 1 with bd issue ID]
- [Task 2 with bd issue ID]
- ...

### Files Changed
- `path/to/file.ts:12-45` - [Brief description of change]
- `path/to/another.ts:8` - [Brief description]
- ...

### Learnings & Notes
- [Important pattern discovered]
- [Root cause of issue found]
- [Key decision made and why]

### Issues Encountered
- [Problem 1 and how it was resolved]
- [Problem 2 - still open, see bd issue beads-XXX]

### Next Steps (Flight Leg N+1)
- [Next task from bd ready]
- [Following tasks in sequence]

### Resume Command
To continue this work:
1. Run `bd ready --label "flight:<flight-id>"` to see available tasks for this flight
2. Review files: [list critical files to read first]
```

---

## Process

### Step 1: Identify Flight ID and Planning Documentation

**CRITICAL — DO NOT SKIP:** Every handoff MUST include both:

1. **Flight ID** - For task isolation across parallel work streams
2. **Planning Doc path** - Source of truth for requirements

These MUST survive across ALL handoffs until the work is fully complete. Without them, future sessions lose context and tasks mix between flights.

**How to find/derive these values:**

- **First handoff:**
  - Look for the plan file in `docs/plans/` that corresponds to the current work
  - Derive Flight ID from filename: `docs/plans/2026-02-06-1229-user-profile.md` → `fp-2026-02-06-1229-user-profile`
  - If you don't know the path, search for it: `ls docs/plans/`
- **Subsequent handoffs:**
  - Copy the `**Flight ID:**` line verbatim from the previous handoff document
  - Copy the `**Planning Doc:**` line verbatim from the previous handoff document
- **Example:**
  - `**Flight ID:** fp-2026-02-06-1229-user-profile`
  - `**Planning Doc:** docs/plans/2026-02-06-1229-user-profile.md`

If no plan exists in `docs/plans/`, note "N/A - ad-hoc task" for both fields, but strongly prefer having a written plan before starting work.

### Step 2: Gather Information

Run these commands to collect handoff data:

```bash
git status                    # Current state
git branch --show-current     # Branch name
git diff --name-only HEAD~N   # Files changed (adjust N for commits in this leg)
bd list --status=in_progress  # Your current work
bd ready                      # What's next
```

### Step 3: Document Completed Tasks

List each task from the flight leg with its bd issue ID:

```
### Tasks Completed

- beads-001: Add UserProfile type to types file
- beads-002: Create UserProfile component skeleton
- beads-003: Implement UserProfile display logic
- beads-004: Add UserProfile to exports
```

### Step 4: List File Changes

Use `file:line` format for precise references:

```
### Files Changed

- `src/types/user.ts:15-32` - Added UserProfile interface
- `src/components/UserProfile.tsx:1-87` - New component
- `src/components/index.ts:12` - Added export
```

**Guidelines:**

- Include line numbers for new/modified code
- Brief description of what changed
- Avoid large code blocks - references are enough

### Step 5: Capture Learnings

Document anything the next agent (or you in a new session) should know:

```
### Learnings & Notes

- UserProfile follows the same pattern as TeamProfile in src/components/TeamProfile.tsx
- The types file uses a specific naming convention: I[Entity]Props for component props
- Tests should use the mock factory pattern from src/test/factories/
```

### Step 6: Note Issues

Be explicit about problems - resolved or open:

```
### Issues Encountered

- TypeScript error with optional chaining - resolved by updating tsconfig target
- Performance concern with re-renders - filed as beads-015 for future optimization
```

### Step 7: Define Next Steps

Pull from bd to show what's coming:

```
### Next Steps (Flight Leg 2: Testing)

- beads-006: Add UserProfile type tests
- beads-007: Add UserProfile component tests
- beads-008: Add hook tests
- beads-009: [HANDOFF] Review tests
```

### Step 8: Write to File, Output Continue Command, and /clear

After generating the handoff content:

1. **Close the handoff task:** `bd close <handoff-task-id>`
2. **Create the `docs/handoffs/` directory** if it doesn't exist: `mkdir -p docs/handoffs`
3. **Write the handoff to a file** using the naming convention:

   ```
   docs/handoffs/YYYY-MM-DD-HHMM-descriptive-title.md
   ```

   Example: `docs/handoffs/2026-02-09-1131-core-types-implementation.md`

   The title should be a kebab-case summary of the flight leg (not the full feature name).

4. **Output the continue command** to the user:
   ```
   continue with: /boss-resume docs/handoffs/YYYY-MM-DD-HHMM-descriptive-title.md
   ```
5. **Run `/clear`** to clear the context window

**✅ THE CORRECT PATTERN:**

```
Flight Leg 1 complete.

**Post-flight checks passed:** quality gates ✓, spec verification ✓

continue with: /boss-resume docs/handoffs/2026-02-09-1131-core-types-implementation.md
```

Then run `/clear`.

**Do NOT continue executing tasks.** The next flight leg will be picked up by `/boss-resume` in a fresh context.

---

## Example Complete Handoff

Written to `docs/handoffs/2026-02-06-1430-core-implementation.md`:

```markdown
## Handoff: Flight Leg 1 - Core Implementation

**Date:** 2025-01-15 14:30 UTC
**Branch:** feature/user-profile
**Flight ID:** fp-2026-02-06-1430-user-profile
**Planning Doc:** docs/plans/2026-02-06-1430-user-profile.md
**bd Issues Completed:** beads-001, beads-002, beads-003, beads-004

### Tasks Completed

- beads-001: Add UserProfile type to types file
- beads-002: Create UserProfile component skeleton
- beads-003: Implement UserProfile display logic
- beads-004: Add UserProfile to exports

### Files Changed

- `src/types/user.ts:15-32` - Added UserProfile and UserProfileProps interfaces
- `src/components/UserProfile.tsx:1-87` - New component with avatar, name, bio display
- `src/components/index.ts:12` - Added UserProfile export
- `src/hooks/useUserProfile.ts:1-45` - New hook for fetching profile data

### Learnings & Notes

- Followed existing pattern from TeamProfile component
- Used the useQuery hook pattern from src/hooks/useTeam.ts
- Avatar component requires explicit size prop (not responsive by default)

### Issues Encountered

- None - implementation straightforward

### Next Steps (Flight Leg 2: Testing)

- beads-006: Add UserProfile type tests
- beads-007: Add UserProfile component tests
- beads-008: Add useUserProfile hook tests
- beads-009: [HANDOFF] Review tests

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-02-06-1430-user-profile"` - should show beads-006
2. Review files: src/components/UserProfile.tsx, src/hooks/useUserProfile.ts
```

Then output:

```
continue with: /boss-resume docs/handoffs/2026-02-06-1430-core-implementation.md
```

Then run `/clear`.

---

## Checklist

Before completing a handoff:

- [ ] **Post-flight checks passed** (`/boss-verify` — quality gates + spec verification)
- [ ] **Flight ID included** (REQUIRED — must survive across all handoffs)
- [ ] **Planning doc path from `docs/plans/` included** (REQUIRED — must survive across all handoffs)
- [ ] All flight leg tasks closed in bd
- [ ] Files changed listed with line references
- [ ] Learnings documented for context recovery
- [ ] Issues noted (resolved and open)
- [ ] Next steps pulled from bd (use `--label` filter)
- [ ] Handoff task itself closed
- [ ] **Handoff written to `docs/handoffs/YYYY-MM-DD-HHMM-title.md`**
- [ ] **Output `continue with: /boss-resume <path>`**
- [ ] **Run `/clear`**

---

## Anti-Patterns

| Anti-Pattern                    | Problem                           | Fix                                                 |
| ------------------------------- | --------------------------------- | --------------------------------------------------- |
| **Skipping post-flight checks** | **Broken code at handoff**        | **Run `/boss-verify` FIRST, fix until pass**        |
| **Not writing to file**         | **Lost context across clears**    | **Write to `docs/handoffs/` ALWAYS**                |
| **Not running /clear**          | **Context bloat**                 | **ALWAYS /clear after writing handoff**             |
| **Continuing past handoff**     | **Context bloat, lost structure** | **Write file, output continue, /clear — that's it** |
| Missing Flight ID               | Tasks mix between flights         | ALWAYS include Flight ID                            |
| Missing planning doc            | Lost original requirements        | ALWAYS include `docs/plans/` path                   |
| Large code blocks               | Bloats handoff, stale quickly     | Use `file:line` references                          |
| Vague descriptions              | Lost context                      | Be specific: what, where, why                       |
| Skipping learnings              | Repeating mistakes                | Always document discoveries                         |
| No next steps                   | Unclear continuation              | Pull from `bd ready --label "flight:..."`           |

---

## Related Skills

| Skill                | Relationship                        |
| -------------------- | ----------------------------------- |
| `/boss-plan`         | Create plan before implementation   |
| `/boss-create-tasks` | Create bd tasks from plan           |
| `/boss-verify`       | Verify flight leg before handoff    |
| `/boss-implement`    | Execute tasks, stopping at handoffs |
| `/boss-resume`       | Resume work from this handoff       |
| `/boss-finalize`     | End session with commit and push    |
