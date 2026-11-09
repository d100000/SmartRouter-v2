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
export type AnalysisFilter = {
  start: number
  end: number
  request_id?: string
  model?: string
  channel?: number
}
export type FailureCounts = {
  requests: number
  affected: number
  zero_output: number
  interrupted: number
  errors: number
  client_cancelled: number
  unknown_output: number
  recovered: number
  settlement_risk?: number
  upstream_usage_unbilled?: number
  charged_zero_output?: number
}
export type Distribution = FailureCounts & { name: string }
export type FailureLog = {
  id: number
  request_id: string
  upstream_request_id: string
  created_at: number
  model: string
  channel: number
  is_stream: boolean
  completion_tokens: number
  attempts: number
  categories: string[]
  diagnostic: Record<string, string | number | boolean | null>
  log_type?: number
  prompt_tokens?: number
  quota?: number
  recorded_quota?: number
  no_charge?: boolean
  usage_source?: string
  error_message?: string
  error_truncated?: boolean
  evidence?: SettlementEvidence
  records?: FailureLog[]
  records_truncated?: boolean
}
export type Snapshot = FailureCounts & {
  filter: AnalysisFilter
  total_log_rows: number
  scanned_log_rows: number
  truncated: boolean
  log_recording_enabled: boolean
  error_recording_enabled: boolean
  models: Distribution[]
  channels: Distribution[]
  reasons: Distribution[]
  timeline: Distribution[]
  logs: FailureLog[]
}
export type AnalysisConfig = {
  version: number
  enabled: boolean
  endpoint: string
  model: string
  timezone: string
  hour: number
  max_attempts: number
  timeout_seconds: number
  idle_seconds: number
  sample_limit: number
  report_language: string
  api_key_configured: boolean
  context_window_tokens?: number
  chunk_input_tokens?: number
  max_output_tokens?: number
  safety_margin_tokens?: number
  max_calls?: number
}
export type ConfigInput = AnalysisConfig & {
  api_key: string
  clear_key: boolean
}
export type AnalysisAttempt = {
  number: number
  started_at: number
  finished_at: number
  code: string
  status_code: number
  retryable: boolean
  duration_ms?: number
  timeout_seconds?: number
  idle_seconds?: number
  max_output_tokens?: number
  finish_reason?: string
}
export type AnalysisTask = {
  task_id: string
  status: 'pending' | 'running' | 'succeeded' | 'failed'
  payload: AnalysisFilter
  created_at: number
  updated_at: number
  error: string
  state: {
    version?: number
    phase: string
    attempt: number
    max_attempts: number
    updated_at: number
    next_retry_at: number
    report: string
    attempts: AnalysisAttempt[]
    scanned_rows?: number
    total_rows?: number
    total_known?: boolean
    completed_chunks?: number
    total_chunks?: number
    current_step?: string
    can_resume?: boolean
    calls_used?: number
    max_calls?: number
  } | null
  result: {
    version?: number
    summary?: string
    timezone?: string
    timeline_interval?: 'hour' | 'day'
    report: string
    model: string
    snapshot: Snapshot | null
    snapshot_omitted?: boolean
    sample_count: number
    attempts: AnalysisAttempt[]
    comparison?: Snapshot | null
    comparison_available?: boolean
    complete?: boolean
    sections?: {
      id: string
      title: string
      content: string
      evidence_ids?: string[]
    }[]
    coverage?: {
      stable?: boolean
      complete?: boolean
      source?: string
      warnings?: string[]
      total_known?: boolean
      scanned_rows: number
      total_rows: number
      completed_chunks?: number
      total_chunks?: number
    }
  } | null
}

export type UpstreamEvidence = {
  attempt: number
  channel: number
  request_id?: string
  status_code: number
  error_body?: string
  error_truncated?: boolean
  usage?: Record<string, unknown>
  capture_incomplete?: boolean
}
export type SettlementEvidence = {
  version: number
  upstream?: UpstreamEvidence[]
  attempts_truncated?: boolean
  downstream?: {
    status_code: number
    usage?: Record<string, unknown>
    completed: boolean
    failed: boolean
    write_failed: boolean
    capture_incomplete: boolean
  }
  local?: {
    state: string
    requested_quota: number
    confirmed_quota: number
    pre_consumed_quota: number
    error?: string
  }
}
