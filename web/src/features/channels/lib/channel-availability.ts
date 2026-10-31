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
import { z } from 'zod'

import { toIntlLocale } from '@/i18n/languages'

const windowSchema = z.object({
  id: z.string().optional(),
  start: z.string(),
  end: z.string(),
})
export const availabilityConfigSchema = z.object({
  enabled: z.boolean().default(false),
  timezone: z.string().default('Asia/Shanghai'),
  mode: z.enum(['daily', 'weekly']).default('daily'),
  rules: z
    .array(windowSchema.extend({ weekdays: z.array(z.number()).optional() }))
    .default([]),
  exceptions: z
    .array(
      z.object({
        id: z.string().optional(),
        date: z.string(),
        windows: z.array(windowSchema),
      })
    )
    .default([]),
})
export type AvailabilityConfig = z.infer<typeof availabilityConfigSchema>
export type AvailabilityWindow = z.infer<typeof windowSchema>

export function defaultAvailability(): AvailabilityConfig {
  return {
    enabled: false,
    timezone: 'Asia/Shanghai',
    mode: 'daily',
    rules: [
      {
        id: 'daily-window',
        start: '09:00',
        end: '18:00',
        weekdays: [1, 2, 3, 4, 5, 6, 7],
      },
    ],
    exceptions: [],
  }
}

export function readAvailability(
  value?: string | null
): AvailabilityConfig | null {
  if (!value?.trim() || value.trim() === '{}') return defaultAvailability()
  try {
    const result = availabilityConfigSchema.safeParse(JSON.parse(value))
    if (!result.success) return null
    // Persist stable row identifiers in the JSON so removing a preceding row
    // cannot transfer focus or control state. Legacy schedules receive IDs
    // deterministically on first edit; the gateway ignores these UI identifiers.
    return {
      ...result.data,
      rules: result.data.rules.map((rule, index) => ({
        ...rule,
        id: rule.id || `rule-${index}`,
      })),
      exceptions: result.data.exceptions.map((exception, index) => ({
        ...exception,
        id: exception.id || `exception-${index}`,
        windows: exception.windows.map((window, index) => ({
          ...window,
          id: window.id || `window-${index}`,
        })),
      })),
    }
  } catch {
    return null
  }
}

function validTime(value: string, end = false): boolean {
  return (end && value === '24:00') || /^(?:[01]\d|2[0-3]):[0-5]\d$/.test(value)
}

export function availabilityValidationError(
  value?: string
): string | undefined {
  if (value && new TextEncoder().encode(value).length > 32768) {
    return 'Invalid channel availability configuration'
  }
  const config = readAvailability(value)
  if (!config) return 'Invalid channel availability configuration'
  if (!config.enabled) return undefined
  if (!config.timezone || config.timezone === 'Local') {
    return 'Select a valid IANA time zone'
  }
  try {
    new Intl.DateTimeFormat('en', { timeZone: config.timezone })
  } catch {
    return 'Select a valid IANA time zone'
  }
  if (
    config.rules.length < 1 ||
    config.rules.length > 32 ||
    config.exceptions.length > 100
  ) {
    return 'Availability requires 1 to 32 rules and at most 100 date exceptions'
  }
  for (const rule of config.rules) {
    if (!validTime(rule.start) || !validTime(rule.end, true)) {
      return 'Availability times must use HH:MM; only an end time may be 24:00'
    }
    if (rule.start === rule.end) {
      return 'Availability start and end must differ; use 00:00 to 24:00 for all day'
    }
    if (config.mode === 'weekly') {
      if (!rule.weekdays?.length || rule.weekdays.length > 7) {
        return 'Select at least one weekday for each availability rule'
      }
      if (
        new Set(rule.weekdays).size !== rule.weekdays.length ||
        rule.weekdays.some(
          (day) => !Number.isInteger(day) || day < 1 || day > 7
        )
      ) {
        return 'Availability weekdays must be unique values from 1 to 7'
      }
    }
  }
  const dates = new Set<string>()
  for (const exception of config.exceptions) {
    const date = new Date(`${exception.date}T00:00:00Z`)
    if (
      !/^\d{4}-\d{2}-\d{2}$/.test(exception.date) ||
      Number.isNaN(date.getTime()) ||
      date.toISOString().slice(0, 10) !== exception.date ||
      exception.windows.length > 8
    ) {
      return 'Date exceptions require a valid date and at most 8 time windows'
    }
    if (dates.has(exception.date)) {
      return 'Availability date exceptions must have unique dates'
    }
    dates.add(exception.date)
    for (const window of exception.windows) {
      if (!validTime(window.start) || !validTime(window.end, true)) {
        return 'Availability times must use HH:MM; only an end time may be 24:00'
      }
      if (window.start >= window.end) {
        return 'Date exception windows must end later on the same day'
      }
    }
  }
  return undefined
}

export function formatAvailabilityTime(
  timestamp: number,
  timezone: string,
  language?: string
): string {
  return new Intl.DateTimeFormat(toIntlLocale(language), {
    timeZone: timezone,
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hourCycle: 'h23',
  }).format(new Date(timestamp * 1000))
}
