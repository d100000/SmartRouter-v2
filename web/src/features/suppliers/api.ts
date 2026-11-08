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
import { AuthOperationError } from '@/lib/secure-verification'
import { requireServerSuccess } from '@/lib/server-error-message'

import type {
  ChannelLimits,
  ChannelLimitUpdate,
  PageData,
  SupplierAccount,
  SupplierBudgetEvent,
  SupplierLimitEvent,
  SupplierAccountUpdate,
  SupplierMode,
  SupplierSelf,
  SupplierTemplate,
  SupplyChannel,
  SupplyChannelUpdate,
  SupplyLog,
  SupplyLogFilters,
  SupplyOverview,
} from './types'

interface Response<T> {
  success: boolean
  message?: string
  data: T
}
export const supplyQueryKey = ['supply'] as const
export async function getSupplierTemplates(
  signal?: AbortSignal
): Promise<SupplierTemplate[]> {
  const result = await api.get<Response<{ items: SupplierTemplate[] }>>(
    '/api/suppliers/templates',
    { signal, disableDuplicate: true }
  )
  return requireServerSuccess(result.data).data.items
}
export function channelEndpoint(mode: SupplierMode) {
  return mode === 'supplier'
    ? '/api/supplier/channels'
    : '/api/channel/read-only'
}
export async function getSupplierSelf(
  signal?: AbortSignal
): Promise<SupplierSelf> {
  const result = await api.get<Response<SupplierSelf>>('/api/supplier/self', {
    signal,
    disableDuplicate: true,
  })
  return requireServerSuccess(result.data).data
}
export async function getSupplyChannels(
  mode: SupplierMode,
  params: { p: number; page_size: number; keyword: string },
  signal?: AbortSignal
): Promise<PageData<SupplyChannel>> {
  const result = await api.get<Response<PageData<SupplyChannel>>>(
    `${channelEndpoint(mode)}/`,
    { params, signal, disableDuplicate: true }
  )
  return requireServerSuccess(result.data).data
}
export async function saveSupplyChannel(
  channelId: number | undefined,
  payload: SupplyChannelUpdate,
  proofToken?: string,
  audience: 'supplier' | 'admin' = 'supplier',
  supplierId?: string
): Promise<SupplyChannel> {
  const config = proofToken
    ? {
        headers: { 'X-Security-Proof': proofToken },
        singleUseAuthorization: true as const,
      }
    : undefined
  try {
    const result = channelId
      ? await api.put<Response<SupplyChannel>>(
          `${audience === 'admin' ? '/api/suppliers/channels' : '/api/supplier/channels'}/${channelId}`,
          payload,
          config
        )
      : await api.post<Response<SupplyChannel>>(
          audience === 'admin'
            ? `/api/suppliers/owners/${supplierId}/channels`
            : '/api/supplier/channels',
          payload,
          config
        )
    return requireServerSuccess(result.data).data
  } catch (error) {
    // Axios errors retain the request body and proof headers. Only the safe
    // message/code may enter React Query's shared mutation cache.
    const safeError = AuthOperationError.from(error)
    throw new AuthOperationError(safeError.message, safeError.code)
  }
}
export async function setSupplyChannelPaused(
  channel: SupplyChannel,
  paused: boolean
): Promise<SupplyChannel> {
  const result = await api.post<Response<SupplyChannel>>(
    `/api/supplier/channels/${channel.id}/status`,
    { paused, version: channel.version }
  )
  return requireServerSuccess(result.data).data
}
export async function discardSupplyChannel(
  channel: SupplyChannel
): Promise<void> {
  requireServerSuccess(
    (
      await api.delete<Response<unknown>>(
        `/api/supplier/channels/${channel.id}`,
        { data: { version: channel.version } }
      )
    ).data
  )
}
export async function testSupplyChannel(channel: SupplyChannel): Promise<void> {
  requireServerSuccess(
    (
      await api.post<Response<unknown>>(
        `/api/supplier/channels/${channel.id}/test`,
        { version: channel.version }
      )
    ).data
  )
}
export async function getChannelLimits(
  channelId: number,
  audience: SupplierMode | 'admin',
  signal?: AbortSignal
): Promise<ChannelLimits> {
  const prefix =
    audience === 'admin' ? '/api/channel' : channelEndpoint(audience)
  return requireServerSuccess(
    (
      await api.get<Response<ChannelLimits>>(`${prefix}/${channelId}/limits`, {
        signal,
        disableDuplicate: true,
      })
    ).data
  ).data
}
export async function saveChannelLimits(
  channelId: number,
  audience: 'supplier' | 'admin',
  payload: ChannelLimitUpdate
): Promise<ChannelLimits> {
  const prefix =
    audience === 'admin' ? '/api/channel' : '/api/supplier/channels'
  return requireServerSuccess(
    (
      await api.put<Response<ChannelLimits>>(
        `${prefix}/${channelId}/limits`,
        payload
      )
    ).data
  ).data
}
export async function getSupplyOverview(
  mode: SupplierMode,
  filters: SupplyLogFilters,
  signal?: AbortSignal
): Promise<SupplyOverview> {
  const path =
    mode === 'supplier'
      ? '/api/supplier/overview'
      : '/api/channel/read-only/overview'
  return requireServerSuccess(
    (
      await api.get<Response<SupplyOverview>>(path, {
        params: filters,
        signal,
        disableDuplicate: true,
      })
    ).data
  ).data
}
export async function getSupplyLogs(
  params: { p: number; page_size: number } & SupplyLogFilters,
  signal?: AbortSignal
): Promise<PageData<SupplyLog>> {
  return requireServerSuccess(
    (
      await api.get<Response<PageData<SupplyLog>>>('/api/supplier/logs', {
        params,
        signal,
        disableDuplicate: true,
      })
    ).data
  ).data
}
export async function getSuppliers(
  signal?: AbortSignal
): Promise<PageData<SupplierAccount>> {
  return requireServerSuccess(
    (
      await api.get<Response<PageData<SupplierAccount>>>('/api/suppliers/', {
        signal,
        disableDuplicate: true,
      })
    ).data
  ).data
}
export async function saveSupplier(
  id: string | undefined,
  payload: SupplierAccountUpdate,
  proofToken?: string
): Promise<SupplierAccount> {
  const config = proofToken
    ? {
        headers: { 'X-Security-Proof': proofToken },
        singleUseAuthorization: true as const,
      }
    : undefined
  const {
    name,
    revision,
    user_id,
    supply_enabled,
    allowed_types,
    allowed_templates,
    allowed_models,
    group,
    max_channels,
    max_output_tokens,
    max_input_tokens,
    cost_ratio,
    pool_priority,
    pool_weight,
    pool_capacity,
  } = payload
  const body = {
    name,
    revision,
    user_id,
    supply_enabled,
    allowed_types,
    allowed_templates,
    allowed_models,
    group,
    max_channels,
    max_output_tokens,
    max_input_tokens,
    cost_ratio,
    pool_priority,
    pool_weight,
    pool_capacity,
  }
  try {
    const result = id
      ? await api.put<Response<SupplierAccount>>(
          `/api/suppliers/${id}`,
          body,
          config
        )
      : await api.post<Response<SupplierAccount>>(
          '/api/suppliers/',
          body,
          config
        )
    return requireServerSuccess(result.data).data
  } catch (error) {
    const safeError = AuthOperationError.from(error)
    throw new AuthOperationError(safeError.message, safeError.code)
  }
}

