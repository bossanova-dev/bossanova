import { createContext, useContext, useMemo } from 'react'
import type { ReactNode } from 'react'
import { useAuth0 } from '@auth0/auth0-react'
import { createApi } from './api.ts'
import type { Client } from '@connectrpc/connect'
import type { OrchestratorService } from './gen/bossanova/v1/orchestrator_pb.ts'

type Api = Client<typeof OrchestratorService>

const ApiContext = createContext<Api | null>(null)

export function ApiProvider({ children }: { children: ReactNode }) {
  const { getAccessTokenSilently } = useAuth0()

  const api = useMemo(
    () => createApi(() => getAccessTokenSilently()),
    [getAccessTokenSilently],
  )

  return <ApiContext value={api}>{children}</ApiContext>
}

export function useApi(): Api {
  const api = useContext(ApiContext)
  if (!api) throw new Error('useApi must be used within ApiProvider')
  return api
}
