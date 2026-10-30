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
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Switch } from '@/components/ui/switch'

import type { AnalysisConfig, ConfigInput } from './types'

export function ConfigDialog(props: {
  config: AnalysisConfig
  pending: boolean
  error: boolean
  onClose: () => void
  onSave: (input: ConfigInput) => void
}) {
  const { t } = useTranslation()
  const schema = z
    .object({
      endpoint: z.url().refine((value) => {
        if (!URL.canParse(value)) return false
        const url = new URL(value)
        return (
          url.protocol === 'https:' &&
          url.pathname.endsWith('/chat/completions') &&
          !url.search &&
          !url.hash &&
          !url.username &&
          !url.password
        )
      }, t('Use an HTTPS chat completions endpoint without credentials or query parameters')),
      model: z.string().trim().min(1).max(200),
      timezone: z.string().min(1),
      hour: z.number().int().min(0).max(23),
      max_attempts: z.number().int().min(1).max(5),
      timeout_seconds: z.number().int().min(30).max(600),
      idle_seconds: z.number().int().min(10).max(600),
      sample_limit: z.number().int().min(1).max(200),
      report_language: z.string(),
      enabled: z.boolean(),
      api_key: z.string().max(4096),
      clear_key: z.boolean(),
    })
    .refine((value) => value.idle_seconds <= value.timeout_seconds, {
      message: t('Idle timeout must not exceed the attempt timeout'),
      path: ['idle_seconds'],
    })
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: { ...props.config, api_key: '', clear_key: false },
  })
  const fields = [
    { name: 'endpoint', label: t('Chat completions endpoint'), type: 'url' },
    { name: 'model', label: t('Model'), type: 'text' },
    { name: 'timezone', label: t('Timezone'), type: 'text' },
    {
      name: 'hour',
      label: t('Daily analysis hour (0–23)'),
      type: 'number',
      min: 0,
      max: 23,
    },
    {
      name: 'max_attempts',
      label: t('Maximum attempts'),
      type: 'number',
      min: 1,
      max: 5,
    },
    {
      name: 'timeout_seconds',
      label: t('Attempt timeout (seconds)'),
      type: 'number',
      min: 30,
      max: 600,
    },
    {
      name: 'idle_seconds',
      label: t('No-token timeout (seconds)'),
      type: 'number',
      min: 10,
      max: 600,
    },
    {
      name: 'sample_limit',
      label: t('Maximum diagnostic samples'),
      type: 'number',
      min: 1,
      max: 200,
    },
  ] as const
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !props.pending) props.onClose()
      }}
      title={t('Analysis settings')}
      description={t(
        'Send diagnostic facts to your configured model. Prompts, response bodies and API keys are excluded from analysis evidence.'
      )}
      footer={
        <>
          <Button
            variant='outline'
            onClick={props.onClose}
            disabled={props.pending}
          >
            {t('Cancel')}
          </Button>
          <Button type='submit' form='analysis-config' disabled={props.pending}>
            {props.pending ? t('Saving...') : t('Save')}
          </Button>
        </>
      }
    >
      <form
        id='analysis-config'
        className='space-y-4'
        onSubmit={form.handleSubmit((values) =>
          props.onSave({ ...props.config, ...values })
        )}
      >
        <div className='grid gap-4 sm:grid-cols-2'>
          {fields.map((field) => (
            <div key={field.name} className='min-w-0 space-y-2'>
              <Label htmlFor={`analysis-config-${field.name}`}>
                {field.label}
              </Label>
              <Input
                id={`analysis-config-${field.name}`}
                type={field.type}
                {...('min' in field ? { min: field.min, max: field.max } : {})}
                {...form.register(field.name, {
                  valueAsNumber: field.type === 'number',
                })}
                aria-invalid={!!form.formState.errors[field.name]}
                disabled={props.pending}
              />
              {form.formState.errors[field.name] && (
                <p role='alert' className='text-destructive text-sm'>
                  {form.formState.errors[field.name]?.message}
                </p>
              )}
            </div>
          ))}
        </div>
        <div className='space-y-2'>
          <Label htmlFor='analysis-key'>{t('API Key')}</Label>
          <PasswordInput
            id='analysis-key'
            autoComplete='new-password'
            {...form.register('api_key')}
            disabled={props.pending}
            placeholder={
              props.config.api_key_configured
                ? t('Leave blank to keep the saved key')
                : t('Enter API key')
            }
          />
          <p className='text-muted-foreground text-xs'>
            {t(
              'Keys are encrypted on the server and never returned to this form.'
            )}
          </p>
        </div>
        <div className='space-y-2'>
          <Label htmlFor='analysis-language'>{t('Report language')}</Label>
          <NativeSelect
            id='analysis-language'
            {...form.register('report_language')}
            disabled={props.pending}
          >
            {[
              ['zh', '简体中文'],
              ['zh-TW', '繁體中文'],
              ['en', 'English'],
              ['fr', 'Français'],
              ['ja', '日本語'],
              ['ru', 'Русский'],
              ['vi', 'Tiếng Việt'],
            ].map(([value, label]) => (
              <NativeSelectOption key={value} value={value}>
                {label}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <Controller
          control={form.control}
          name='enabled'
          render={({ field }) => (
            <div className='flex items-center gap-2'>
              <Switch
                id='analysis-enabled'
                checked={field.value}
                onCheckedChange={field.onChange}
                disabled={props.pending}
              />
              <Label htmlFor='analysis-enabled'>
                {t('Analyze the previous day automatically')}
              </Label>
            </div>
          )}
        />
        {props.config.api_key_configured && (
          <Controller
            control={form.control}
            name='clear_key'
            render={({ field }) => (
              <div className='flex items-center gap-2'>
                <Switch
                  id='analysis-clear-key'
                  checked={field.value}
                  onCheckedChange={field.onChange}
                  disabled={props.pending}
                />
                <Label htmlFor='analysis-clear-key'>
                  {t('Remove saved API key')}
                </Label>
              </div>
            )}
          />
        )}
        {props.error && (
          <p role='alert' className='text-destructive text-sm'>
            {t('Settings were not saved. Review the error and try again.')}
          </p>
        )}
      </form>
    </Dialog>
  )
}