export async function exportSupplyLogs(
  filters: SupplyLogFilters
): Promise<string> {
  const result = await api.get<string>('/api/supplier/logs/export', {
    params: filters,
    responseType: 'text',
  })
  if (!String(result.headers['content-type'] ?? '').includes('text/csv')) {
    let error: unknown
    try {
      error = JSON.parse(result.data)
    } catch {
      throw new Error('Failed to export logs')
    }
    requireServerSuccess(error as Response<unknown>)
    throw new Error('Failed to export logs')
  }
  return result.data
}

export async function getReadOnlyChannel(
  channelId: number,
  signal?: AbortSignal
): Promise<SupplyChannel> {
  return requireServerSuccess(
    (
      await api.get<Response<SupplyChannel>>(
        `/api/channel/read-only/${channelId}`,
        { signal, disableDuplicate: true }
      )
    ).data
  ).data
}

export async function getSupplierBudgetEvents(
  channelId: number,
  params: { p: number; page_size: number; day?: string },
  signal?: AbortSignal
): Promise<PageData<SupplierBudgetEvent>> {
  return requireServerSuccess(
    (
      await api.get<Response<PageData<SupplierBudgetEvent>>>(
        `/api/supplier/channels/${channelId}/budget-events`,
        { params, signal, disableDuplicate: true }
      )
    ).data
  ).data
}

export async function copySupplyChannel(
  channel: SupplyChannel,
  name: string,
  proofToken: string
): Promise<void> {
  try {
    requireServerSuccess(
      (
        await api.post<Response<{ id: number }>>(
          `/api/supplier/channels/${channel.id}/copy`,
          { version: channel.version, name },
          {
            headers: { 'X-Security-Proof': proofToken },
            singleUseAuthorization: true,
          }
        )
      ).data
    )
  } catch (error) {
    const safeError = AuthOperationError.from(error)
    throw new AuthOperationError(safeError.message, safeError.code)
  }
}

export async function getSupplierLimitEvents(
  channelId: number,
  audience: 'supplier' | 'admin',
  params: { p: number; page_size: number },
  signal?: AbortSignal
): Promise<PageData<SupplierLimitEvent>> {
  const prefix =
    audience === 'supplier' ? '/api/supplier/channels' : '/api/channel'
  return requireServerSuccess(
    (
      await api.get<Response<PageData<SupplierLimitEvent>>>(
        `${prefix}/${channelId}/limit-events`,
        { params, signal, disableDuplicate: true }
      )
    ).data
  ).data
}
