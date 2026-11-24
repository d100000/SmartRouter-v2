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
import { useState } from 'react'
import { Controller, useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { PasswordInput } from '@/components/password-input'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Switch } from '@/components/ui/switch'
import { getServerErrorMessage } from '@/lib/server-error-message'
import { cn } from '@/lib/utils'

import { listModels } from './api'
import type { AnalysisConfig, AnalysisModelOption, ConfigInput } from './types'

const recognizedClass =
  'bg-emerald-500/15 text-emerald-700 dark:text-emerald-400'

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
      context_window_tokens: z.number().int().min(4096).max(2000000),
      chunk_input_tokens: z.number().int().min(256).max(2000000),
      max_output_tokens: z.number().int().min(128).max(32768),
      safety_margin_tokens: z.number().int().min(256).max(2000000),
      max_calls: z.number().int().min(1).max(1000),
      max_concurrency: z.number().int().min(0).max(8),
      report_language: z.string(),
      enabled: z.boolean(),
      api_key: z.string().max(4096),
      clear_key: z.boolean(),
    })
    .refine(
      (value) =>
        value.context_window_tokens >
        value.max_output_tokens + value.safety_margin_tokens + 512,
      {
        message: t(
          'The context limit must exceed the output reserve and safety margin.'
        ),
        path: ['context_window_tokens'],
      }
    )
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: {
      ...props.config,
      context_window_tokens: props.config.context_window_tokens ?? 131072,
      chunk_input_tokens: props.config.chunk_input_tokens ?? 65536,
      max_output_tokens: props.config.max_output_tokens ?? 16384,
      safety_margin_tokens: props.config.safety_margin_tokens ?? 4096,
      max_calls: props.config.max_calls ?? 200,
      max_concurrency: props.config.max_concurrency ?? 0,
      api_key: '',
      clear_key: false,
    },
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
      name: 'context_window_tokens',
      label: t('Model context limit (tokens)'),
      type: 'number',
      min: 4096,
      max: 2000000,
    },
    {
      name: 'chunk_input_tokens',
      label: t('Segment input target (tokens)'),
      type: 'number',
      min: 256,
      max: 2000000,
    },
    {
      name: 'max_output_tokens',
      label: t('Maximum output (tokens)'),
      type: 'number',
      min: 128,
      max: 32768,
    },
    {
      name: 'safety_margin_tokens',
      label: t('Context safety margin (tokens)'),
      type: 'number',
      min: 256,
      max: 2000000,
    },
    {
      name: 'max_calls',
      label: t('Model call budget'),
      type: 'number',
      min: 1,
      max: 1000,
    },
    {
      name: 'max_concurrency',
      label: t('Parallel model calls (0 = preset)'),
      type: 'number',
      min: 0,
      max: 8,
    },
  ] as const
  const [models, setModels] = useState<AnalysisModelOption[] | null>(null)
  const [loadingModels, setLoadingModels] = useState(false)
  const [modelError, setModelError] = useState('')
  const currentModel = form.watch('model')
  const currentOption = models?.find((option) => option.id === currentModel)
  const currentPreset =
    currentOption?.preset?.name ??
    (props.config.preset && props.config.model === currentModel
      ? props.config.preset
      : '')
  const fetchModels = async () => {
    if (!(await form.trigger('endpoint'))) return
    setLoadingModels(true)
    setModelError('')
    try {
      const data = await listModels({
        endpoint: form.getValues('endpoint'),
        api_key: form.getValues('api_key'),
      })
      setModels(data.models)
    } catch (error) {
      setModels(null)
      setModelError(
        getServerErrorMessage(
          error,
          t('Unable to fetch models. Check the endpoint and API key.')
        )
      )
    } finally {
      setLoadingModels(false)
    }
  }
  const applyModel = (option: AnalysisModelOption) => {
    const update = { shouldDirty: true, shouldValidate: true }
    form.setValue('model', option.id, update)
    form.setValue(
      'context_window_tokens',
      option.suggested_context_window,
      update
    )
    form.setValue('chunk_input_tokens', option.suggested_input_budget, update)
    form.setValue(
      'max_output_tokens',
      Math.min(option.suggested_output_reserve, 32768),
      update
    )
    form.setValue('max_concurrency', option.preset?.concurrency ?? 0, update)
  }
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
        <p className='text-muted-foreground text-sm'>
          {t(
            'Analysis uses non-streaming model requests. Set the context limit supported by your provider; oversized evidence is split automatically.'
          )}
        </p>
        <div className='space-y-2 rounded-md border p-3'>
          <div className='flex flex-wrap items-center justify-between gap-2'>
            <div className='min-w-0 space-y-1'>
              <p className='text-sm font-medium'>{t('Model presets')}</p>
              <p className='text-muted-foreground text-xs'>
                {t(
                  'Fetch the models of this endpoint. Recognized models fill in context, input and output settings automatically.'
                )}
              </p>
            </div>
            <Button
              type='button'
              variant='outline'
              size='sm'
              onClick={() => void fetchModels()}
              disabled={props.pending || loadingModels}
            >
              {loadingModels ? t('Fetching...') : t('Fetch models')}
            </Button>
          </div>
          {modelError && (
            <p role='alert' className='text-destructive text-sm'>
              {modelError}
            </p>
          )}
          {models && models.length === 0 && (
            <p className='text-muted-foreground text-sm'>
              {t('No models returned')}
            </p>
          )}
          {models && models.length > 0 && (
            <ul className='max-h-56 space-y-1 overflow-y-auto'>
              {models.map((option) => (
                <li key={option.id}>
                  <button
                    type='button'
                    onClick={() => applyModel(option)}
                    disabled={props.pending}
                    className={cn(
                      'hover:bg-muted flex w-full items-center justify-between gap-2 rounded-md px-2 py-1.5 text-left text-sm',
                      option.id === currentModel && 'bg-muted'
                    )}
                  >
                    <span className='min-w-0 truncate font-mono text-xs'>
                      {option.id}
                    </span>
                    {option.preset ? (
                      <Badge className={recognizedClass}>
                        {t('Recognized')} · {option.preset.name}
                      </Badge>
                    ) : (
                      <span className='text-muted-foreground shrink-0 text-xs'>
                        {option.context_length
                          ? t('Context {{value}} tokens', {
                              value: option.context_length,
                            })
                          : t('Not recognized')}
                      </span>
                    )}
                  </button>
                </li>
              ))}
            </ul>
          )}
          {currentPreset ? (
            <p className='text-xs text-emerald-700 dark:text-emerald-400'>
              {t('Recognized preset: {{name}}', { name: currentPreset })}
            </p>
          ) : (
            models && (
              <p className='text-muted-foreground text-xs'>
                {t(
                  'The selected model is not recognized. Conservative defaults apply; adjust them to your provider.'
                )}
              </p>
            )
          )}
        </div>
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
                {t('Analyze the previous seven days automatically')}
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
