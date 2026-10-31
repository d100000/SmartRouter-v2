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
  AnalysisConfig,
  AnalysisFilter,
  AnalysisTask,
  ConfigInput,
  Snapshot,
} from './types'

type Response<T> = { success: boolean; message?: string; data: T }
export async function getAnalysis(
  filter: AnalysisFilter,
  offset: number,
  category: string,
  signal?: AbortSignal,
  details = false
) {
  const response = await api.get<
    Response<{ snapshot: Snapshot; log_total: number; offset: number }>
  >('/api/error-analysis', {
    params: { ...filter, offset, category, details },
    signal,
    disableDuplicate: true,
  })
  return requireServerSuccess(response.data).data
}
export async function getConfig(): Promise<AnalysisConfig> {
  const response = await api.get<Response<AnalysisConfig>>(
    '/api/error-analysis/config'
  )
  return requireServerSuccess(response.data).data
}
export async function saveConfig(input: ConfigInput): Promise<AnalysisConfig> {
  const response = await api.put<Response<AnalysisConfig>>(
    '/api/error-analysis/config',
    input
  )
  return requireServerSuccess(response.data).data
}
export async function startAnalysis(filter: AnalysisFilter) {
  const response = await api.post<
    Response<{ task: AnalysisTask; created: boolean }>
  >('/api/error-analysis/reports', filter)
  return requireServerSuccess(response.data).data
}
export async function getReports(offset: number) {
  const response = await api.get<
    Response<{ items: AnalysisTask[]; total: number }>
  >('/api/error-analysis/reports', {
    params: { offset },
    disableDuplicate: true,
  })
  return requireServerSuccess(response.data).data
}
export async function getReport(id: string): Promise<AnalysisTask> {
  const response = await api.get<Response<AnalysisTask>>(
    `/api/error-analysis/reports/${encodeURIComponent(id)}`,
    { disableDuplicate: true }
  )
  return requireServerSuccess(response.data).data
}
