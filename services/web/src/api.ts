import type { Interceptor } from '@connectrpc/connect'
import { createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'
import { OrchestratorService } from '~/gen/bossanova/v1/orchestrator_pb'

const baseUrl = (import.meta.env.VITE_API_BASE_URL as string) || 'http://localhost:8080'

export function createAuthInterceptor(getToken: () => Promise<string>): Interceptor {
  return (next) => async (req) => {
    const token = await getToken()
    req.header.set('Authorization', `Bearer ${token}`)
    return next(req)
  }
}

export function createApi(getToken: () => Promise<string>) {
  const transport = createConnectTransport({
    baseUrl,
    interceptors: [createAuthInterceptor(getToken)],
  })

  return createClient(OrchestratorService, transport)
}
