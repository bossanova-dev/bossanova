import { createContext } from 'react'
import type { Client } from '@connectrpc/connect'
import type { OrchestratorService } from './gen/bossanova/v1/orchestrator_pb'

export type Api = Client<typeof OrchestratorService>

export const ApiContext = createContext<Api | null>(null)
