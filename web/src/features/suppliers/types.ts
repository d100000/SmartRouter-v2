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
export interface SupplierTemplate {
  id: string
  name: string
  type: number
  models: string[]
  protocols?: SupplyProtocol[]
  tpm_applicable?: boolean
}
export type SupplyProtocol = 'http' | 'async_task' | 'realtime'
export type SupplyStage =
  | 'inference'
  | 'embedding'
  | 'rerank'
  | 'image'
  | 'audio'
  | 'task_submit'
  | 'realtime_generation'
  | 'realtime_transcription'
export type SupplyTaskStatus =
  | 'accepted'
  | 'queued'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'unknown'
export interface SupplierAccount {
  id: string
  name: string
  user_id: number
  username?: string
  supply_enabled: boolean
  login_enabled: boolean
  allowed_types: number[]
  allowed_templates?: string[]
  allowed_models: string[]
  max_channels: number
  group: string
  revision: number
  cost_ratio: number | null
  pool_priority: number
  pool_weight: number
  pool_capacity?: number
  max_output_tokens: number
  max_input_tokens?: number
}
export interface SupplierSelf {
  supplier: Pick<SupplierAccount, 'id' | 'name' | 'supply_enabled'>
  templates: SupplierTemplate[]
}
export interface SupplyChannel {
  id: number
  name: string
  type: number
  template_id?: string
  base_url: string
  models: string[]
  remark: string
  priority: number
  version: number
  paused: boolean
  status: string
  blocking_reasons?: string[]
  supplier_id: string
  supplier_name: string
  has_key: boolean
  has_validated_config?: boolean
  validation_status: string
  group?: string
  weight?: number
  platform_priority?: number
  protocols?: SupplyProtocol[]
  tpm_applicable?: boolean
}
export interface ChannelLimitConfig {
  channel_id: number
  revision: number
  rpm: number | null
  tpm: number | null
  daily_usd: string | null
  reconcile_seconds: number
  budget_ready: boolean
  tpm_applicable?: boolean
}
export interface ChannelLimitUsage {
  rpm_used: number
  tpm_used: number
  tpm_reserved: number
  daily_used_usd: string | null
  daily_reserved_usd: string | null
  daily_remaining_usd: string | null
  budget_day: string
  reset_at: number
  reason: string
}
export interface ChannelLimits {
  config: ChannelLimitConfig
  usage: ChannelLimitUsage
}
export type ChannelLimitUpdate = Pick<
  ChannelLimitConfig,
  'revision' | 'rpm' | 'tpm' | 'daily_usd'
> & { reconcile_seconds?: number }
export interface SupplyChannelUpdate {
  name: string
  template_id: string
  base_url: string
  key?: string
  models: string[]
  remark: string
  priority: number
  version?: number
  limits?: Omit<ChannelLimitUpdate, 'revision'>
}
export interface SupplyTrend extends SupplyMetrics {
  timestamp: number
  request_count: number
  success_count: number
  failure_count: number
  unknown_count: number
  avg_latency_ms: number | null
}
export interface SupplyMetrics {
  video_seconds?: number | null
  video_usage_known_count?: number
  credits?: string | null
  credit_usage_known_count?: number
  realtime_generation_count?: number
  async?: SupplyAsyncMetrics
  image_count?: number | null
  audio_seconds?: number | null
  image_usage_known_count?: number
  audio_usage_known_count?: number
  logical_request_count?: number
  last_called_at?: number | null
  auth_error_count?: number
  timeout_count?: number
  rate_limited_count?: number
  upstream_error_count?: number
  request_error_count?: number
  unknown_error_count?: number
}
export interface SupplyAsyncMetrics {
  submitted_count: number
  accepted_count: number
  rejected_count: number
  unknown_submission_count: number
  queued_count: number
  running_count: number
  succeeded_count: number
  failed_count: number
  unknown_count: number
  avg_submit_latency_ms: number | null
  avg_completion_ms: number | null
}
export interface SupplyOverview extends SupplyMetrics {
  trend?: SupplyTrend[]
  coverage?: {
    collected_since: number | null
    timezone: string
    limitations: string[]
  }
  cache_read_tokens?: number | null
  cache_write_tokens?: number | null
  usage_known_count?: number
  avg_first_token_ms?: number | null
  channel_count: number
  available_channels: number
  request_count: number
  success_count: number
  failure_count: number
  unknown_count: number
  input_tokens: number
  output_tokens: number
  avg_latency_ms: number | null
  daily_used_usd: string | null
  budget_day: string
  updated_at: number
  channels: Array<
    SupplyMetrics & {
      channel_id: number
      channel_name: string
      request_count: number
      success_count: number
      failure_count: number
      unknown_count: number
      avg_latency_ms: number | null
    }
  >
}
export interface SupplyLogFilters {
  channel_id?: number
  model?: string
  correlation_id?: string
  result?: string
  days?: number
  stage?: SupplyStage
  task_status?: SupplyTaskStatus
  from?: number
  to?: number
}
export interface SupplyLog {
  protocol?: SupplyProtocol
  task_status?: SupplyTaskStatus | ''
  task_observed_at?: number | null
  task_completed_at?: number | null
  task_duration_ms?: number | null
  video_seconds?: number | null
  video_usage_source?: string
  credits?: string | null
  credit_usage_source?: string
  image_count?: number | null
  audio_seconds?: number | null
  image_usage_source?: string
  audio_usage_source?: string
  is_stream?: boolean
  first_token_ms?: number | null
  cache_read_tokens?: number | null
  cache_write_tokens?: number | null
  id: string
  channel_id: number
  channel_name: string
  created_at: number
  model: string
  stage: SupplyStage | 'test' | 'polling' | 'unknown'
  result: string
  error_category?: string
  status_code: number
  duration_ms: number | null
  input_tokens: number | null
  output_tokens: number | null
  usage_source: string
  correlation_id: string
  upstream_request_id?: string
}
export interface PageData<T> {
  items: T[]
  total: number
}
export type SupplierMode = 'supplier' | 'viewer'
export type SupplierAccountUpdate = Omit<
  SupplierAccount,
  'id' | 'username' | 'login_enabled'
> & { name: string }

export interface SupplierBudgetEvent {
  id: string
  channel_id: number
  budget_day: string
  sent_at: number
  status: string
  cost_usd: string | null
  reserved_usd: string | null
  tpm_estimated: boolean
  reconcile_until: number
}

export interface SupplierLimitEvent {
  id: string
  channel_id: number
  kind: string
  budget_day: string
  occurred_at: number
}
