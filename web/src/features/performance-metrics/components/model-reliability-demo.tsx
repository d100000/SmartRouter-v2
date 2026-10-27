/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.
*/
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  ArrowDown,
  ArrowUp,
  AlertTriangle,
  Columns3,
  DatabaseZap,
  HeartPulse,
  Gauge,
  RefreshCcw,
  RotateCcw,
  Settings2,
  TriangleAlert,
} from 'lucide-react'
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { cn } from '@/lib/utils'

import {
  getPerformanceHealthCompanies,
  getPerformanceHealthSummary,
} from '../api'
import { formatLatency, formatThroughput, formatUptimePct } from '../lib/format'
import {
  getPerformanceModelStatus,
  normalizePerformanceModel,
  type PerformanceColumnId,
} from '../lib/model-status'
import type { PerformanceHealthCompany } from '../types'
import { PerformanceCollectionHealthStatus } from './performance-collection-health'
import { PerformanceConfigDialog } from './performance-config-dialog'
import { PerformanceModelTable } from './performance-model-table'
import { PerformanceReliabilityChart } from './performance-reliability-chart'

const PERFORMANCE_WINDOW_HOURS = 24
const COLUMN_STORAGE_KEY = 'performance-health-model-columns-v1'
type ColumnId = PerformanceColumnId

const DEFAULT_COLUMNS: ColumnId[] = [
  'model',
  'company',
  'requests',
  'success',
  'avg',
  'tps',
  'status',
]
const ALL_COLUMNS: Array<{ id: ColumnId; label: string; required?: boolean }> =
  [
    { id: 'model', label: 'Model', required: true },
    { id: 'company', label: 'Company / group' },
    { id: 'requests', label: 'Requests (24h)' },
    { id: 'success', label: 'Final success rate', required: true },
    { id: 'first', label: 'First-attempt success' },
    { id: 'retry', label: 'Retry recovery' },
    { id: 'p95', label: 'P95 latency' },
    { id: 'avg', label: 'Average latency' },
    { id: 'ttft', label: 'P95 TTFT' },
    { id: 'tps', label: 'Output throughput' },
    { id: 'incidents', label: 'Incidents' },
    { id: 'status', label: 'Status' },
  ]
const ALL_COLUMN_ORDER = ALL_COLUMNS.map((column) => column.id)

function loadColumnPreferences(): { order: ColumnId[]; visible: ColumnId[] } {
  const fallback = { order: ALL_COLUMN_ORDER, visible: DEFAULT_COLUMNS }
  if (typeof window === 'undefined') return fallback
  try {
    const parsed = JSON.parse(
      window.localStorage.getItem(COLUMN_STORAGE_KEY) ?? '{}'
    ) as { order?: ColumnId[]; visible?: ColumnId[] }
    const valid = new Set(ALL_COLUMNS.map((column) => column.id))
    const order = (parsed.order ?? ALL_COLUMN_ORDER).filter((id) =>
      valid.has(id)
    )
    const visible = (parsed.visible ?? DEFAULT_COLUMNS).filter((id) =>
      valid.has(id)
    )
    return {
      order: [...new Set([...order, ...ALL_COLUMN_ORDER])] as ColumnId[],
      visible: [...new Set([...visible, 'model', 'success'])] as ColumnId[],
    }
  } catch {
    return fallback
  }
}

function persistColumnPreferences(order: ColumnId[], visible: ColumnId[]) {
  if (typeof window !== 'undefined') {
    try {
      window.localStorage.setItem(
        COLUMN_STORAGE_KEY,
        JSON.stringify({ order, visible })
      )
    } catch {
      // Keep the current view usable when browser storage is unavailable.
    }
  }
}

function MetricTile(props: {
  label: string
  value: string
  hint: string
  tone: 'success' | 'warning' | 'info'
}) {
  return (
    <div className='bg-muted/35 min-w-0 rounded-xl border border-transparent px-3 py-3'>
      <div className='text-muted-foreground truncate text-[11px] font-medium'>
        {props.label}
      </div>
      <div
        className={cn(
          'mt-1 font-mono text-lg font-semibold tracking-tight tabular-nums',
          props.tone === 'success' && 'text-emerald-600 dark:text-emerald-400',
          props.tone === 'warning' && 'text-amber-600 dark:text-amber-400',
          props.tone === 'info' && 'text-blue-600 dark:text-blue-400'
        )}
      >
        {props.value}
      </div>
      <div className='text-muted-foreground mt-1 truncate text-[11px]'>
        {props.hint}
      </div>
    </div>
  )
}

