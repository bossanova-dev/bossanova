import { create } from '@bufbuild/protobuf'
import { useEffect, useState } from 'react'
import { Link } from 'react-router'
import type { Session } from '~/gen/bossanova/v1/models_pb'
import { SessionState } from '~/gen/bossanova/v1/models_pb'
import { ProxyListSessionsRequestSchema } from '~/gen/bossanova/v1/orchestrator_pb'
import { useApi } from '~/useApi'

const POLL_INTERVAL = 5000

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

export default function Sessions() {
  const api = useApi()
  const [sessions, setSessions] = useState<Session[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false

    async function fetch() {
      try {
        const res = await api.proxyListSessions(create(ProxyListSessionsRequestSchema, {}))
        if (!cancelled) {
          setSessions(res.sessions)
          setError(null)
        }
      } catch (err) {
        if (!cancelled) {
          setError(String(err))
        }
      } finally {
        if (!cancelled) {
          setLoading(false)
        }
      }
    }

    fetch()
    const id = setInterval(fetch, POLL_INTERVAL)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [api])

  if (loading) {
    return <p>Loading sessions...</p>
  }
  if (error) {
    return <p style={{ color: 'red' }}>Error: {error}</p>
  }

  return (
    <div style={{ textAlign: 'left', padding: '0 24px' }}>
      <h2>Sessions</h2>
      {sessions.length === 0 ? (
        <p>No sessions found.</p>
      ) : (
        <table style={{ width: '100%', borderCollapse: 'collapse' }}>
          <thead>
            <tr>
              <th style={th}>Title</th>
              <th style={th}>Branch</th>
              <th style={th}>State</th>
              <th style={th}>PR</th>
            </tr>
          </thead>
          <tbody>
            {sessions.map((s) => (
              <tr key={s.id}>
                <td style={td}>
                  <Link
                    to={`/sessions/${s.id}`}
                    style={{ color: 'var(--accent)', textDecoration: 'none' }}
                  >
                    {s.title || s.id}
                  </Link>
                </td>
                <td style={td}>
                  <code>{s.branchName}</code>
                </td>
                <td style={td}>{stateLabel[s.state] ?? 'Unknown'}</td>
                <td style={td}>
                  {s.prUrl ? (
                    <a href={s.prUrl} target="_blank" rel="noreferrer">
                      #{s.prNumber}
                    </a>
                  ) : (
                    '—'
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

const th: React.CSSProperties = {
  textAlign: 'left',
  borderBottom: '2px solid var(--border)',
  padding: '8px 12px',
}

const td: React.CSSProperties = {
  borderBottom: '1px solid var(--border)',
  padding: '8px 12px',
}
