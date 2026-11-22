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

import {
  accountPasswordSchema,
  SUPPLIER_PASSWORD_MIN_LENGTH,
} from '@/lib/password-policy'

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
})

const positive = z
  .string()
  .regex(/^[1-9]\d*$/, 'Enter a positive whole number or leave blank')
  .refine(
    (value) => Number.isSafeInteger(Number(value)),
    'Enter a valid number'
  )
export const supplierAccountSchema = z
  .object({
    name: z.string().trim().min(1, 'Name is required').max(100),
    username: z.string().trim(),
    password: z.string(),
    allowed_types: z.array(z.string()),
    allowed_models: z.array(z.string()),
    allow_all_templates: z.boolean(),
    allow_all_models: z.boolean(),
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
  .superRefine((values, ctx) => {
    if (!values.allow_all_templates && !values.allowed_types.length) {
      ctx.addIssue({
        code: 'custom',
        path: ['allowed_types'],
        message: 'Select a channel template',
      })
    }
    if (!values.allow_all_models && !values.allowed_models.length) {
      ctx.addIssue({
        code: 'custom',
        path: ['allowed_models'],
        message: 'Select at least one model',
      })
    }
  })

export const supplierAccountCredentialsSchema = z.object({
  username: z.string().trim().min(1, 'Name is required').max(20),
  password: accountPasswordSchema.refine(
    (value) => [...value].length >= SUPPLIER_PASSWORD_MIN_LENGTH,
    'Supplier account passwords must contain at least 15 characters'
  ),
})

export const supplierRoutingSchema = z.object({
  models: z.array(z.string()),
  groups: z
    .array(z.string())
    .min(1, 'Select at least one group')
    .refine(
      (value) => new TextEncoder().encode(value.join(',')).length <= 64,
      'Selected group names are too long'
    ),
  priority: z
    .string()
    .regex(/^-?\d+$/, 'Enter a whole number')
    .refine(
      (value) => Number(value) >= -1000 && Number(value) <= 1000,
      'Enter a whole number from -1000 to 1000'
    ),
  weight: z
    .string()
    .regex(/^\d+$/, 'Enter a valid number')
    .refine((value) => Number(value) <= 1000, 'Enter a valid number'),
})
