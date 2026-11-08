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

import type { ChannelLimitConfig, ChannelLimitUpdate } from '../types'

const minuteLimit = z
  .string()
  .trim()
  .refine(
    (value) =>
      value === '' ||
      (/^[1-9]\d*$/.test(value) && Number.isSafeInteger(Number(value))),
    'Enter a positive whole number or leave blank'
  )
const moneyLimit = z
  .string()
  .trim()
  .refine(
    (value) =>
      value === '' ||
      (/^\d+(\.\d{1,9})?$/.test(value) &&
        Number(value) > 0 &&
        Number(value) <= 9_000_000_000),
    'Enter a positive USD amount or leave blank'
  )
export const channelLimitsSchema = z.object({
  rpm: minuteLimit,
  tpm: minuteLimit,
  daily_usd: moneyLimit,
})
export const channelLimitsEditorSchema = channelLimitsSchema.extend({
  reconcile_seconds: z
    .string()
    .trim()
    .refine(
      (value) =>
        /^[1-9]\d*$/.test(value) &&
        Number.isSafeInteger(Number(value)) &&
        Number(value) <= 86400,
      'Enter a whole number from 1 to 86400'
    ),
})
export type ChannelLimitsEditorForm = z.infer<typeof channelLimitsEditorSchema>
export type ChannelLimitsForm = z.infer<typeof channelLimitsSchema>
export function limitsFormValues(
  config: ChannelLimitConfig
): ChannelLimitsForm {
  return {
    rpm: config.rpm == null ? '' : String(config.rpm),
    tpm: config.tpm == null ? '' : String(config.tpm),
    daily_usd: config.daily_usd ?? '',
  }
}
export function limitsPayload(
  values: ChannelLimitsForm,
  revision: number
): ChannelLimitUpdate {
  const validated = channelLimitsSchema.parse(values)
  return {
    revision,
    rpm: validated.rpm === '' ? null : Number(validated.rpm),
    tpm: validated.tpm === '' ? null : Number(validated.tpm),
    daily_usd: validated.daily_usd === '' ? null : validated.daily_usd,
  }
}