function SignalLine(props: {
  label: string
  value: string
  detail?: string
  tone: 'success' | 'warning' | 'danger' | 'neutral'
}) {
  return (
    <div
      role='group'
      aria-label={props.label}
      className='flex items-center justify-between gap-3 border-b py-2.5 last:border-b-0'
    >
      <div className='min-w-0'>
        <div className='truncate text-sm font-medium'>{props.label}</div>
        {props.detail && (
          <div className='text-muted-foreground mt-0.5 truncate text-xs'>
            {props.detail}
          </div>
        )}
      </div>
      <span
        className={cn(
          'shrink-0 font-mono text-sm font-semibold tabular-nums',
          props.tone === 'success' && 'text-emerald-600 dark:text-emerald-400',
          props.tone === 'warning' && 'text-amber-600 dark:text-amber-400',
          props.tone === 'danger' && 'text-red-600 dark:text-red-400',
          props.tone === 'neutral' && 'text-muted-foreground'
        )}
      >
        {props.value}
      </span>
    </div>
  )
}

function DiagnosticCard(props: {
  title: string
  description: string
  icon: ReactNode
  rows: Array<{ label: string; value: string }>
}) {
  return (
    <Card>
      <CardHeader className='border-b pb-3'>
        <CardTitle className='flex items-center gap-2 text-base'>
          <span className='text-muted-foreground'>{props.icon}</span>
          {props.title}
        </CardTitle>
        <CardDescription className='text-xs'>
          {props.description}
        </CardDescription>
      </CardHeader>
      <CardContent className='space-y-2 pt-2'>
        {props.rows.map((row) => (
          <div
            key={row.label}
            className='flex items-center justify-between gap-3 border-b py-2 last:border-b-0'
          >
            <span className='text-muted-foreground text-sm'>{row.label}</span>
            <span className='font-mono text-xs tabular-nums'>{row.value}</span>
          </div>
        ))}
      </CardContent>
    </Card>
  )
}

