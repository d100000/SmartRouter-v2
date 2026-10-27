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
export type PerformanceSummary = {
  avg_latency_ms: number
  success_rate: number
  avg_tps: number
  output_metrics_available?: boolean
}

export type PerformanceSeriesPoint = {
  ts: number
  avg_ttft_ms: number
  avg_latency_ms: number
  success_rate: number
  avg_tps: number
}

export type PerformanceGroup = {
  group: string
  avg_ttft_ms: number
  avg_latency_ms: number
  success_rate: number
  avg_tps: number
  series: PerformanceSeriesPoint[]
}

export type PerformanceMetricsData = {
  success: boolean
  message?: string
  data: {
    summary?: PerformanceSummary | null
    series?: PerformanceSeriesPoint[]
    window_start?: number
    window_end?: number
    model_name: string
    series_schema?: string
    groups: PerformanceGroup[]
  }
}

export type SuccessRatePoint = { ts: number; success_rate: number }

export type PerfModelSummary = {
  model_name: string
  avg_latency_ms: number
  success_rate: number
  avg_tps: number
  output_metrics_available?: boolean
  recent_success_series?: SuccessRatePoint[]
  request_count?: number
}

export type PerfSummaryAllData = {
  success: boolean
  message?: string
  data: {
    summary?: PerformanceSummary | null
    window_start?: number
    window_end?: number
    models: PerfModelSummary[]
    recent_success_series?: Record<string, SuccessRatePoint[]>
  }
}

export type PerformanceHealthModel = Omit<
  PerfModelSummary,
  'avg_latency_ms' | 'success_rate' | 'avg_tps'
> & {
  avg_latency_ms: number | null
  success_rate: number | null
  avg_tps: number | null
  effective_slo?: number | null
  min_samples?: number
  monitoring_enabled?: boolean
  configured?: boolean
  company_id?: string
  company_name?: string
  group_id?: string
  group_name?: string
  group?: string
  p95_latency_ms?: number
  p95_ttft_ms?: number
  first_attempt_success_rate?: number
  retry_request_rate?: number
  retry_recovery_rate?: number
  attempt_success_rate?: number
  incidents?: number
  recent_success_series?: SuccessRatePoint[]
}

export type PerformanceHealthCompany = {
  id: string
  name: string
  status?: number | string
  slo?: number
  slo_confirmed?: boolean
  preset_key?: string
  owner?: string
  note?: string
  groups?: Array<{
    id: string
    name: string
    models: string[]
    enabled?: boolean
    slo?: number
    slo_confirmed?: boolean
    preset_key?: string
    p95_latency_ms?: number
    min_samples?: number
  }>
}

export type PerformanceHealthConfig = {
  version?: number
  config_version?: number
  updated_at?: number
  preset_version?: string
  excluded_models?: string[]
  excluded_companies?: string[]
  excluded_groups?: string[]
  companies: PerformanceHealthCompany[]
}

export type PerformanceHealthPresetPreview = {
  config: PerformanceHealthConfig
  expected_config_version: number
  preview_token: string
  added_companies: string[]
  added_groups: string[]
  added_models: string[]
  conflicts: string[]
  skipped_models: string[]
}

export type PerformanceHealthData = {
  summary?: PerformanceSummary | null
  models: PerformanceHealthModel[]
  companies?: PerformanceHealthCompany[]
  series?: PerformanceSeriesPoint[]
  window_start?: number
  window_end?: number
  terminal_coverage?: number
  terminal_coverage_available?: boolean
  data_freshness?: string | number
  definition_version?: string
  generated_at?: number
  window_alignment?: string
  bucket_seconds?: number
  current_bucket_partial?: boolean
  last_sample_at?: number | null
  persisted_through?: number | null
  collector_heartbeat_at?: number | null
  collection?: PerformanceCollectionHealth
}

export type PerformanceCollectionHealth = {
  enabled: boolean
  availability: 'ready' | 'disabled' | 'delayed' | 'partial'
  storage: 'database_outbox'
  target_visibility_seconds: number
  pending_events: number
  oldest_pending_at: number | null
  last_sample_at: number | null
  last_accepted_at: number | null
  last_projected_at: number | null
  persisted_through: number | null
  collector_heartbeat_at: number | null
  unpersisted_events: number
  oldest_unpersisted_at: number | null
  dropped_events: number
  stale_collectors: number
  local_unpersisted_events: number
  quarantined_events?: number
}

export type PerformanceHealthResponse = {
  success: boolean
  message?: string
  data: PerformanceHealthData
}

export type PerformanceHealthCompanyResponse = {
  success: boolean
  message?: string
  data: PerformanceHealthConfig
}
