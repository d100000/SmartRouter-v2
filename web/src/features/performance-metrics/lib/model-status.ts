/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.
*/
import type { PerformanceHealthModel, SuccessRatePoint } from '../types'

export type PerformanceColumnId =
  | 'model'
  | 'company'
  | 'requests'
  | 'success'
  | 'first'
  | 'retry'
  | 'p95'
  | 'avg'
  | 'ttft'
  | 'tps'
  | 'incidents'
  | 'status'

export type PerformanceModelRow = {
  model: string
  companyId: string
  companyName: string
  groupName: string
  isAssigned: boolean
  requests: number
  successRate: number
  firstAttemptRate: number
  retryRecoveryRate: number
  p95LatencyMs: number
  avgLatencyMs: number
  p95TtftMs: number
  tps: number
  incidents: number
  series: SuccessRatePoint[]
  effectiveSlo: number
  minSamples: number
  monitoringEnabled: boolean
  configured: boolean
}

export function normalizePerformanceModel(
  raw: PerformanceHealthModel
): PerformanceModelRow {
  const companyId = raw.company_id?.trim() || 'unassigned'
  const isAssigned = Boolean(raw.company_id?.trim())
  const requests = raw.request_count ?? 0
  let tps = Number.NaN
  if (
    requests > 0 &&
    raw.output_metrics_available !== false &&
    raw.avg_tps !== null &&
    (raw.avg_tps > 0 ||
      (raw.avg_tps === 0 && raw.output_metrics_available === true))
  ) {
    tps = raw.avg_tps
  }
  return {
    model: raw.model_name,
    companyId,
    companyName: raw.company_name || (isAssigned ? companyId : 'Unassigned'),
    groupName: raw.group_name || raw.group || raw.group_id || 'Unassigned',
    isAssigned,
    requests,
    successRate: requests > 0 ? (raw.success_rate ?? Number.NaN) : Number.NaN,
    avgLatencyMs:
      requests > 0 ? (raw.avg_latency_ms ?? Number.NaN) : Number.NaN,
    tps,
    firstAttemptRate: raw.first_attempt_success_rate ?? Number.NaN,
    retryRecoveryRate: raw.retry_recovery_rate ?? Number.NaN,
    p95LatencyMs: raw.p95_latency_ms ?? Number.NaN,
    p95TtftMs: raw.p95_ttft_ms ?? Number.NaN,
    incidents: raw.incidents ?? Number.NaN,
    series: raw.recent_success_series ?? [],
    effectiveSlo: raw.effective_slo ?? Number.NaN,
    minSamples: raw.min_samples ?? 100,
    monitoringEnabled: raw.monitoring_enabled ?? true,
    configured: raw.configured ?? false,
  }
}

export function getPerformanceModelStatus(row: PerformanceModelRow): {
  label: string
  tone: 'healthy' | 'degraded' | 'unknown'
} {
  if (row.requests === 0) return { label: 'No requests', tone: 'unknown' }
  if (!row.monitoringEnabled) {
    return { label: 'Monitoring disabled', tone: 'unknown' }
  }
  if (row.requests < row.minSamples) {
    return { label: 'Insufficient samples', tone: 'unknown' }
  }
  if (!Number.isFinite(row.effectiveSlo)) {
    return { label: 'Target not configured', tone: 'unknown' }
  }
  if (!Number.isFinite(row.successRate)) {
    return { label: 'Unknown', tone: 'unknown' }
  }
  if (row.successRate >= row.effectiveSlo) {
    return { label: 'Meets target', tone: 'healthy' }
  }
  return { label: 'Below target', tone: 'degraded' }
}
