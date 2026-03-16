import { useEffect, useRef, useState } from 'react'
import { useParams, Link } from 'react-router'
import { create } from '@bufbuild/protobuf'
import { useApi } from '../ApiContext.ts'
import {
  ProxyGetSessionRequestSchema,
  ProxyAttachSessionRequestSchema,
  ProxyStopSessionRequestSchema,
  ProxyPauseSessionRequestSchema,
  ProxyResumeSessionRequestSchema,
  ListDaemonsRequestSchema,
  TransferSessionRequestSchema,
} from '../gen/bossanova/v1/orchestrator_pb.ts'
import type { DaemonInfo } from '../gen/bossanova/v1/orchestrator_pb.ts'
import { SessionState } from '../gen/bossanova/v1/models_pb.ts'
import type { Session } from '../gen/bossanova/v1/models_pb.ts'
import type { Timestamp } from '@bufbuild/protobuf/wkt'

function timestampToDate(ts: Timestamp | undefined): Date | undefined {
  if (!ts) return undefined
  return new Date(Number(ts.seconds) * 1000 + ts.nanos / 1_000_000)
}

const stateLabel: Record<number, string> = {
  [SessionState.CREATING_WORKTREE]: 'Creating Worktree',
  [SessionState.STARTING_CLAUDE]: 'Starting Claude',
  [SessionState.PUSHING_BRANCH]: 'Pushing Branch',
  [SessionState.OPENING_DRAFT_PR]: 'Opening Draft PR',
  [SessionState.IMPLEMENTING_PLAN]: 'Implementing',
  [SessionState.AWAITING_CHECKS]: 'Awaiting Checks',
  [SessionState.FIXING_CHECKS]: 'Fixing Checks',
  [SessionState.GREEN_DRAFT]: 'Green Draft',
  [SessionState.READY_FOR_REVIEW]: 'Ready for Review',
  [SessionState.BLOCKED]: 'Blocked',
  [SessionState.MERGED]: 'Merged',
  [SessionState.CLOSED]: 'Closed',
}

interface LogEntry {
  type: 'output' | 'state' | 'ended'
  text: string
  timestamp?: Date
}

