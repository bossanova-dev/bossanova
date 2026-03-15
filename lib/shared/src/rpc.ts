import type { Attempt, Repo, Session } from "./types.js";

// --- JSON-RPC 2.0 Envelope ---

export interface JsonRpcRequest<P = unknown> {
  jsonrpc: "2.0";
  method: string;
  params?: P;
  id: string | number;
}

export interface JsonRpcResponse<R = unknown> {
  jsonrpc: "2.0";
  id: string | number;
  result?: R;
  error?: JsonRpcError;
}

export interface JsonRpcError {
  code: number;
  message: string;
  data?: unknown;
}

// Standard JSON-RPC error codes
export const RpcErrorCode = {
  ParseError: -32700,
  InvalidRequest: -32600,
  MethodNotFound: -32601,
  InvalidParams: -32602,
  InternalError: -32603,
} as const;

// --- Context Resolution ---

export interface ContextResolveParams {
  cwd: string;
}

export type ContextResolveResult =
  | { type: "session"; sessionId: string; repoId: string }
  | { type: "repo"; repoId: string }
  | { type: "unregistered_repo"; localPath: string; originUrl: string }
  | { type: "none" };

// --- Repo Methods ---

export interface RepoRegisterParams {
  localPath: string;
  setupScript?: string;
}

export type RepoRegisterResult = Repo;

// eslint-disable-next-line @typescript-eslint/no-empty-interface
export interface RepoListParams {}

export type RepoListResult = Repo[];

export interface RepoRemoveParams {
  repoId: string;
}

export interface RepoRemoveResult {
  removed: boolean;
}

export interface RepoListPrsParams {
  repoId: string;
}

export interface RepoListPrsResult {
  prs: Array<{
    number: number;
    title: string;
    headBranch: string;
    author: string;
    url: string;
  }>;
}

// --- Session Methods ---

export interface SessionListParams {
  repoId?: string;
}

export type SessionListResult = Session[];

export interface SessionCreateParams {
  repoId: string;
  title: string;
  plan: string;
  prNumber?: number;
}

export type SessionCreateResult = Session;

export interface SessionGetParams {
  sessionId: string;
}

export type SessionGetResult = Session;

export interface SessionAttachParams {
  sessionId: string;
}

export interface SessionAttachResult {
  attached: boolean;
}

export interface SessionStopParams {
  sessionId: string;
}

export interface SessionStopResult {
  stopped: boolean;
}

export interface SessionPauseParams {
  sessionId: string;
}

export interface SessionPauseResult {
  paused: boolean;
}

export interface SessionResumeParams {
  sessionId: string;
}

export interface SessionResumeResult {
  resumed: boolean;
}

export interface SessionRetryParams {
  sessionId: string;
}

export interface SessionRetryResult {
  retried: boolean;
}

export interface SessionCloseParams {
  sessionId: string;
}

export interface SessionCloseResult {
  closed: boolean;
}

export interface SessionRemoveParams {
  sessionId: string;
}

export interface SessionRemoveResult {
  removed: boolean;
}

export interface SessionLogsParams {
  sessionId: string;
  tail?: number;
}

export interface SessionLogsResult {
  lines: string[];
}

export interface SessionAttemptsParams {
  sessionId: string;
}

export type SessionAttemptsResult = Attempt[];

// --- RPC Method Map ---

export interface RpcMethods {
  "context.resolve": {
    params: ContextResolveParams;
    result: ContextResolveResult;
  };
  "repo.register": { params: RepoRegisterParams; result: RepoRegisterResult };
  "repo.list": { params: RepoListParams; result: RepoListResult };
  "repo.remove": { params: RepoRemoveParams; result: RepoRemoveResult };
  "repo.listPrs": { params: RepoListPrsParams; result: RepoListPrsResult };
  "session.list": { params: SessionListParams; result: SessionListResult };
  "session.create": {
    params: SessionCreateParams;
    result: SessionCreateResult;
  };
  "session.get": { params: SessionGetParams; result: SessionGetResult };
  "session.attach": {
    params: SessionAttachParams;
    result: SessionAttachResult;
  };
  "session.stop": { params: SessionStopParams; result: SessionStopResult };
  "session.pause": { params: SessionPauseParams; result: SessionPauseResult };
  "session.resume": {
    params: SessionResumeParams;
    result: SessionResumeResult;
  };
  "session.retry": { params: SessionRetryParams; result: SessionRetryResult };
  "session.close": { params: SessionCloseParams; result: SessionCloseResult };
  "session.remove": {
    params: SessionRemoveParams;
    result: SessionRemoveResult;
  };
  "session.logs": { params: SessionLogsParams; result: SessionLogsResult };
  "session.attempts": {
    params: SessionAttemptsParams;
    result: SessionAttemptsResult;
  };
}

export type RpcMethod = keyof RpcMethods;
