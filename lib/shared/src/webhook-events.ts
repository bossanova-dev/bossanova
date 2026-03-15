// --- GitHub Webhook Events (subset relevant to Bossanova) ---

export interface WebhookPullRequestEvent {
  type: "pull_request";
  action:
    | "opened"
    | "closed"
    | "reopened"
    | "synchronize"
    | "edited"
    | "review_requested";
  repoFullName: string;
  prNumber: number;
  prUrl: string;
  headBranch: string;
  baseBranch: string;
  merged: boolean;
}

export interface WebhookCheckRunEvent {
  type: "check_run";
  action: "completed" | "created" | "rerequested";
  repoFullName: string;
  checkName: string;
  conclusion:
    | "success"
    | "failure"
    | "neutral"
    | "cancelled"
    | "timed_out"
    | "action_required"
    | "skipped"
    | null;
  headSha: string;
  prNumbers: number[];
}

export interface WebhookCheckSuiteEvent {
  type: "check_suite";
  action: "completed" | "requested" | "rerequested";
  repoFullName: string;
  conclusion:
    | "success"
    | "failure"
    | "neutral"
    | "cancelled"
    | "timed_out"
    | "action_required"
    | "skipped"
    | null;
  headSha: string;
  prNumbers: number[];
}

export interface WebhookPullRequestReviewEvent {
  type: "pull_request_review";
  action: "submitted" | "edited" | "dismissed";
  repoFullName: string;
  prNumber: number;
  state: "approved" | "changes_requested" | "commented" | "dismissed";
  body: string | null;
}

export type WebhookEvent =
  | WebhookPullRequestEvent
  | WebhookCheckRunEvent
  | WebhookCheckSuiteEvent
  | WebhookPullRequestReviewEvent;

// --- Daemon Events (simplified events sent to daemon) ---

export interface DaemonCheckFailedEvent {
  type: "check_failed";
  sessionId: string;
  payload: {
    checkName: string;
    headSha: string;
    prNumber: number;
  };
}

export interface DaemonPrUpdatedEvent {
  type: "pr_updated";
  sessionId: string;
  payload: {
    action: string;
    prNumber: number;
    merged: boolean;
  };
}

export interface DaemonConflictDetectedEvent {
  type: "conflict_detected";
  sessionId: string;
  payload: {
    prNumber: number;
    baseBranch: string;
  };
}

export interface DaemonReviewSubmittedEvent {
  type: "review_submitted";
  sessionId: string;
  payload: {
    prNumber: number;
    state: "approved" | "changes_requested" | "commented" | "dismissed";
    body: string | null;
  };
}

export type DaemonEvent =
  | DaemonCheckFailedEvent
  | DaemonPrUpdatedEvent
  | DaemonConflictDetectedEvent
  | DaemonReviewSubmittedEvent;

export type DaemonEventType = DaemonEvent["type"];
