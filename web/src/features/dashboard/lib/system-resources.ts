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
import { formatNumber } from '@/lib/format'

export interface SystemResourcePoint {
  timestamp: number
  cpu_percent: number | null
  memory_percent: number | null
  memory_used: number | null
  memory_total: number | null
  disk_percent: number | null
  disk_used: number | null
  disk_total: number | null
  network_receive: number | null
  network_send: number | null
}

export interface SystemResourceSnapshot {
  node: string
  server_time: number
  sample: SystemResourcePoint | null
}

export interface SystemResourceHistory {
  node: string
  from: number
  to: number
  interval_seconds: number
  points: SystemResourcePoint[]
}

export function formatResourceBytes(
  bytes: number | null | undefined,
  locale: string | undefined
): string {
  if (bytes == null || !Number.isFinite(bytes) || bytes < 0) return '—'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  const unit = Math.min(
    units.length - 1,
    Math.floor(Math.log2(Math.max(1, bytes)) / 10)
  )
  return `${formatNumber(bytes / 1024 ** unit, locale, { maximumFractionDigits: 1 })} ${units[unit]}`
}

// Keep outages visible. Recharts otherwise joins adjacent stored points across
// a restart, suggesting that the server was continuously sampled.
export function buildResourceTimeline(
  history: SystemResourceHistory | undefined,
  current: SystemResourceSnapshot | undefined,
  from: number,
  to: number
): SystemResourcePoint[] {
  const points =
    history?.node === current?.node ? [...(history?.points ?? [])] : []
  const sample = current?.sample
  if (sample && sample.timestamp > (points.at(-1)?.timestamp ?? 0)) {
    points.push(sample)
  }
  const timeline: SystemResourcePoint[] = []
  for (const point of points) {
    if (point.timestamp < from || point.timestamp > to) continue
    const previous = timeline.at(-1)
    if (previous && point.timestamp - previous.timestamp > 90) {
      timeline.push({
        timestamp: previous.timestamp + 60,
        cpu_percent: null,
        memory_percent: null,
        memory_used: null,
        memory_total: null,
        disk_percent: null,
        disk_used: null,
        disk_total: null,
        network_receive: null,
        network_send: null,
      })
    }
    timeline.push(point)
  }
  return timeline
}
