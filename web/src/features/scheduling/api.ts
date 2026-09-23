/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

import type {
  SchedulingConfig,
  SchedulingFilter,
  SchedulingSnapshot,
} from './types'

type Response<T> = { success: boolean; message?: string; data: T }

export async function getScheduling(
  filter: SchedulingFilter,
  signal?: AbortSignal
): Promise<SchedulingSnapshot> {
  const response = await api.get<Response<SchedulingSnapshot>>(
    '/api/scheduling',
    // React Query owns deduplication; each new query needs its own abort signal.
    { params: filter, signal, disableDuplicate: true }
  )
  return requireServerSuccess(response.data).data
}

export async function saveSchedulingConfig(
  group: string,
  model: string,
  config: SchedulingConfig
): Promise<void> {
  const response = await api.post<Response<unknown>>('/api/scheduling/config', {
    group,
    model,
    ...config,
  })
  requireServerSuccess(response.data)
}

export async function recoverSchedulingChannel(
  group: string,
  model: string,
  channelId: number
): Promise<void> {
  const response = await api.post<Response<unknown>>(
    '/api/scheduling/recover',
    { group, model, channel_id: channelId }
  )
  requireServerSuccess(response.data)
}
