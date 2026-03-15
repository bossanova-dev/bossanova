import { assign, setup } from "xstate";

// --- State Enum ---

export const SessionState = {
  CreatingWorktree: "creating_worktree",
  StartingClaude: "starting_claude",
  PushingBranch: "pushing_branch",
  OpeningDraftPr: "opening_draft_pr",
  ImplementingPlan: "implementing_plan",
  AwaitingChecks: "awaiting_checks",
  FixingChecks: "fixing_checks",
  GreenDraft: "green_draft",
  ReadyForReview: "ready_for_review",
  Blocked: "blocked",
  Merged: "merged",
  Closed: "closed",
} as const;

export type SessionStateValue =
  (typeof SessionState)[keyof typeof SessionState];

// --- Event Types ---

export type SessionEvent =
  | { type: "WORKTREE_CREATED"; worktreePath: string; branchName: string }
  | { type: "CLAUDE_STARTED"; claudeSessionId: string }
  | { type: "PLAN_COMPLETE" }
  | { type: "BRANCH_PUSHED"; sha: string }
  | { type: "PR_OPENED"; prNumber: number; prUrl: string }
  | { type: "CHECKS_PASSED" }
  | { type: "CHECKS_FAILED" }
  | { type: "CONFLICT_DETECTED" }
  | { type: "REVIEW_SUBMITTED" }
  | { type: "FIX_COMPLETE" }
  | { type: "FIX_FAILED"; reason: string }
  | { type: "BLOCK"; reason: string }
  | { type: "UNBLOCK" }
  | { type: "PR_MERGED" }
  | { type: "PR_CLOSED" };

// --- Context Type ---

export interface SessionContext {
  repoId: string;
  title: string;
  plan: string;
  worktreePath: string | null;
  branchName: string | null;
  baseBranch: string;
  prNumber: number | null;
  prUrl: string | null;
  claudeSessionId: string | null;
  attemptCount: number;
  maxAttempts: number;
  blockedReason: string | null;
  lastCheckState: "pending" | "passed" | "failed" | null;
}

// --- Input Type ---

export interface SessionMachineInput {
  repoId: string;
  title: string;
  plan: string;
  baseBranch: string;
  maxAttempts?: number;
}

// --- Machine Definition ---