function ColumnSettings(props: {
  open: boolean
  onOpenChange: (open: boolean) => void
  order: ColumnId[]
  visible: ColumnId[]
  onSave: (order: ColumnId[], visible: ColumnId[]) => void
  t: (key: string) => string
}) {
  const [draftOrder, setDraftOrder] = useState(props.order)
  const [draftVisible, setDraftVisible] = useState(props.visible)
  const [draggingId, setDraggingId] = useState<ColumnId | null>(null)
  useEffect(() => {
    if (props.open) {
      setDraftOrder(props.order)
      setDraftVisible(props.visible)
    }
  }, [props.open, props.order, props.visible])
  const move = (id: ColumnId, direction: -1 | 1) => {
    const index = draftOrder.indexOf(id)
    const next = index + direction
    if (index < 0 || next < 0 || next >= draftOrder.length) return
    const order = [...draftOrder]
    ;[order[index], order[next]] = [order[next], order[index]]
    setDraftOrder(order)
  }
  const dropBefore = (sourceId: ColumnId, targetId: ColumnId) => {
    if (sourceId === targetId) return
    const sourceIndex = draftOrder.indexOf(sourceId)
    const targetIndex = draftOrder.indexOf(targetId)
    if (sourceIndex < 0 || targetIndex < 0) return
    const order = [...draftOrder]
    const [source] = order.splice(sourceIndex, 1)
    if (!source) return
    order.splice(order.indexOf(targetId), 0, source)
    setDraftOrder(order)
  }
  const reset = () => {
    setDraftOrder(ALL_COLUMN_ORDER)
    setDraftVisible(DEFAULT_COLUMNS)
  }
  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className='sm:max-w-lg'>
        <DialogHeader>
          <DialogTitle>{props.t('Model table columns')}</DialogTitle>
          <DialogDescription>
            {props.t('Choose fields and order for the model table')}
          </DialogDescription>
        </DialogHeader>
        <div className='max-h-[55vh] space-y-2 overflow-y-auto'>
          {draftOrder.map((id) => {
            const column = ALL_COLUMNS.find((item) => item.id === id)
            if (!column) return null
            return (
              <div
                key={id}
                draggable
                onDragStart={(event) => {
                  event.dataTransfer.effectAllowed = 'move'
                  event.dataTransfer.setData('text/plain', id)
                  setDraggingId(id)
                }}
                onDragOver={(event) => event.preventDefault()}
                onDrop={(event) => {
                  event.preventDefault()
                  dropBefore(
                    event.dataTransfer.getData('text/plain') as ColumnId,
                    id
                  )
                  setDraggingId(null)
                }}
                onDragEnd={() => setDraggingId(null)}
                className={cn(
                  'flex items-center gap-2 rounded-lg border px-3 py-2',
                  draggingId === id && 'opacity-50'
                )}
              >
                <Checkbox
                  checked={draftVisible.includes(id)}
                  disabled={column.required}
                  onCheckedChange={(checked) =>
                    setDraftVisible((current) =>
                      checked
                        ? [...current, id]
                        : current.filter((item) => item !== id)
                    )
                  }
                  aria-label={props.t(column.label)}
                />
                <span className='min-w-0 flex-1 text-sm'>
                  {props.t(column.label)}
                </span>
                <Button
                  variant='ghost'
                  size='icon-sm'
                  onClick={() => move(id, -1)}
                  disabled={draftOrder.indexOf(id) === 0}
                  aria-label={props.t('Move up')}
                >
                  <ArrowUp />
                </Button>
                <Button
                  variant='ghost'
                  size='icon-sm'
                  onClick={() => move(id, 1)}
                  disabled={draftOrder.indexOf(id) === draftOrder.length - 1}
                  aria-label={props.t('Move down')}
                >
                  <ArrowDown />
                </Button>
              </div>
            )
          })}
        </div>
        <DialogFooter>
          <Button variant='ghost' onClick={reset}>
            {props.t('Reset')}
          </Button>
          <Button variant='outline' onClick={() => props.onOpenChange(false)}>
            {props.t('Cancel')}
          </Button>
          <Button
            onClick={() => {
              props.onSave(draftOrder, draftVisible)
              props.onOpenChange(false)
            }}
          >
            {props.t('Apply')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function ModelReliabilityDemo() {
  const { t, i18n } = useTranslation()
  const queryClient = useQueryClient()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language) ?? 'en-US'
  const [selectedCompany, setSelectedCompany] = useState('all')
  const [selectedModel, setSelectedModel] = useState('')
  const [includePresets, setIncludePresets] = useState(false)
  const [refreshEnabled, setRefreshEnabled] = useState(true)
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const [columnPreferences, setColumnPreferences] = useState(
    loadColumnPreferences
  )
  const [columnDialogOpen, setColumnDialogOpen] = useState(false)
  const [configDialogOpen, setConfigDialogOpen] = useState(false)
  const query = useQuery({
    queryKey: [
      'performance-health-summary',
      PERFORMANCE_WINDOW_HOURS,
      ...(includePresets ? ['all-presets'] : []),
    ],
    queryFn: async () =>
      requireServerSuccess(
        await getPerformanceHealthSummary(
          PERFORMANCE_WINDOW_HOURS,
          includePresets
        )
      ),
    staleTime: 60_000,
    retry: false,
    refetchInterval: refreshEnabled ? 60_000 : false,
    refetchIntervalInBackground: false,
    meta: { errorToast: false },
  })
  const configQuery = useQuery({
    queryKey: ['performance-health-companies'],
    queryFn: async () =>
      requireServerSuccess(await getPerformanceHealthCompanies()),
    staleTime: 60_000,
    retry: false,
    meta: { errorToast: false },
  })
  const rows = useMemo(
    () => (query.data?.data.models ?? []).map(normalizePerformanceModel),
    [query.data]
  )
  const configuredCompanies = configQuery.data?.data.companies
  const summaryCompanies = query.data?.data.companies
  const availableCompanies = useMemo(() => {
    const merged = new Map<string, PerformanceHealthCompany>()
    for (const company of configuredCompanies ?? []) {
      merged.set(company.id, company)
    }
    for (const company of summaryCompanies ?? []) {
      if (!merged.has(company.id)) merged.set(company.id, company)
    }
    for (const row of rows) {
      if (!merged.has(row.companyId)) {
        merged.set(row.companyId, {
          id: row.companyId,
          name: row.companyName,
          groups: [],
        })
      }
    }
    return [...merged.values()]
  }, [configuredCompanies, rows, summaryCompanies])
  const filteredRows = useMemo(
    () =>
      selectedCompany === 'all'
        ? rows
        : rows.filter((row) => row.companyId === selectedCompany),
    [rows, selectedCompany]
  )
  const activeModel =
    filteredRows.find((row) => row.model === selectedModel) ??
    filteredRows.find((row) => row.requests > 0) ??
    filteredRows[0]
  useEffect(() => {
    if (activeModel && activeModel.model !== selectedModel) {
      setSelectedModel(activeModel.model)
    }
  }, [activeModel, selectedModel])
  const trafficRows = filteredRows.filter((row) => row.requests > 0)
  const requests = trafficRows.reduce((sum, row) => sum + row.requests, 0)
  const summary = query.data?.data.summary
  let successRate = Number.NaN
  if (
    requests > 0 &&
    selectedCompany === 'all' &&
    Number.isFinite(summary?.success_rate)
  ) {
    successRate = summary?.success_rate ?? Number.NaN
  } else if (requests > 0) {
    successRate =
      trafficRows.reduce(
        (sum, row) => sum + row.successRate * row.requests,
        0
      ) / requests
  }
  const averageLatency =
    requests > 0
      ? trafficRows.reduce(
          (sum, row) => sum + row.avgLatencyMs * row.requests,
          0
        ) / requests
      : Number.NaN
  let throughput = Number.NaN
  if (
    requests > 0 &&
    selectedCompany === 'all' &&
    summary &&
    summary.output_metrics_available !== false &&
    (summary.avg_tps > 0 ||
      (summary.avg_tps === 0 && summary.output_metrics_available === true))
  ) {
    throughput = summary.avg_tps
  }
  const configuredModelCount = new Set(
    (configuredCompanies ?? []).flatMap((company) =>
      (company.groups ?? []).flatMap((group) => group.models ?? [])
    )
  ).size
  const mappedTrafficCount = trafficRows.filter((row) => row.isAssigned).length
  const unassignedRows = trafficRows.filter((row) => !row.isAssigned)
  const belowTargetRows = trafficRows.filter(
    (row) => getPerformanceModelStatus(row).tone === 'degraded'
  )
  const generatedAt = Number(
    query.data?.data.generated_at ?? query.data?.data.data_freshness
  )
  const refreshedAt =
    Number.isFinite(generatedAt) && generatedAt > 0
      ? new Intl.DateTimeFormat(locale, {
          hour: '2-digit',
          minute: '2-digit',
          second: '2-digit',
        }).format(new Date(generatedAt * 1000))
      : '—'
  const visibleColumns = columnPreferences.order.flatMap((id) => {
    const column = ALL_COLUMNS.find((item) => item.id === id)
    return column && columnPreferences.visible.includes(id) ? [column] : []
  })

  const refresh = () => {
    void query.refetch()
    void configQuery.refetch()
  }
  const onConfigurationSaved = () => {
    void queryClient.invalidateQueries({
      queryKey: ['performance-health-summary'],
    })
    void queryClient.invalidateQueries({
      queryKey: ['performance-health-companies'],
    })
  }

  return (
    <div className='space-y-4 sm:space-y-5'>
      <div className='bg-card flex flex-col justify-between gap-3 rounded-xl border p-4 sm:p-5'>
        <div>
          <h3 className='flex items-center gap-2 text-xl font-semibold'>
            <HeartPulse className='text-primary size-5' />
            {t('Model reliability')}
          </h3>
          <p className='text-muted-foreground mt-1 text-sm'>
            {t(
              'Request-level metrics count a retry-recovered request once. Attempt-level diagnostics are not connected yet.'
            )}
          </p>
          <p className='text-muted-foreground mt-1 text-xs'>
            {t('24 hourly buckets, including the current partial hour')}
          </p>
        </div>
        <div className='flex flex-wrap items-center gap-2'>
          <label
            htmlFor='company-filter'
            className='text-muted-foreground text-xs'
          >
            {t('Company')}
          </label>
          <NativeSelect
            id='company-filter'
            value={selectedCompany}
            size='sm'
            onChange={(event) => {
              setSelectedCompany(event.target.value)
              setSelectedModel('')
            }}
          >
            <NativeSelectOption value='all'>
              {t('All companies')}
            </NativeSelectOption>
            {availableCompanies.map((company) => (
              <NativeSelectOption key={company.id} value={company.id}>
                {company.name === 'Unassigned' ? t('Unassigned') : company.name}
              </NativeSelectOption>
            ))}
          </NativeSelect>
          <Button
            variant='outline'
            size='sm'
            onClick={() => setConfigDialogOpen(true)}
          >
            <Settings2 />
            {t('Configure models')}
          </Button>
          <Button
            variant='ghost'
            size='icon-sm'
            onClick={refresh}
            disabled={query.isFetching}
            aria-label={t('Refresh')}
          >
            <RefreshCcw className={cn(query.isFetching && 'animate-spin')} />
          </Button>
          <Button
            variant='ghost'
            size='sm'
            onClick={() => setRefreshEnabled((value) => !value)}
            aria-pressed={refreshEnabled}
          >
            {refreshEnabled ? t('Pause refresh') : t('Resume refresh')}
          </Button>
          <label className='ml-auto flex cursor-pointer items-center gap-2 text-xs'>
            <Checkbox
              checked={includePresets}
              onCheckedChange={(value) => setIncludePresets(value === true)}
            />
            {t('Show all preset models')}
          </label>
        </div>
      </div>
      {query.isLoading && <LoadingState />}
      {query.isError && (
        <ErrorState
          title={
            query.data
              ? t('Unable to refresh model reliability')
              : t('Unable to load model reliability')
          }
          description={getServerErrorMessage(
            query.error,
            t('Company configuration is still available.')
          )}
          onRetry={refresh}
        />
      )}
      {query.isError && query.data && (
        <Alert role='status' aria-label={t('Cached metrics')}>
          <AlertDescription>
            {t(
              'Showing previously loaded metrics from {{time}}. The latest query failed.',
              { time: refreshedAt }
            )}
          </AlertDescription>
        </Alert>
      )}
      {!query.isLoading && (!query.isError || query.data) && (
        <>
          <div className='grid grid-cols-2 gap-2 sm:grid-cols-4 sm:gap-3'>
            <MetricTile
              label={t('Requests (24h)')}
              value={formatNumber(requests, locale)}
              hint={t('Completed request samples')}
              tone='info'
            />
            <MetricTile
              label={t('Final request success rate')}
              value={formatUptimePct(successRate)}
              hint={`${formatNumber(requests, locale)} ${t('requests')}`}
              tone='info'
            />
            <MetricTile
              label={t('Average latency')}
              value={formatLatency(averageLatency)}
              hint={t('All sampled request outcomes')}
              tone='info'
            />
            <MetricTile
              label={t('Output throughput')}
              value={formatThroughput(throughput, true)}
              hint={
                selectedCompany === 'all'
                  ? t('Performance metrics for the last 24 hours')
                  : t('Not available for this scope')
              }
              tone='info'
            />
          </div>
          <div className='grid gap-4 xl:grid-cols-[minmax(0,1.35fr)_minmax(18rem,0.65fr)]'>
            <Card>
              <CardHeader>
                <CardTitle>{t('24-hour success rate curve')}</CardTitle>
                <CardDescription>
                  {activeModel
                    ? `${activeModel.model} · ${activeModel.companyName === 'Unassigned' ? t('Unassigned') : activeModel.companyName}`
                    : t('Select a model to inspect its trend')}
                </CardDescription>
              </CardHeader>
              <CardContent>
                <PerformanceReliabilityChart
                  model={activeModel}
                  windowStart={query.data?.data.window_start}
                  windowEnd={query.data?.data.window_end}
                  bucketSeconds={query.data?.data.bucket_seconds}
                  locale={locale}
                />
              </CardContent>
            </Card>
            <Card>
              <CardHeader>
                <CardTitle>{t('Company model groups')}</CardTitle>
                <CardDescription>
                  {t(
                    'Configured models and models with traffic are counted separately'
                  )}
                </CardDescription>
              </CardHeader>
              <CardContent className='space-y-2'>
                {availableCompanies.map((company) => {
                  const companyRows = rows.filter(
                    (row) => row.companyId === company.id
                  )
                  const count = companyRows.reduce(
                    (sum, row) => sum + row.requests,
                    0
                  )
                  const rate =
                    count > 0
                      ? companyRows
                          .filter((row) => row.requests > 0)
                          .reduce(
                            (sum, row) => sum + row.successRate * row.requests,
                            0
                          ) / count
                      : Number.NaN
                  return (
                    <Button
                      key={company.id}
                      variant={
                        selectedCompany === company.id ? 'secondary' : 'outline'
                      }
                      className='h-auto w-full min-w-0 justify-between gap-3 py-3 text-left'
                      aria-pressed={selectedCompany === company.id}
                      onClick={() => {
                        setSelectedCompany((current) =>
                          current === company.id ? 'all' : company.id
                        )
                        setSelectedModel('')
                      }}
                    >
                      <span className='min-w-0'>
                        <span className='block truncate'>
                          {company.name === 'Unassigned'
                            ? t('Unassigned')
                            : company.name}
                        </span>
                        <span className='text-muted-foreground block text-xs'>
                          {formatNumber(
                            companyRows.filter((row) => row.requests > 0)
                              .length,
                            locale
                          )}{' '}
                          {t('Models with traffic')}
                        </span>
                      </span>
                      <span className='shrink-0 text-right'>
                        <span className='block font-mono'>
                          {formatUptimePct(rate)}
                        </span>
                        <span className='text-muted-foreground block text-xs'>
                          {formatNumber(count, locale)} {t('requests')}
                        </span>
                      </span>
                    </Button>
                  )
                })}
              </CardContent>
            </Card>
          </div>
          <Card>
            <CardHeader>
              <div className='flex flex-wrap items-center justify-between gap-2'>
                <div>
                  <CardTitle>{t('Model performance metrics')}</CardTitle>
                  <CardDescription>
                    {t('Edit fields and order to match your analysis workflow')}
                  </CardDescription>
                </div>
                <Button
                  variant='outline'
                  size='sm'
                  onClick={() => setColumnDialogOpen(true)}
                >
                  <Columns3 />
                  {t('Fields')}
                </Button>
              </div>
            </CardHeader>
            <CardContent>
              <PerformanceModelTable
                rows={filteredRows}
                columns={visibleColumns}
                selectedModel={activeModel?.model}
                onSelectModel={setSelectedModel}
                locale={locale}
              />
            </CardContent>
          </Card>
          <div className='grid gap-4 lg:grid-cols-2'>
            <Card>
              <CardHeader>
                <CardTitle className='flex items-center gap-2'>
                  <DatabaseZap className='size-4' />
                  {t('Data quality')}
                </CardTitle>
              </CardHeader>
              <CardContent>
                <SignalLine
                  label={t('Assigned models with traffic')}
                  value={`${formatNumber(mappedTrafficCount, locale)} / ${formatNumber(trafficRows.length, locale)}`}
                  detail={t(
                    'Assigned / all models with traffic in this window'
                  )}
                  tone={
                    mappedTrafficCount === trafficRows.length
                      ? 'neutral'
                      : 'warning'
                  }
                />
                <SignalLine
                  label={t('Configured models')}
                  value={
                    configQuery.isSuccess
                      ? formatNumber(configuredModelCount, locale)
                      : '—'
                  }
                  detail={t('Saved company model memberships')}
                  tone='neutral'
                />
                <SignalLine
                  label={t('Query refreshed at')}
                  value={refreshedAt}
                  tone='neutral'
                />
                <PerformanceCollectionHealthStatus
                  collection={query.data?.data.collection}
                  lastSampleAt={query.data?.data.last_sample_at}
                  stale={query.isError}
                />
              </CardContent>
            </Card>
            <Card>
              <CardHeader>
                <CardTitle className='flex items-center gap-2'>
                  <AlertTriangle className='size-4' />
                  {t('Routing watchpoints')}
                </CardTitle>
              </CardHeader>
              <CardContent>
                <SignalLine
                  label={t('Models below target')}
                  value={formatNumber(belowTargetRows.length, locale)}
                  detail={t('Confirmed SLO targets with sufficient samples')}
                  tone={belowTargetRows.length ? 'danger' : 'neutral'}
                />
                <SignalLine
                  label={t('Unassigned')}
                  value={formatNumber(unassignedRows.length, locale)}
                  detail={t(
                    'Models that are being used but not configured in the system'
                  )}
                  tone={unassignedRows.length ? 'warning' : 'neutral'}
                />
                <p className='text-muted-foreground py-2 text-xs'>
                  {t(
                    'Performance history is grouped by the current model ownership configuration.'
                  )}
                </p>
                <p className='text-muted-foreground pb-2 text-xs'>
                  {t(
                    'Historical buckets keep their original token measurements; output-token splits cannot be backfilled.'
                  )}
                </p>
              </CardContent>
            </Card>
          </div>
          <Collapsible open={advancedOpen} onOpenChange={setAdvancedOpen}>
            <CollapsibleTrigger render={<Button variant='outline' size='sm' />}>
              {t('Advanced diagnostics')}
            </CollapsibleTrigger>
            <CollapsibleContent className='space-y-3 pt-3'>
              <p className='text-muted-foreground text-sm'>
                {t(
                  'Attempt-level metrics, percentiles and terminal coverage are not connected yet.'
                )}
              </p>
              <div className='grid gap-4 lg:grid-cols-3'>
                <DiagnosticCard
                  title={t('Error breakdown')}
                  description={t(
                    'Attempt-level telemetry is not available yet'
                  )}
                  icon={<TriangleAlert className='size-4' />}
                  rows={[
                    { label: t('429 rate'), value: t('Not collected yet') },
                    { label: t('5xx rate'), value: t('Not collected yet') },
                    { label: t('Timeout rate'), value: t('Not collected yet') },
                  ]}
                />
                <DiagnosticCard
                  title={t('Routing resilience')}
                  description={t(
                    'Retries, fallback and circuit breaker telemetry'
                  )}
                  icon={<RotateCcw className='size-4' />}
                  rows={[
                    {
                      label: t('Retry request rate'),
                      value: t('Not collected yet'),
                    },
                    {
                      label: t('Fallback success rate'),
                      value: t('Not collected yet'),
                    },
                    {
                      label: t('Circuit breaker trips'),
                      value: t('Not collected yet'),
                    },
                  ]}
                />
                <DiagnosticCard
                  title={t('Latency tail')}
                  description={t(
                    'Percentiles require mergeable latency histograms'
                  )}
                  icon={<Gauge className='size-4' />}
                  rows={[
                    { label: t('P50 latency'), value: t('Not collected yet') },
                    { label: t('P95 latency'), value: t('Not collected yet') },
                    { label: t('P99 latency'), value: t('Not collected yet') },
                  ]}
                />
              </div>
            </CollapsibleContent>
          </Collapsible>
        </>
      )}
      <ColumnSettings
        open={columnDialogOpen}
        onOpenChange={setColumnDialogOpen}
        order={columnPreferences.order}
        visible={columnPreferences.visible}
        onSave={(order, visible) => {
          setColumnPreferences({ order, visible })
          persistColumnPreferences(order, visible)
        }}
        t={t}
      />
      <PerformanceConfigDialog
        open={configDialogOpen}
        onOpenChange={setConfigDialogOpen}
        onSaved={onConfigurationSaved}
        observedModels={rows
          .filter((row) => row.requests > 0)
          .map((row) => row.model)}
        channelModels={rows
          .filter((row) => row.configured)
          .map((row) => row.model)}
      />
    </div>
  )
}
