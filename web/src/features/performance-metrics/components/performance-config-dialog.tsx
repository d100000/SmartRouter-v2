/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { MultiSelect, type Option } from '@/components/multi-select'
import { Button } from '@/components/ui/button'
import { Field, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'
import {
  getServerErrorMessage,
  getServerErrorStatus,
  requireServerSuccess,
} from '@/lib/server-error-message'

import {
  applyPerformanceHealthPresets,
  getPerformanceHealthCompanies,
  getPerformanceHealthPresets,
  previewPerformanceHealthPresets,
  savePerformanceHealthConfig,
} from '../api'
import {
  performanceConfigurationSchema,
  type PerformanceConfigurationValues,
} from '../lib/configuration-schema'
import type {
  PerformanceHealthCompany,
  PerformanceHealthConfig,
  PerformanceHealthPresetPreview,
} from '../types'
import { PerformanceCompanyEditor } from './performance-company-editor'

type ConfigurationAction =
  | { type: 'discard' | 'reload' }
  | { type: 'remove-company' | 'disable-company'; companyId: string }
  | {
      type: 'remove-group' | 'disable-group' | 'move'
      companyId: string
      groupId: string
    }

function newConfigurationId(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 9)}`
}

function normalizeConfigurationCompanies(
  companies: PerformanceHealthCompany[]
) {
  return structuredClone(companies).map((company) => ({
    ...company,
    groups: (company.groups ?? []).map((group) => ({
      ...group,
      models: group.models ?? [],
    })),
  }))
}

export function PerformanceConfigDialog(props: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSaved: () => void
  observedModels?: string[]
  channelModels?: string[]
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const client = useQueryClient()
  const [loaded, setLoaded] = useState<PerformanceHealthConfig | null>(null)
  const [search, setSearch] = useState('')
  const [preview, setPreview] = useState<PerformanceHealthPresetPreview | null>(
    null
  )
  const [action, setAction] = useState<ConfigurationAction | null>(null)
  const [movingModels, setMovingModels] = useState<string[]>([])
  const [targetGroup, setTargetGroup] = useState('')
  const form = useForm<PerformanceConfigurationValues>({
    resolver: zodResolver(performanceConfigurationSchema),
    defaultValues: { companies: [] },
  })
  const companies = form.watch('companies')
  const isDirty = form.formState.isDirty
  const query = useQuery({
    queryKey: ['performance-health-companies'],
    queryFn: async () =>
      requireServerSuccess(await getPerformanceHealthCompanies()),
    enabled: props.open,
    retry: false,
    refetchOnWindowFocus: false,
    meta: { errorToast: false },
  })
  const catalog = useQuery({
    queryKey: ['performance-health-presets'],
    queryFn: getPerformanceHealthPresets,
    enabled: props.open,
    staleTime: 60_000,
    retry: false,
    meta: { errorToast: false },
  })
  useEffect(() => {
    if (!props.open) {
      setLoaded(null)
      setPreview(null)
      setAction(null)
      setSearch('')
      return
    }
    if (loaded || !query.data || query.isError) return
    const config = query.data.data
    const version = config.version ?? config.config_version
    if (typeof version !== 'number' || !Number.isFinite(version)) return
    setLoaded(structuredClone({ ...config, version }))
    form.reset({ companies: normalizeConfigurationCompanies(config.companies) })
  }, [props.open, query.data, query.isError, loaded, form])

  const save = useMutation({
    mutationFn: savePerformanceHealthConfig,
    meta: { errorToast: false },
    onSuccess: (config) => {
      client.setQueryData(['performance-health-companies'], {
        success: true,
        data: config,
      })
      form.reset({
        companies: normalizeConfigurationCompanies(config.companies),
      })
      props.onSaved()
      props.onOpenChange(false)
    },
  })
  const previewMutation = useMutation({
    mutationFn: previewPerformanceHealthPresets,
    meta: { errorToast: false },
    onSuccess: setPreview,
  })
  const apply = useMutation({
    mutationFn: applyPerformanceHealthPresets,
    meta: { errorToast: false },
    onSuccess: (config) => {
      client.setQueryData(['performance-health-companies'], {
        success: true,
        data: config,
      })
      form.reset({
        companies: normalizeConfigurationCompanies(config.companies),
      })
      props.onSaved()
      props.onOpenChange(false)
    },
  })
  const resetSave = save.reset
  const resetApply = apply.reset
  const resetPreview = previewMutation.reset
  useEffect(() => {
    if (!props.open) {
      resetSave()
      resetApply()
      resetPreview()
    }
  }, [props.open, resetSave, resetApply, resetPreview])
  const isBusy = save.isPending || apply.isPending || previewMutation.isPending
  const version = query.data?.data.version ?? query.data?.data.config_version
  const newerVersion =
    loaded !== null && typeof version === 'number' && version !== loaded.version
  const conflict =
    getServerErrorStatus(save.error) === 409 ||
    getServerErrorStatus(apply.error) === 409
  const ready = loaded !== null && !query.isPending && !query.isError
  const saveError = save.error ?? apply.error ?? previewMutation.error
  const options = useMemo(() => {
    const values = new Map<string, Option>()
    for (const company of catalog.data?.companies ?? []) {
      for (const group of company.groups ?? []) {
        for (const model of group.models ?? []) {
          values.set(model, {
            value: model,
            label: model,
            hint: t('Preset model'),
          })
        }
      }
    }
    for (const model of props.channelModels ?? []) {
      values.set(model, {
        value: model,
        label: model,
        hint: t('Channel models'),
      })
    }
    for (const model of props.observedModels ?? []) {
      values.set(model, {
        value: model,
        label: model,
        hint: t('Observed model'),
      })
    }
    for (const company of companies) {
      for (const group of company.groups ?? []) {
        for (const model of group.models) {
          if (!values.has(model)) {
            values.set(model, {
              value: model,
              label: model,
              hint: t('Custom model'),
            })
          }
        }
      }
    }
    return [...values.values()]
  }, [catalog.data, companies, props.observedModels, props.channelModels, t])
  const keyword = search.trim().toLowerCase()
  const groups = companies.flatMap((company) =>
    (company.groups ?? []).map((group) => ({ company, group }))
  )
  const selectedCompany =
    action && 'companyId' in action
      ? companies.find((company) => company.id === action.companyId)
      : undefined
  const selectedGroup =
    action && 'groupId' in action
      ? selectedCompany?.groups?.find((group) => group.id === action.groupId)
      : undefined
  const removedCount =
    selectedGroup?.models.length ??
    selectedCompany?.groups?.reduce(
      (sum, group) => sum + group.models.length,
      0
    ) ??
    0
  const hasAdditions = Boolean(
    preview &&
    (preview.added_companies.length ||
      preview.added_groups.length ||
      preview.added_models.length)
  )
  const updateCompanies = (next: PerformanceHealthCompany[]) => {
    form.setValue('companies', next, {
      shouldDirty: true,
      shouldValidate: true,
    })
    setPreview(null)
  }
  const close = (open: boolean) => {
    if (open || isBusy) return
    if (isDirty) setAction({ type: 'discard' })
    else props.onOpenChange(false)
  }
  const reload = async () => {
    const result = await query.refetch()
    if (!result.data || result.isError) return
    const config = result.data.data
    const loadedVersion = config.version ?? config.config_version
    if (typeof loadedVersion !== 'number') return
    setLoaded(structuredClone({ ...config, version: loadedVersion }))
    form.reset({ companies: normalizeConfigurationCompanies(config.companies) })
    setPreview(null)
    save.reset()
    apply.reset()
    previewMutation.reset()
  }
  const confirmAction = () => {
    if (!action) return
    if (action.type === 'discard') props.onOpenChange(false)
    if (action.type === 'reload') void reload()
    if (action.type === 'remove-company') {
      updateCompanies(
        companies.filter((company) => company.id !== action.companyId)
      )
    }
    if (action.type === 'remove-group') {
      updateCompanies(
        companies.map((company) =>
          company.id === action.companyId
            ? {
                ...company,
                groups: (company.groups ?? []).filter(
                  (group) => group.id !== action.groupId
                ),
              }
            : company
        )
      )
    }
    if (action.type === 'disable-company') {
      updateCompanies(
        companies.map((company) =>
          company.id === action.companyId
            ? { ...company, status: 'disabled' }
            : company
        )
      )
    }
    if (action.type === 'disable-group') {
      updateCompanies(
        companies.map((company) =>
          company.id === action.companyId
            ? {
                ...company,
                groups: (company.groups ?? []).map((group) =>
                  group.id === action.groupId
                    ? { ...group, enabled: false }
                    : group
                ),
              }
            : company
        )
      )
    }
    if (action.type === 'move') {
      updateCompanies(
        companies.map((company) => ({
          ...company,
          groups: (company.groups ?? []).map((group) => {
            if (group.id === action.groupId) {
              return {
                ...group,
                models: group.models.filter(
                  (model) => !movingModels.includes(model)
                ),
              }
            }
            if (group.id === targetGroup) {
              return {
                ...group,
                models: [...new Set([...group.models, ...movingModels])],
              }
            }
            return group
          }),
        }))
      )
    }
    setAction(null)
  }
  let confirmationTitle = t('Discard unsaved changes?')
  let confirmationDescription = t('Your unsaved changes will be lost.')
  if (action?.type === 'reload') {
    confirmationTitle = t('Reload configuration?')
    confirmationDescription = t(
      'Reloading replaces your unsaved changes with the latest saved configuration.'
    )
  }
  if (action?.type === 'remove-company' || action?.type === 'remove-group') {
    confirmationTitle =
      action.type === 'remove-company'
        ? t('Delete company')
        : t('Delete model group')
    confirmationDescription = t(
      'This clears {{count}} model assignments. Performance history is kept.',
      { count: removedCount }
    )
  }
  if (action?.type === 'move') {
    confirmationTitle = t('Move models')
    confirmationDescription = t(
      'Select the models and destination group. Changes are applied when you save.'
    )
  }
  if (action?.type === 'disable-company' || action?.type === 'disable-group') {
    confirmationTitle =
      action.type === 'disable-company'
        ? t('Disable company monitoring?')
        : t('Disable model group monitoring?')
    confirmationDescription = t(
      'This disables monitoring for {{count}} model assignments. Performance history is kept.',
      { count: removedCount }
    )
  }

  return (
    <>
      <Dialog
        open={props.open}
        onOpenChange={close}
        title={t('Company and model groups')}
        description={t(
          'Manage model ownership without changing channels or billing.'
        )}
        contentClassName='sm:max-w-3xl'
        footer={
          <>
            <Button
              type='button'
              variant='outline'
              disabled={!ready || isBusy || companies.length >= 100}
              onClick={() =>
                updateCompanies([
                  ...companies,
                  {
                    id: newConfigurationId('company'),
                    name: t('New company'),
                    status: 'active',
                    slo: 99.9,
                    slo_confirmed: false,
                    groups: [],
                  },
                ])
              }
            >
              {t('Add company')}
            </Button>
            <Button
              type='button'
              variant='outline'
              disabled={isBusy}
              onClick={() => close(false)}
            >
              {t('Cancel')}
            </Button>
            <Button
              type='submit'
              form='performance-config-form'
              disabled={
                !ready || isBusy || !isDirty || newerVersion || conflict
              }
            >
              {save.isPending ? t('Saving') : t('Save changes')}
            </Button>
          </>
        }
      >
        {query.isPending && <LoadingState />}
        {query.isError && (
          <ErrorState
            description={getServerErrorMessage(query.error)}
            onRetry={() => void query.refetch()}
          />
        )}
        {query.isSuccess && !loaded && typeof version !== 'number' && (
          <ErrorState
            description={t(
              'Configuration version is unavailable. Reload before editing.'
            )}
            onRetry={() => void query.refetch()}
          />
        )}
        {loaded && (
          <div className='space-y-5'>
            <div className='bg-muted/30 space-y-2 rounded-lg border p-3 text-sm'>
              <p>
                {t(
                  'Supplement presets adds missing companies and models while keeping manual assignments and SLO targets.'
                )}
              </p>
              <Button
                type='button'
                size='sm'
                variant='outline'
                disabled={
                  !ready || isBusy || isDirty || newerVersion || conflict
                }
                onClick={() => previewMutation.mutate()}
              >
                {previewMutation.isPending
                  ? t('Loading...')
                  : t('Preview preset additions')}
              </Button>
              {isDirty && (
                <p className='text-muted-foreground text-xs'>
                  {t('Save or discard your edits before previewing presets.')}
                </p>
              )}
              {catalog.isError && (
                <p className='text-muted-foreground text-xs'>
                  {t(
                    'Preset suggestions are unavailable. You can still edit saved models.'
                  )}{' '}
                  <Button
                    type='button'
                    variant='link'
                    size='sm'
                    onClick={() => void catalog.refetch()}
                  >
                    {t('Retry')}
                  </Button>
                </p>
              )}
            </div>
            {(newerVersion || conflict) && (
              <div
                role='alert'
                className='border-warning/40 bg-warning/5 space-y-2 rounded-lg border p-3 text-sm'
              >
                <p>{t('Configuration changed. Reload before saving again.')}</p>
                <Button
                  type='button'
                  variant='outline'
                  size='sm'
                  disabled={isBusy || query.isFetching}
                  onClick={() =>
                    isDirty ? setAction({ type: 'reload' }) : void reload()
                  }
                >
                  {t('Reload configuration')}
                </Button>
              </div>
            )}
            {saveError && (
              <p role='alert' className='text-destructive text-sm break-words'>
                {getServerErrorMessage(saveError)}
              </p>
            )}
            {preview && (
              <section
                aria-label={t('Preset additions')}
                className='space-y-3 rounded-lg border p-3 text-sm'
              >
                <p className='font-medium'>{t('Preset additions')}</p>
                <p>
                  {t(
                    '{{companies}} companies, {{groups}} groups, {{models}} models',
                    {
                      companies: formatNumber(
                        preview.added_companies.length,
                        locale
                      ),
                      groups: formatNumber(preview.added_groups.length, locale),
                      models: formatNumber(preview.added_models.length, locale),
                    }
                  )}
                </p>
                <div className='max-h-40 space-y-2 overflow-y-auto text-xs break-all'>
                  {preview.added_companies.length > 0 && (
                    <p>
                      {t('Companies')}: {preview.added_companies.join(', ')}
                    </p>
                  )}
                  {preview.added_groups.length > 0 && (
                    <p>
                      {t('Model groups')}: {preview.added_groups.join(', ')}
                    </p>
                  )}
                  {preview.added_models.length > 0 && (
                    <p>
                      {t('Models')}: {preview.added_models.join(', ')}
                    </p>
                  )}
                  {preview.conflicts.length > 0 && (
                    <p>
                      {t('Manual assignments kept')}:{' '}
                      {preview.conflicts.join(', ')}
                    </p>
                  )}
                  {preview.skipped_models.length > 0 && (
                    <p>
                      {t('Excluded models kept')}:{' '}
                      {preview.skipped_models.join(', ')}
                    </p>
                  )}
                </div>
                {!hasAdditions && <p>{t('Presets are already up to date.')}</p>}
                <Button
                  type='button'
                  size='sm'
                  disabled={!hasAdditions || isBusy || newerVersion || conflict}
                  onClick={() =>
                    apply.mutate({
                      expected_config_version: preview.expected_config_version,
                      preview_token: preview.preview_token,
                    })
                  }
                >
                  {apply.isPending ? t('Saving') : t('Apply preset additions')}
                </Button>
              </section>
            )}
            <Input
              aria-label={t('Search companies or models')}
              placeholder={t('Search companies or models')}
              value={search}
              onChange={(event) => setSearch(event.target.value)}
            />
            <form
              id='performance-config-form'
              noValidate
              onSubmit={form.handleSubmit((values) => {
                if (!ready || isBusy || !loaded || newerVersion || conflict) {
                  return
                }
                save.mutate({ ...loaded, companies: values.companies })
              })}
              className='space-y-5'
            >
              {companies.length === 0 && (
                <EmptyState
                  title={t('No company configuration available')}
                  description={t(
                    'Preview preset additions to add Kimi, Openai and Claude.'
                  )}
                />
              )}
              {companies.map((company, index) => {
                if (
                  keyword &&
                  !`${company.name} ${company.id} ${(company.groups ?? []).flatMap((group) => [group.name, ...group.models]).join(' ')}`
                    .toLowerCase()
                    .includes(keyword)
                ) {
                  return null
                }
                return (
                  <PerformanceCompanyEditor
                    key={company.id}
                    company={company}
                    options={options}
                    errors={form.formState.errors.companies?.[index]}
                    disabled={!ready || isBusy}
                    onChange={(value) => {
                      if (
                        value.status === 'disabled' &&
                        company.status !== 'disabled' &&
                        company.status !== 0
                      ) {
                        setAction({
                          type: 'disable-company',
                          companyId: company.id,
                        })
                        return
                      }
                      const disabledGroup = (value.groups ?? []).find(
                        (group) =>
                          group.enabled === false &&
                          company.groups?.find(
                            (existing) => existing.id === group.id
                          )?.enabled !== false
                      )
                      if (disabledGroup) {
                        setAction({
                          type: 'disable-group',
                          companyId: company.id,
                          groupId: disabledGroup.id,
                        })
                        return
                      }
                      updateCompanies(
                        companies.map((item) =>
                          item.id === company.id ? value : item
                        )
                      )
                    }}
                    onDelete={() =>
                      setAction({
                        type: 'remove-company',
                        companyId: company.id,
                      })
                    }
                    onAddGroup={() =>
                      updateCompanies(
                        companies.map((item) =>
                          item.id === company.id
                            ? {
                                ...item,
                                groups: [
                                  ...(item.groups ?? []),
                                  {
                                    id: newConfigurationId('group'),
                                    name: t('New model group'),
                                    models: [],
                                    enabled: true,
                                    slo: item.slo,
                                    slo_confirmed:
                                      item.slo_confirmed ?? (item.slo ?? 0) > 0,
                                    min_samples: 100,
                                    p95_latency_ms: 0,
                                  },
                                ],
                              }
                            : item
                        )
                      )
                    }
                    onDeleteGroup={(group) =>
                      setAction({
                        type: 'remove-group',
                        companyId: company.id,
                        groupId: group.id,
                      })
                    }
                    onMoveModels={(group) => {
                      setMovingModels([...group.models])
                      setTargetGroup('')
                      setAction({
                        type: 'move',
                        companyId: company.id,
                        groupId: group.id,
                      })
                    }}
                  />
                )
              })}
            </form>
          </div>
        )}
      </Dialog>
      <ConfirmDialog
        open={action !== null}
        onOpenChange={(open) => {
          if (!open) setAction(null)
        }}
        title={confirmationTitle}
        desc={confirmationDescription}
        confirmText={action?.type === 'move' ? t('Move models') : t('Continue')}
        destructive={
          action?.type === 'remove-company' ||
          action?.type === 'remove-group' ||
          action?.type === 'disable-company' ||
          action?.type === 'disable-group' ||
          action?.type === 'discard'
        }
        disabled={
          action?.type === 'move' && (!targetGroup || movingModels.length === 0)
        }
        handleConfirm={confirmAction}
      >
        {action?.type === 'move' && selectedGroup && (
          <div className='space-y-3'>
            <Field>
              <FieldLabel htmlFor='moving-models'>
                {t('Models to move')}
              </FieldLabel>
              <MultiSelect
                id='moving-models'
                options={selectedGroup.models.map((model) => ({
                  value: model,
                  label: model,
                }))}
                selected={movingModels}
                onChange={setMovingModels}
                maxVisibleChips={6}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor='target-model-group'>
                {t('Destination model group')}
              </FieldLabel>
              <NativeSelect
                id='target-model-group'
                value={targetGroup}
                onChange={(event) => setTargetGroup(event.target.value)}
              >
                <NativeSelectOption value=''>
                  {t('Select a model group')}
                </NativeSelectOption>
                {groups
                  .filter((item) => item.group.id !== selectedGroup.id)
                  .map((item) => (
                    <NativeSelectOption
                      key={item.group.id}
                      value={item.group.id}
                    >
                      {item.company.name} / {item.group.name}
                    </NativeSelectOption>
                  ))}
              </NativeSelect>
            </Field>
          </div>
        )}
      </ConfirmDialog>
    </>
  )
}