export const sessionMachine = setup({
  types: {
    context: {} as SessionContext,
    events: {} as SessionEvent,
    input: {} as SessionMachineInput,
  },
  guards: {
    hasReachedMaxAttempts: ({ context }) =>
      context.attemptCount >= context.maxAttempts,
  },
  actions: {
    incrementAttemptCount: assign({
      attemptCount: ({ context }) => context.attemptCount + 1,
    }),
    setChecksPassed: assign({
      lastCheckState: () => "passed" as const,
    }),
    setChecksFailed: assign({
      lastCheckState: () => "failed" as const,
    }),
    clearBlockedReason: assign({
      blockedReason: () => null,
    }),
  },
}).createMachine({
  id: "session",
  initial: SessionState.CreatingWorktree,
  context: ({ input }) => ({
    repoId: input.repoId,
    title: input.title,
    plan: input.plan,
    baseBranch: input.baseBranch,
    worktreePath: null,
    branchName: null,
    prNumber: null,
    prUrl: null,
    claudeSessionId: null,
    attemptCount: 0,
    maxAttempts: input.maxAttempts ?? 5,
    blockedReason: null,
    lastCheckState: null,
  }),
  states: {
    [SessionState.CreatingWorktree]: {
      on: {
        WORKTREE_CREATED: {
          target: SessionState.StartingClaude,
          actions: assign({
            worktreePath: ({ event }) => event.worktreePath,
            branchName: ({ event }) => event.branchName,
          }),
        },
        BLOCK: {
          target: SessionState.Blocked,
          actions: assign({
            blockedReason: ({ event }) => event.reason,
          }),
        },
      },
    },

    [SessionState.StartingClaude]: {
      on: {
        CLAUDE_STARTED: {
          target: SessionState.ImplementingPlan,
          actions: assign({
            claudeSessionId: ({ event }) => event.claudeSessionId,
          }),
        },
        BLOCK: {
          target: SessionState.Blocked,
          actions: assign({
            blockedReason: ({ event }) => event.reason,
          }),
        },
      },
    },

    [SessionState.ImplementingPlan]: {
      on: {
        PLAN_COMPLETE: {
          target: SessionState.PushingBranch,
        },
        BLOCK: {
          target: SessionState.Blocked,
          actions: assign({
            blockedReason: ({ event }) => event.reason,
          }),
        },
      },
    },

    [SessionState.PushingBranch]: {
      on: {
        BRANCH_PUSHED: {
          target: SessionState.OpeningDraftPr,
        },
        BLOCK: {
          target: SessionState.Blocked,
          actions: assign({
            blockedReason: ({ event }) => event.reason,
          }),
        },
      },
    },

    [SessionState.OpeningDraftPr]: {
      on: {
        PR_OPENED: {
          target: SessionState.AwaitingChecks,
          actions: assign({
            prNumber: ({ event }) => event.prNumber,
            prUrl: ({ event }) => event.prUrl,
          }),
        },
        BLOCK: {
          target: SessionState.Blocked,
          actions: assign({
            blockedReason: ({ event }) => event.reason,
          }),
        },
      },
    },

    [SessionState.AwaitingChecks]: {
      on: {
        CHECKS_PASSED: {
          target: SessionState.GreenDraft,
          actions: "setChecksPassed",
        },
        CHECKS_FAILED: [
          {
            guard: "hasReachedMaxAttempts",
            target: SessionState.Blocked,
            actions: [
              "setChecksFailed",
              assign({
                blockedReason: () => "Max fix attempts reached",
              }),
            ],
          },
          {
            target: SessionState.FixingChecks,
            actions: ["setChecksFailed", "incrementAttemptCount"],
          },
        ],
        CONFLICT_DETECTED: {
          target: SessionState.FixingChecks,
          actions: "incrementAttemptCount",
        },
        REVIEW_SUBMITTED: {
          target: SessionState.FixingChecks,
          actions: "incrementAttemptCount",
        },
        PR_MERGED: { target: SessionState.Merged },
        PR_CLOSED: { target: SessionState.Closed },
        BLOCK: {
          target: SessionState.Blocked,
          actions: assign({
            blockedReason: ({ event }) => event.reason,
          }),
        },
      },
    },

    [SessionState.FixingChecks]: {
      on: {
        FIX_COMPLETE: {
          target: SessionState.AwaitingChecks,
        },
        FIX_FAILED: [
          {
            guard: "hasReachedMaxAttempts",
            target: SessionState.Blocked,
            actions: assign({
              blockedReason: ({ event }) => event.reason,
            }),
          },
          {
            target: SessionState.AwaitingChecks,
          },
        ],
        BLOCK: {
          target: SessionState.Blocked,
          actions: assign({
            blockedReason: ({ event }) => event.reason,
          }),
        },
      },
    },

    [SessionState.GreenDraft]: {
      on: {
        CHECKS_FAILED: {
          target: SessionState.FixingChecks,
          actions: ["setChecksFailed", "incrementAttemptCount"],
        },
        REVIEW_SUBMITTED: {
          target: SessionState.FixingChecks,
          actions: "incrementAttemptCount",
        },
        CONFLICT_DETECTED: {
          target: SessionState.FixingChecks,
          actions: "incrementAttemptCount",
        },
        PR_MERGED: { target: SessionState.Merged },
        PR_CLOSED: { target: SessionState.Closed },
      },
    },

    [SessionState.ReadyForReview]: {
      on: {
        CHECKS_FAILED: {
          target: SessionState.FixingChecks,
          actions: ["setChecksFailed", "incrementAttemptCount"],
        },
        REVIEW_SUBMITTED: {
          target: SessionState.FixingChecks,
          actions: "incrementAttemptCount",
        },
        CONFLICT_DETECTED: {
          target: SessionState.FixingChecks,
          actions: "incrementAttemptCount",
        },
        PR_MERGED: { target: SessionState.Merged },
        PR_CLOSED: { target: SessionState.Closed },
      },
    },

    [SessionState.Blocked]: {
      on: {
        UNBLOCK: {
          target: SessionState.AwaitingChecks,
          actions: "clearBlockedReason",
        },
        PR_CLOSED: { target: SessionState.Closed },
      },
    },

    [SessionState.Merged]: { type: "final" },
    [SessionState.Closed]: { type: "final" },
  },
});
