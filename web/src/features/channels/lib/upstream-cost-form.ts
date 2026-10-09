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

export const costRatioInputSchema = z
  .string()
  .trim()
  .refine(
    (value) =>
      value === '' ||
      (Number.isFinite(Number(value)) &&
        Number(value) >= 0 &&
        Number(value) <= 100),
    'Enter a cost ratio between 0 and 100, or leave empty.'
  )

export const channelCostFormSchema = z.object({
  cost_ratio: costRatioInputSchema,
})

export const upstreamCredentialFormSchema = z.object({
  alias: z.string().trim().max(100, 'Name must be 100 characters or fewer.'),
  supplier_id: z.string(),
  tags: z
    .array(z.string().trim())
    .refine(
      (tags) =>
        tags.length <= 20 &&
        tags.every((tag) => tag.length > 0 && tag.length <= 40),
      'Use up to 20 tags, with no more than 40 characters each.'
    ),
  cost_ratio: costRatioInputSchema,
})
