import { execSync } from 'node:child_process';
import type {
  ContextResolveParams,
  ContextResolveResult,
  JsonRpcRequest,
  JsonRpcResponse,
  RepoListResult,
  RepoRegisterParams,
  RepoRegisterResult,
  RepoRemoveParams,
  RepoRemoveResult,
  RpcMethod,
  SessionAttemptsParams,
  SessionAttemptsResult,
  SessionCreateParams,
  SessionCreateResult,
  SessionGetParams,
  SessionGetResult,
  SessionListParams,
  SessionListResult,
  SessionLogsParams,
  SessionLogsResult,
  SessionPauseParams,
  SessionPauseResult,
  SessionRemoveParams,
  SessionRemoveResult,
  SessionResumeParams,
  SessionResumeResult,
  SessionStopParams,
  SessionStopResult,
} from '@bossanova/shared';
import { RpcErrorCode } from '@bossanova/shared';
import { inject, injectable } from 'tsyringe';
import { readLog } from '~/claude/logger';
import type { ClaudeSupervisor } from '~/claude/supervisor';
import type { AttemptStore } from '~/db/attempts';
import type { RepoStore } from '~/db/repos';
import type { SessionStore } from '~/db/sessions';
import type { Logger } from '~/di/container';
import { Service } from '~/di/tokens';
import { resolveContext } from '~/ipc/handlers/context';
import { removeSession, startSession } from '~/session/lifecycle';

type Handler = (params: unknown) => unknown | Promise<unknown>;

@injectable()
export class Dispatcher {
  private handlers: Record<string, Handler> = {};

  constructor(
    @inject(Service.RepoStore) private repos: RepoStore,
    @inject(Service.SessionStore) private sessions: SessionStore,
    @inject(Service.AttemptStore) private attempts: AttemptStore,
    @inject(Service.Logger) private logger: Logger,
    @inject(Service.ClaudeSupervisor) private supervisor: ClaudeSupervisor,
  ) {
    this.registerHandlers();
  }

  async dispatch(request: JsonRpcRequest): Promise<JsonRpcResponse> {
    const handler = this.handlers[request.method];

    if (!handler) {
      return {
        jsonrpc: '2.0',
        id: request.id,
        error: {
          code: RpcErrorCode.MethodNotFound,
          message: `Method not found: ${request.method}`,
        },
      };
    }

    try {
      const result = await handler(request.params ?? {});
      return { jsonrpc: '2.0', id: request.id, result };
    } catch (err) {
      this.logger.error(`RPC error in ${request.method}`, err);
      return {
        jsonrpc: '2.0',
        id: request.id,
        error: {
          code: RpcErrorCode.InternalError,
          message: err instanceof Error ? err.message : 'Internal error',
        },
      };
    }
  }

  getMethodNames(): RpcMethod[] {
    return Object.keys(this.handlers) as RpcMethod[];
  }

  private registerHandlers(): void {
    // Context
    this.handlers['context.resolve'] = (params) =>
      this.handleContextResolve(params as ContextResolveParams);

    // Repos
    this.handlers['repo.register'] = (params) =>
      this.handleRepoRegister(params as RepoRegisterParams);
    this.handlers['repo.list'] = () => this.handleRepoList();
    this.handlers['repo.remove'] = (params) => this.handleRepoRemove(params as RepoRemoveParams);

    // Sessions
    this.handlers['session.list'] = (params) => this.handleSessionList(params as SessionListParams);
    this.handlers['session.create'] = (params) =>
      this.handleSessionCreate(params as SessionCreateParams);
    this.handlers['session.get'] = (params) => this.handleSessionGet(params as SessionGetParams);
    this.handlers['session.remove'] = (params) =>
      this.handleSessionRemove(params as SessionRemoveParams);
    this.handlers['session.stop'] = (params) => this.handleSessionStop(params as SessionStopParams);
    this.handlers['session.pause'] = (params) =>
      this.handleSessionPause(params as SessionPauseParams);
    this.handlers['session.resume'] = (params) =>
      this.handleSessionResume(params as SessionResumeParams);
    this.handlers['session.logs'] = (params) => this.handleSessionLogs(params as SessionLogsParams);
    this.handlers['session.attempts'] = (params) =>
      this.handleSessionAttempts(params as SessionAttemptsParams);
  }

