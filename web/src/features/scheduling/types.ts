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
export interface ChannelOverride {
  channel_id: number
  weight: number | null
  capacity: number | null
  capacity_key?: string
}

export interface SchedulingConfig {
  enabled: boolean
  target_success_rate: number
  target_ttft_ms: number
  default_capacity: number
  channel_overrides: ChannelOverride[]
}

export interface SchedulingChannel {
  channel_id: number
  name: string
  status: number
  route_state:
    | 'eligible'
    | 'ineligible'
    | 'degraded'
    | 'disabled'
    | 'cooling'
    | 'saturated'
    | 'cold'
  traffic_share: number
  effective_weight: number
  configured_weight: number
  priority: number
  dispatches_30m: number
  success_rate_30m: number | null
  success_rate_5m: number | null
  in_flight: number
  capacity: number
  health_attainment: number | null
  health_score: number
  health_baseline: number
  recovery_limit: number
  ramp_successes: number
  ramp_progress: number
  ramp_limited: boolean
  cooldown_until?: string
  avg_ttft_ms_5m: number | null
  quality_score: number
  selection_probability: number
  can_recover: boolean
}

export interface SchedulingTrendPoint {
  timestamp: number
  requests: number
  success_rate: number | null
  avg_ttft_ms: number | null
  in_flight: number | null
}

export interface SchedulingSnapshot {
  groups: string[]
  models: string[]
  group: string
  model: string
  stream: boolean
  scope: 'instance'
  started_at: number
  refreshed_at: number
  active: boolean
  phase: 'cold' | 'transition' | 'dynamic'
  activation_requests: number
  activation_threshold: number
  config: SchedulingConfig
  summary: {
    requests_30m: number
    attempts_30m: number
    success_rate_30m: number | null
    success_rate_5m: number | null
    avg_ttft_ms_5m: number | null
    healthy_channels: number
    eligible_channels: number
    degraded_channels: number
    in_flight: number
    top_traffic_share: number
  }
  trend: SchedulingTrendPoint[]
  channels: SchedulingChannel[]
}

export interface SchedulingFilter {
  group: string
  model: string
  stream: boolean
}
