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
import type { OverviewDimension } from './components/overview-filters'

export interface OverviewMetrics {
  requests: number
  attempts: number
  successes: number
  failures: number
  revenue: number
  cost: number
  complete_revenue: number
  complete_cost: number
  margin: number
  margin_rate: number | null
  cost_coverage: number | null
  health: number | null
  latency_ms: number | null
  unknown_cost_attempts: number
  complete_requests: number
  complete_attempts: number
  unconfirmed_revenue_requests: number
  subscription_revenue: number
}

export interface OverviewDailyPoint extends OverviewMetrics {
  day: string
}

export interface OverviewRow extends OverviewMetrics {
  id: string
  name: string
}

export interface OverviewSnapshot {
  dimension: OverviewDimension
  days: number
  currency: 'USD'
  scope: 'synchronous_relay'
  summary: OverviewMetrics
  daily: OverviewDailyPoint[]
  ranking: OverviewRow[]
  distribution: OverviewDistribution[]
  models: OverviewModel[]
  options: Array<{ id: string; name: string }>
  refreshed_at: number
  collected_since: number | null
  partial: boolean
  limits: { max_days: number; max_rows: number }
  warnings: string[]
}

export interface OverviewModel {
  id: string
  name: string
  revenue: number
  cost: number
  attempts: number
}

export interface OverviewDistribution extends OverviewModel {
  day: string
}
