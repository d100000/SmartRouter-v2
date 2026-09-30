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
import { zodResolver } from '@hookform/resolvers/zod'
import { Controller, useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'

import type { SchedulingConfig, SchedulingSnapshot } from '../types'

export function ConfigDialog(props: {
  data: SchedulingSnapshot
  onClose: () => void
  onSave: (config: SchedulingConfig) => void
  pending: boolean
  error: boolean
}) {
  const { t } = useTranslation()
  const positiveNumber = z
    .number({ error: t('Enter a valid number') })
    .positive(t('Must be greater than zero'))
  const schema = z.object({
    enabled: z.boolean(),
    target_success_rate: positiveNumber.max(100, t('Must not exceed 100')),
    target_ttft_ms: positiveNumber.min(1).max(3600000),
    default_capacity: positiveNumber.int().max(100000),
    channel_overrides: z.array(
      z.object({
        channel_id: z.number(),
        weight: z
          .number({ error: t('Enter a valid number') })
          .min(0)
          .max(1000000)
          .nullable(),
        capacity: positiveNumber.int().max(100000).nullable(),
        capacity_key: z.string().max(128),
      })
    ),
  })
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: {
      ...props.data.config,
      target_success_rate: props.data.config.target_success_rate * 100,
      channel_overrides: props.data.channels.map((channel) => {
        const override = (props.data.config.channel_overrides ?? []).find(
          (item) => item.channel_id === channel.channel_id
        )
        return {
          channel_id: channel.channel_id,
          weight: override?.weight ?? null,
          capacity: override?.capacity ?? null,
          capacity_key: override?.capacity_key ?? '',
        }
      }),
    },
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !props.pending) props.onClose()
      }}
      title={t('Routing configuration')}
      description={`${props.data.group} / ${props.data.model}`}
      footer={
        <>
          <Button
            variant='outline'
            onClick={props.onClose}
            disabled={props.pending}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='submit'
            form='scheduling-config'
            disabled={props.pending}
          >
            {props.pending ? t('Saving...') : t('Save')}
          </Button>
        </>
      }
    >
      <form
        id='scheduling-config'
        onSubmit={form.handleSubmit((values) =>
          props.onSave({
            ...values,
            target_success_rate: values.target_success_rate / 100,
          })
        )}
        className='space-y-5'
      >
        <div className='flex items-center justify-between gap-4'>
          <Label htmlFor='scheduling-enabled'>
            {t('Enable intelligent routing')}
          </Label>
          <Controller
            control={form.control}
            name='enabled'
            render={({ field }) => (
              <Switch
                id='scheduling-enabled'
                checked={field.value}
                onCheckedChange={field.onChange}
                disabled={props.pending}
              />
            )}
          />
        </div>
        <p className='text-muted-foreground text-xs leading-relaxed'>
          {t(
            'Cold start follows native priority and configured weights. Dynamic routing activates after 50 original requests in 30 minutes, then gradually replaces the initial weights.'
          )}
        </p>
        <div className='grid gap-4 sm:grid-cols-3'>
          {(
            [
              {
                name: 'target_success_rate',
                label: t('Success target (%)'),
                step: 'any',
                min: 0.000001,
                max: 100,
              },
              {
                name: 'target_ttft_ms',
                label: t('First-output target (ms)'),
                step: 1,
                min: 1,
                max: 3600000,
              },
              {
                name: 'default_capacity',
                label: t('Default safe concurrency'),
                step: 1,
                min: 1,
                max: 100000,
              },
            ] as const
          ).map((field) => (
            <div key={field.name} className='space-y-2'>
              <Label htmlFor={field.name}>{field.label}</Label>
              <Input
                id={field.name}
                type='number'
                step={field.step}
                min={field.min}
                max={field.max}
                disabled={props.pending}
                aria-invalid={!!form.formState.errors[field.name]}
                {...form.register(field.name, { valueAsNumber: true })}
              />
              {form.formState.errors[field.name] && (
                <p role='alert' className='text-destructive text-xs'>
                  {form.formState.errors[field.name]?.message}
                </p>
              )}
            </div>
          ))}
        </div>
        <div className='space-y-3 border-t pt-4'>
          <h3 className='text-sm font-semibold'>{t('Channel overrides')}</h3>
          <p className='text-muted-foreground text-xs'>
            {t(
              'Blank values inherit channel weight or default capacity. Matching capacity pool keys share upstream concurrency across channels and groups.'
            )}
          </p>
          {props.data.channels.map((channel, index) => (
            <fieldset
              key={channel.channel_id}
              className='space-y-3 rounded-lg border p-3'
              disabled={props.pending}
            >
              <legend
                className='max-w-full truncate px-1 text-xs font-medium'
                title={channel.name}
              >
                {channel.name}
              </legend>
              <div className='grid gap-3 sm:grid-cols-3'>
                <div className='space-y-2'>
                  <Label htmlFor={`weight-${channel.channel_id}`}>
                    {t('Initial configured weight')}
                  </Label>
                  <Input
                    id={`weight-${channel.channel_id}`}
                    type='number'
                    min={0}
                    max={1000000}
                    step='any'
                    placeholder={t('Inherit')}
                    aria-invalid={
                      !!form.formState.errors.channel_overrides?.[index]?.weight
                    }
                    {...form.register(`channel_overrides.${index}.weight`, {
                      setValueAs: (value: string) =>
                        value == null || value === '' ? null : Number(value),
                    })}
                  />
                </div>
                <div className='space-y-2'>
                  <Label htmlFor={`capacity-${channel.channel_id}`}>
                    {t('Safe concurrency')}
                  </Label>
                  <Input
                    id={`capacity-${channel.channel_id}`}
                    type='number'
                    min={1}
                    max={100000}
                    placeholder={t('Inherit')}
                    aria-invalid={
                      !!form.formState.errors.channel_overrides?.[index]
                        ?.capacity
                    }
                    {...form.register(`channel_overrides.${index}.capacity`, {
                      setValueAs: (value: string) =>
                        value == null || value === '' ? null : Number(value),
                    })}
                  />
                </div>
                <div className='space-y-2'>
                  <Label htmlFor={`pool-${channel.channel_id}`}>
                    {t('Capacity pool key')}
                  </Label>
                  <Input
                    id={`pool-${channel.channel_id}`}
                    maxLength={128}
                    placeholder={t('Channel default')}
                    {...form.register(
                      `channel_overrides.${index}.capacity_key`
                    )}
                  />
                </div>
              </div>
              {form.formState.errors.channel_overrides?.[index] && (
                <p role='alert' className='text-destructive text-xs'>
                  {t('Check channel weight, capacity and pool key.')}
                </p>
              )}
            </fieldset>
          ))}
        </div>
        {props.error && (
          <p role='alert' className='text-destructive text-sm'>
            {t('Configuration was not saved. Review the error and try again.')}
          </p>
        )}
      </form>
    </Dialog>
  )
}
