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

import { channelLimitsSchema } from './limits'

export const supplierChannelSchema = z.object({
  name: z.string().trim().min(1, 'Name is required').max(100),
  template_id: z.string().min(1, 'Select a channel template'),
  base_url: z
    .string()
    .trim()
    .url('Enter a valid HTTPS URL')
    .refine((value) => {
      const url = URL.canParse(value) ? new URL(value) : null
      return (
        url?.protocol === 'https:' &&
        !url.username &&
        !url.password &&
        !url.search &&
        !url.hash
      )
    }, 'supplier upstream requires an HTTPS URL without credentials or query parameters'),
  key: z.string(),
  models: z.array(z.string()).min(1, 'Select at least one model'),
  remark: z
    .string()
    .refine(
      (value) => [...value].length <= 255,
      'Use no more than 255 characters'
    ),
  priority: z
    .string()
    .refine(
      (value) =>
        /^-?\d+$/.test(value) &&
        Number.isSafeInteger(Number(value)) &&
        Number(value) >= -1000 &&
        Number(value) <= 1000,
      'Enter a whole number from -1000 to 1000'
    ),
  ...channelLimitsSchema.shape,
})

const positive = z
  .string()
  .regex(/^[1-9]\d*$/, 'Enter a positive whole number or leave blank')
  .refine(
    (value) => Number.isSafeInteger(Number(value)),
    'Enter a valid number'
  )
export const supplierAccountSchema = z.object({
  name: z.string().trim().min(1, 'Name is required').max(100),
  user_id: z
    .string()
    .refine(
      (value) =>
        value === '' ||
        (/^[1-9]\d*$/.test(value) && Number.isSafeInteger(Number(value))),
      'Enter a valid user ID'
    ),
  allowed_types: z.array(z.string()).min(1, 'Select a channel template'),
  allowed_models: z.array(z.string()).min(1, 'Select at least one model'),
  group: z.string().trim().min(1, 'Group is required'),
  max_channels: positive.refine(
    (value) => Number(value) <= 10000,
    'Enter a valid number'
  ),
  cost_ratio: z
    .string()
    .refine(
      (value) =>
        value === '' ||
        (Number.isFinite(Number(value)) &&
          Number(value) >= 0 &&
          Number(value) <= 100),
      'cost ratio must be finite and between 0 and 100'
    ),
  pool_priority: z
    .string()
    .refine(
      (value) => /^-?\d+$/.test(value) && Number.isSafeInteger(Number(value)),
      'Enter a whole number'
    ),
  pool_capacity: z
    .string()
    .regex(/^\d+$/, 'Enter a valid number')
    .refine(
      (value) => Number.isSafeInteger(Number(value)),
      'Enter a valid number'
    ),
  pool_weight: positive.refine(
    (value) => Number(value) <= 1000,
    'Enter a valid number'
  ),
  max_output_tokens: z
    .string()
    .refine(
      (value) =>
        value === '' ||
        (/^\d+$/.test(value) && Number.isSafeInteger(Number(value))),
      'Enter a valid number'
    ),
  max_input_tokens: z
    .string()
    .refine(
      (value) =>
        value === '' ||
        (/^\d+$/.test(value) && Number.isSafeInteger(Number(value))),
      'Enter a valid number'
    )
    .optional(),
  supply_enabled: z.boolean(),
})
