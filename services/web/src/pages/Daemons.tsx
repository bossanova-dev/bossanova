import { useEffect, useState } from 'react'
import { create } from '@bufbuild/protobuf'
import { useApi } from '../useApi'
import { ListDaemonsRequestSchema } from '../gen/bossanova/v1/orchestrator_pb'
import type { DaemonInfo } from '../gen/bossanova/v1/orchestrator_pb'
import type { Timestamp } from '@bufbuild/protobuf/wkt'

const POLL_INTERVAL = 10000

function formatTimestamp(ts: Timestamp | undefined): string {
  if (!ts) return '—'
  const d = new Date(Number(ts.seconds) * 1000 + ts.nanos / 1_000_000)
  return d.toLocaleTimeString()
}

export default function Daemons() {
  const api = useApi()
  const [daemons, setDaemons] = useState<DaemonInfo[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false

    async function fetch() {
      try {
        const res = await api.listDaemons(
          create(ListDaemonsRequestSchema, {}),
        )
        if (!cancelled) {
          setDaemons(res.daemons)
          setError(null)
        }
      } catch (err) {
        if (!cancelled) setError(String(err))
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    fetch()
    const id = setInterval(fetch, POLL_INTERVAL)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [api])

  if (loading) return <p>Loading daemons...</p>
  if (error) return <p style={{ color: 'red' }}>Error: {error}</p>

  return (
    <div style={{ textAlign: 'left', padding: '0 24px' }}>
      <h2>Daemons</h2>
      {daemons.length === 0 ? (
        <p>No daemons registered.</p>
      ) : (
        <table style={{ width: '100%', borderCollapse: 'collapse' }}>
          <thead>
            <tr>
              <th style={th}>Status</th>
              <th style={th}>Hostname</th>
              <th style={th}>Repos</th>
              <th style={th}>Sessions</th>
              <th style={th}>Last Heartbeat</th>
            </tr>
          </thead>
          <tbody>
            {daemons.map((d) => (
              <tr key={d.daemonId}>
                <td style={td}>
                  <span style={{ color: d.online ? '#22c55e' : '#ef4444' }}>
                    {d.online ? 'Online' : 'Offline'}
                  </span>
                </td>
                <td style={td}><code>{d.hostname}</code></td>
                <td style={td}>{d.repoIds.length}</td>
                <td style={td}>{d.activeSessions}</td>
                <td style={td}>{formatTimestamp(d.lastHeartbeat)}</td>
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
