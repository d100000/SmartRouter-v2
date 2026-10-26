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

import type {
  PerformanceHealthCompanyResponse,
  PerformanceHealthConfig,
  PerformanceHealthResponse,
  PerformanceMetricsData,
  PerfSummaryAllData,
} from './types'

export async function getPerfMetricsSummary(
  hours = 24,
  recentModels?: string[]
): Promise<PerfSummaryAllData> {
  const res = await api.get<PerfSummaryAllData>('/api/perf-metrics/summary', {
    params: { hours, recent_model: recentModels },
    paramsSerializer: { indexes: null },
  })
  return res.data
}

export async function getPerfMetrics(
  modelName: string,
  hours = 24
): Promise<PerformanceMetricsData> {
  const res = await api.get<PerformanceMetricsData>('/api/perf-metrics', {
    params: {
      model: modelName,
      hours,
    },
  })
  return res.data
}

export async function getPerformanceHealthSummary(
  hours = 24
): Promise<PerformanceHealthResponse> {
  const res = await api.get<PerformanceHealthResponse>(
    '/api/performance-health/summary',
    { params: { hours } }
  )
  return res.data
}

export async function getPerformanceHealthCompanies(): Promise<PerformanceHealthCompanyResponse> {
  const res = await api.get<PerformanceHealthCompanyResponse>(
    '/api/performance-health/companies'
  )
  return res.data
}

export async function savePerformanceHealthConfig(
  config: PerformanceHealthConfig
): Promise<PerformanceHealthConfig> {
  const res = await api.put<{
    success: boolean
    data: PerformanceHealthConfig
  }>('/api/performance-health/config', config)
  return res.data.data
}