  // --- Context ---

  private async handleContextResolve(params: ContextResolveParams): Promise<ContextResolveResult> {
    return resolveContext(params.cwd, this.repos, this.sessions);
  }

  // --- Repos ---

  private handleRepoRegister(params: RepoRegisterParams): RepoRegisterResult {
    // Resolve git info from local path
    const localPath = execSync('git rev-parse --show-toplevel', {
      cwd: params.localPath,
      encoding: 'utf8',
    }).trim();

    let originUrl = '';
    try {
      originUrl = execSync('git remote get-url origin', {
        cwd: localPath,
        encoding: 'utf8',
      }).trim();
    } catch {
      // No remote configured
    }

    const dirName = localPath.split('/').pop() ?? localPath;

    return this.repos.register({
      displayName: dirName,
      localPath,
      originUrl,
      worktreeBaseDir: `${localPath}/.worktrees`,
      setupScript: params.setupScript ?? null,
    });
  }

  private handleRepoList(): RepoListResult {
    return this.repos.list();
  }

  private handleRepoRemove(params: RepoRemoveParams): RepoRemoveResult {
    const existing = this.repos.get(params.repoId);
    if (!existing) return { removed: false };
    this.repos.remove(params.repoId);
    return { removed: true };
  }

  // --- Sessions ---

  private handleSessionList(params: SessionListParams): SessionListResult {
    return this.sessions.list(params.repoId);
  }

  private async handleSessionCreate(params: SessionCreateParams): Promise<SessionCreateResult> {
    return startSession(this.repos, this.sessions, params, this.supervisor);
  }

  private handleSessionGet(params: SessionGetParams): SessionGetResult {
    const session = this.sessions.get(params.sessionId);
    if (!session) {
      throw new Error(`Session not found: ${params.sessionId}`);
    }
    return session;
  }

  private async handleSessionRemove(params: SessionRemoveParams): Promise<SessionRemoveResult> {
    const removed = await removeSession(this.repos, this.sessions, params.sessionId, this.supervisor);
    return { removed };
  }

  private handleSessionStop(params: SessionStopParams): SessionStopResult {
    try {
      this.supervisor.stop(params.sessionId);
      this.sessions.update(params.sessionId, { state: 'blocked', blockedReason: 'Stopped by user' });
      return { stopped: true };
    } catch {
      return { stopped: false };
    }
  }

  private handleSessionPause(params: SessionPauseParams): SessionPauseResult {
    try {
      this.supervisor.pause(params.sessionId);
      return { paused: true };
    } catch {
      return { paused: false };
    }
  }

  private async handleSessionResume(params: SessionResumeParams): Promise<SessionResumeResult> {
    const session = this.sessions.get(params.sessionId);
    if (!session?.worktreePath) return { resumed: false };

    try {
      await this.supervisor.resume(params.sessionId, session.worktreePath, session.plan);
      this.sessions.update(params.sessionId, { state: 'implementing_plan' });
      return { resumed: true };
    } catch {
      return { resumed: false };
    }
  }

  private handleSessionLogs(params: SessionLogsParams): SessionLogsResult {
    const content = readLog(params.sessionId);
    if (!content) return { lines: [] };

    const allLines = content.split('\n').filter(Boolean);
    if (params.tail && params.tail > 0) {
      return { lines: allLines.slice(-params.tail) };
    }
    return { lines: allLines };
  }

  // --- Attempts ---

  private handleSessionAttempts(params: SessionAttemptsParams): SessionAttemptsResult {
    return this.attempts.listBySession(params.sessionId);
  }
}
