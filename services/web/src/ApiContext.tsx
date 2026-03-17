import { useAuth0 } from '@auth0/auth0-react'
import type { ReactNode } from 'react'
import { useMemo } from 'react'
import { createApi } from '~/api'
import { ApiContext } from '~/apiContext'

export function ApiProvider({ children }: { children: ReactNode }) {
  const { getAccessTokenSilently } = useAuth0()

  const api = useMemo(() => createApi(() => getAccessTokenSilently()), [getAccessTokenSilently])

  return <ApiContext value={api}>{children}</ApiContext>
}
