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

export interface UpstreamSupplier {
  id: string
  name: string
}

export interface UpstreamCredential {
  binding_id: string
  channel_id: number
  key_index: number
  credential_id: string
  credential_version_id: string
  alias: string
  supplier_id: string
  tags: string[]
  cost_ratio: number | null
  effective_cost_ratio: number | null
  cost_source: 'credential' | 'channel' | 'unknown'
  cost_version_id: string
  ownership_version_id: string
  active: boolean
}

export interface UpstreamCredentialMetadata {
  alias: string
  supplier_id: string
  tags: string[]
  cost_ratio: number | null
}

type MetadataResponse<T> = { success: boolean; message?: string; data: T }

export async function getUpstreamCredentials(
  channelId: number,
  signal?: AbortSignal
): Promise<UpstreamCredential[]> {
  const response = await api.get<MetadataResponse<UpstreamCredential[]>>(
    `/api/channel/${channelId}/upstream-credentials`,
    { signal, disableDuplicate: true }
  )
  return requireServerSuccess(response.data).data
}

export async function getUpstreamSuppliers(
  signal?: AbortSignal
): Promise<UpstreamSupplier[]> {
  const response = await api.get<MetadataResponse<UpstreamSupplier[]>>(
    '/api/upstream-supplier',
    { signal, disableDuplicate: true }
  )
  return requireServerSuccess(response.data).data
}

export async function saveUpstreamCredential(
  credentialId: string,
  input: UpstreamCredentialMetadata
): Promise<void> {
  const response = await api.put<MetadataResponse<unknown>>(
    `/api/upstream-credential/${encodeURIComponent(credentialId)}`,
    input
  )
  requireServerSuccess(response.data)
}

export async function createUpstreamSupplier(
  name: string
): Promise<UpstreamSupplier> {
  const response = await api.post<MetadataResponse<UpstreamSupplier>>(
    '/api/upstream-supplier',
    { name }
  )
  return requireServerSuccess(response.data).data
}
