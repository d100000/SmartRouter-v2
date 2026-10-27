/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { z } from 'zod'

const sloSchema = z
  .number('SLO must be between 0 and 100')
  .min(0, 'SLO must be between 0 and 100')
  .max(100, 'SLO must be between 0 and 100')
  .optional()

export const performanceConfigurationSchema = z
  .object({
    companies: z
      .array(
        z
          .object({
            id: z.string().min(1),
            name: z
              .string()
              .trim()
              .min(1, 'Company name is required')
              .max(128, 'Name must not exceed 128 characters'),
            status: z.union([z.string(), z.number()]).optional(),
            slo: sloSchema,
            slo_confirmed: z.boolean().optional(),
            preset_key: z.string().optional(),
            owner: z.string().optional(),
            note: z.string().optional(),
            groups: z
              .array(
                z
                  .object({
                    id: z.string().min(1),
                    name: z
                      .string()
                      .trim()
                      .min(1, 'Model group name is required')
                      .max(128, 'Name must not exceed 128 characters'),
                    models: z.array(z.string().min(1)),
                    enabled: z.boolean().optional(),
                    slo: sloSchema,
                    slo_confirmed: z.boolean().optional(),
                    preset_key: z.string().optional(),
                    p95_latency_ms: z.number().optional(),
                    min_samples: z
                      .number(
                        'Minimum samples must be a whole number between 0 and 1000000000'
                      )
                      .int(
                        'Minimum samples must be a whole number between 0 and 1000000000'
                      )
                      .min(
                        0,
                        'Minimum samples must be a whole number between 0 and 1000000000'
                      )
                      .max(
                        1000000000,
                        'Minimum samples must be a whole number between 0 and 1000000000'
                      )
                      .optional(),
                  })
                  .passthrough()
              )
              .max(100, 'A company supports at most 100 model groups')
              .optional(),
          })
          .passthrough()
      )
      .max(100, 'At most 100 companies are supported'),
  })
  .superRefine((config, ctx) => {
    const owners = new Set<string>()
    for (const [companyIndex, company] of config.companies.entries()) {
      for (const [groupIndex, group] of (company.groups ?? []).entries()) {
        for (const model of group.models) {
          if (owners.has(model)) {
            ctx.addIssue({
              code: 'custom',
              message: 'A model can belong to only one model group',
              path: ['companies', companyIndex, 'groups', groupIndex, 'models'],
            })
          }
          owners.add(model)
        }
      }
    }
  })

export type PerformanceConfigurationValues = z.infer<
  typeof performanceConfigurationSchema
>
