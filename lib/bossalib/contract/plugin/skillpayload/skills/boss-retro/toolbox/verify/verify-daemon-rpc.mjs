// Authenticated Connect JSON over the host daemon's Unix socket. Settings socket_path overrides
// are honoured through BOSS_SOCKET only; the fallback matches Go's os.UserConfigDir.
import fs from 'node:fs'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'

export function daemonSocket(env = process.env, platform = process.platform, home = os.homedir()) {
  if (env.BOSS_SOCKET) return env.BOSS_SOCKET
  const config =
    platform === 'darwin'
      ? path.join(home, 'Library', 'Application Support')
      : platform === 'win32'
        ? env.APPDATA
        : env.XDG_CONFIG_HOME || path.join(home, '.config')
  return path.join(config, 'bossanova', 'bossd.sock')
}

export async function daemonUnary(socketPath, method, message = {}, { timeoutMs = 5000 } = {}) {
  const token = fs.readFileSync(path.join(path.dirname(socketPath), 'bossd.token'), 'utf8').trim()
  const body = JSON.stringify(message)
  return new Promise((resolve, reject) => {
    const req = http.request(
      {
        socketPath,
        method: 'POST',
        path: `/bossanova.v1.DaemonService/${method}`,
        signal: AbortSignal.timeout(timeoutMs),
        headers: {
          'content-type': 'application/json',
          'connect-protocol-version': '1',
          'content-length': Buffer.byteLength(body),
          authorization: `Bearer ${token}`,
        },
      },
      (res) => {
        const chunks = []
        res.on('error', reject)
        res.on('data', (chunk) => chunks.push(chunk))
        res.on('end', () => {
          try {
            const text = Buffer.concat(chunks).toString('utf8')
            const result = text ? JSON.parse(text) : {}
            if (res.statusCode === 200) resolve(result)
            else
              reject(
                Object.assign(new Error(`${method} → HTTP ${res.statusCode}`), {
                  code: result.code,
                  reason: res.headers['bossd-evidence-reason'],
                }),
              )
          } catch (err) {
            reject(err)
          }
        })
      },
    )
    req.on('error', reject)
    req.end(body)
  })
}