export default function SessionDetail() {
  const { id } = useParams()
  const api = useApi()
  const [session, setSession] = useState<Session | null>(null)
  const [log, setLog] = useState<LogEntry[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [streaming, setStreaming] = useState(false)
  const [actionPending, setActionPending] = useState(false)
  const [daemons, setDaemons] = useState<DaemonInfo[]>([])
  const logEndRef = useRef<HTMLDivElement>(null)

  const isTerminal =
    session?.state === SessionState.MERGED ||
    session?.state === SessionState.CLOSED

  async function runAction(fn: () => Promise<{ session?: Session }>) {
    setActionPending(true)
    try {
      const res = await fn()
      if (res.session) setSession(res.session)
    } catch (err) {
      setError(String(err))
    } finally {
      setActionPending(false)
    }
  }

  function handleStop() {
    if (!id) return
    runAction(() =>
      api.proxyStopSession(create(ProxyStopSessionRequestSchema, { id })),
    )
  }

  function handlePause() {
    if (!id) return
    runAction(() =>
      api.proxyPauseSession(create(ProxyPauseSessionRequestSchema, { id })),
    )
  }

  function handleResume() {
    if (!id) return
    runAction(() =>
      api.proxyResumeSession(create(ProxyResumeSessionRequestSchema, { id })),
    )
  }

  function handleTransfer(targetDaemonId: string) {
    if (!id || !session) return
    // We need to determine the source daemon — for now use the first daemon
    // that isn't the target (the orchestrator routes by session ownership)
    runAction(() =>
      api.transferSession(
        create(TransferSessionRequestSchema, {
          sessionId: id,
          sourceDaemonId: '',
          targetDaemonId,
        }),
      ),
    )
  }

  // Fetch session metadata
  useEffect(() => {
    if (!id) return
    let cancelled = false

    async function fetchSession() {
      try {
        const res = await api.proxyGetSession(
          create(ProxyGetSessionRequestSchema, { id }),
        )
        if (!cancelled && res.session) {
          setSession(res.session)
        }
      } catch (err) {
        if (!cancelled) setError(String(err))
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    fetchSession()
    return () => { cancelled = true }
  }, [api, id])

  // Fetch daemons for transfer dropdown
  useEffect(() => {
    let cancelled = false
    async function fetchDaemons() {
      try {
        const res = await api.listDaemons(create(ListDaemonsRequestSchema, {}))
        if (!cancelled) setDaemons(res.daemons.filter((d) => d.online))
      } catch {
        // Non-critical — transfer just won't be available
      }
    }
    fetchDaemons()
    return () => { cancelled = true }
  }, [api])

  // Attach to session stream
  useEffect(() => {
    if (!id) return
    const abortController = new AbortController()
    setStreaming(true)

    async function attach() {
      try {
        const stream = api.proxyAttachSession(
          create(ProxyAttachSessionRequestSchema, { id }),
          { signal: abortController.signal },
        )
        for await (const msg of stream) {
          const event = msg.event
          switch (event.case) {
            case 'outputLine':
              setLog((prev) => [
                ...prev,
                {
                  type: 'output',
                  text: event.value.text,
                  timestamp: timestampToDate(event.value.timestamp),
                },
              ])
              break
            case 'stateChange':
              setLog((prev) => [
                ...prev,
                {
                  type: 'state',
                  text: `State: ${stateLabel[event.value.previousState] ?? event.value.previousState} → ${stateLabel[event.value.newState] ?? event.value.newState}`,
                },
              ])
              // Update session state locally
              setSession((prev) =>
                prev ? { ...prev, state: event.value.newState } : prev,
              )
              break
            case 'sessionEnded':
              setLog((prev) => [
                ...prev,
                {
                  type: 'ended',
                  text: `Session ended: ${stateLabel[event.value.finalState] ?? event.value.finalState}${event.value.reason ? ` — ${event.value.reason}` : ''}`,
                },
              ])
              setStreaming(false)
              break
          }
        }
      } catch {
        if (!abortController.signal.aborted) {
          setStreaming(false)
        }
      }
    }

    attach()
    return () => { abortController.abort() }
  }, [api, id])

  // Auto-scroll log
  useEffect(() => {
    logEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [log])

  if (loading) return <p style={page}>Loading session...</p>
  if (error) return <p style={{ ...page, color: 'red' }}>Error: {error}</p>
  if (!session) return <p style={page}>Session not found.</p>

  return (
    <div style={page}>
      <div style={{ marginBottom: 16 }}>
        <Link to="/" style={{ color: 'var(--accent)', textDecoration: 'none', fontSize: 14 }}>
          &larr; Sessions
        </Link>
      </div>

      <h2 style={{ margin: '0 0 16px' }}>{session.title || session.id}</h2>

      <div style={meta}>
        <MetaItem label="Branch" value={<code>{session.branchName}</code>} />
        <MetaItem label="Base" value={<code>{session.baseBranch}</code>} />
        <MetaItem label="State" value={stateLabel[session.state] ?? 'Unknown'} />
        <MetaItem
          label="PR"
          value={
            session.prUrl ? (
              <a href={session.prUrl} target="_blank" rel="noreferrer">
                #{session.prNumber}
              </a>
            ) : (
              '—'
            )
          }
        />
        <MetaItem label="Automation" value={session.automationEnabled ? 'On' : 'Off'} />
        <MetaItem label="Attempts" value={String(session.attemptCount)} />
        {session.blockedReason && (
          <MetaItem label="Blocked" value={session.blockedReason} />
        )}
      </div>

      {!isTerminal && (
        <div style={actions}>
          <button
            style={actionBtn}
            disabled={actionPending}
            onClick={handlePause}
          >
            Pause
          </button>
          <button
            style={actionBtn}
            disabled={actionPending}
            onClick={handleResume}
          >
            Resume
          </button>
          <button
            style={{ ...actionBtn, color: '#ef4444' }}
            disabled={actionPending}
            onClick={handleStop}
          >
            Stop
          </button>
          {daemons.length > 1 && (
            <select
              style={actionBtn}
              disabled={actionPending}
              defaultValue=""
              onChange={(e) => {
                if (e.target.value) handleTransfer(e.target.value)
                e.target.value = ''
              }}
            >
              <option value="" disabled>
                Transfer to...
              </option>
              {daemons.map((d) => (
                <option key={d.daemonId} value={d.daemonId}>
                  {d.hostname}
                </option>
              ))}
            </select>
          )}
        </div>
      )}

      {session.plan && (
        <details style={{ marginBottom: 16 }}>
          <summary style={{ cursor: 'pointer', fontWeight: 600, marginBottom: 8 }}>Plan</summary>
          <pre style={planStyle}>{session.plan}</pre>
        </details>
      )}

      <div style={logHeader}>
        <strong>Output</strong>
        {streaming && <span style={streamingBadge}>Live</span>}
      </div>
      <div style={logContainer}>
        {log.length === 0 ? (
          <p style={{ color: 'var(--text-dim)', margin: 0 }}>
            {streaming ? 'Waiting for output...' : 'No output.'}
          </p>
        ) : (
          log.map((entry, i) => (
            <div key={i} style={logLine(entry.type)}>
              {entry.text}
            </div>
          ))
        )}
        <div ref={logEndRef} />
      </div>
    </div>
  )
}

function MetaItem({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div style={{ display: 'flex', gap: 8 }}>
      <span style={{ color: 'var(--text-dim)', minWidth: 90 }}>{label}</span>
      <span>{value}</span>
    </div>
  )
}

const page: React.CSSProperties = {
  textAlign: 'left',
  padding: '0 24px',
}

const meta: React.CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: 4,
  marginBottom: 16,
  fontSize: 14,
}

const planStyle: React.CSSProperties = {
  background: 'var(--bg-secondary, #1a1a1a)',
  padding: 12,
  borderRadius: 4,
  overflow: 'auto',
  fontSize: 13,
  whiteSpace: 'pre-wrap',
  margin: 0,
}

const actions: React.CSSProperties = {
  display: 'flex',
  gap: 8,
  marginBottom: 16,
}

const actionBtn: React.CSSProperties = {
  background: 'var(--bg-secondary, #1a1a1a)',
  color: 'var(--text)',
  border: '1px solid var(--border)',
  borderRadius: 4,
  padding: '6px 14px',
  cursor: 'pointer',
  fontSize: 13,
}

const logHeader: React.CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: 8,
  marginBottom: 8,
}

const streamingBadge: React.CSSProperties = {
  background: '#22c55e',
  color: '#fff',
  fontSize: 11,
  fontWeight: 600,
  padding: '2px 8px',
  borderRadius: 10,
}

const logContainer: React.CSSProperties = {
  background: 'var(--bg-secondary, #1a1a1a)',
  borderRadius: 4,
  padding: 12,
  maxHeight: 500,
  overflow: 'auto',
  fontFamily: 'monospace',
  fontSize: 13,
  lineHeight: 1.6,
}

function logLine(type: LogEntry['type']): React.CSSProperties {
  const base: React.CSSProperties = { whiteSpace: 'pre-wrap', wordBreak: 'break-all' }
  if (type === 'state') return { ...base, color: '#60a5fa', fontWeight: 600 }
  if (type === 'ended') return { ...base, color: '#facc15', fontWeight: 600 }
  return base
}
