import { useContext } from 'react'
import { ApiContext } from './apiContext'
import type { Api } from './apiContext'

export function useApi(): Api {
  const api = useContext(ApiContext)
  if (!api) throw new Error('useApi must be used within ApiProvider')
  return api
}
