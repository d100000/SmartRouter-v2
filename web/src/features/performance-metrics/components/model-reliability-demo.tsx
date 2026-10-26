/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.
*/
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Activity,
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
  ShieldCheck,
  TriangleAlert,
} from 'lucide-react'
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

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
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { IconBadge } from '@/components/ui/icon-badge'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Skeleton } from '@/components/ui/skeleton'
import {
  getPerformanceHealthCompanies,
  getPerformanceHealthSummary,
  savePerformanceHealthConfig,
} from '@/features/performance-metrics/api'
import {
  formatLatency,
  formatThroughput,
  formatUptimePct,
  getSuccessRateTextClass,
} from '@/features/performance-metrics/lib/format'
import type {
  PerformanceHealthCompany,
  PerformanceHealthModel,
  SuccessRatePoint,
} from '@/features/performance-metrics/types'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'
import { requireServerSuccess } from '@/lib/server-error-message'
import { cn } from '@/lib/utils'

const PERFORMANCE_WINDOW_HOURS = 24
const COLUMN_STORAGE_KEY = 'performance-health-model-columns-v1'
type ColumnId =
  | 'model'
  | 'company'
  | 'requests'
  | 'success'
  | 'first'
  | 'retry'
  | 'p95'
  | 'avg'
  | 'ttft'
  | 'tps'
  | 'incidents'
  | 'status'
type ModelRow = {
  model: string
  companyId: string
  companyName: string
  groupName: string
  isAssigned: boolean
  requests: number
  successRate: number
  firstAttemptRate: number
  retryRecoveryRate: number
  p95LatencyMs: number
  avgLatencyMs: number
  p95TtftMs: number
  tps: number
  incidents: number
  series: SuccessRatePoint[]
}

const DEFAULT_COLUMNS: ColumnId[] = [
  'model',
  'company',
  'requests',
  'success',
  'first',
  'retry',
  'p95',
  'avg',
  'ttft',
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

function loadColumnPreferences(): { order: ColumnId[]; visible: ColumnId[] } {
  const fallback = { order: DEFAULT_COLUMNS, visible: DEFAULT_COLUMNS }
  if (typeof window === 'undefined') return fallback
  try {
    const parsed = JSON.parse(
      window.localStorage.getItem(COLUMN_STORAGE_KEY) ?? '{}'
    ) as { order?: ColumnId[]; visible?: ColumnId[] }
    const valid = new Set(ALL_COLUMNS.map((column) => column.id))
    const order = (parsed.order ?? DEFAULT_COLUMNS).filter((id) =>
      valid.has(id)
    )
    const visible = (parsed.visible ?? DEFAULT_COLUMNS).filter((id) =>
      valid.has(id)
    )
    return {
      order: [...new Set([...order, ...DEFAULT_COLUMNS])] as ColumnId[],
      visible: [...new Set([...visible, 'model', 'success'])] as ColumnId[],
    }
  } catch {
    return fallback
  }
}

function persistColumnPreferences(order: ColumnId[], visible: ColumnId[]) {
  if (typeof window !== 'undefined') {
    window.localStorage.setItem(
      COLUMN_STORAGE_KEY,
      JSON.stringify({ order, visible })
    )
  }
}

function normalizeModel(raw: PerformanceHealthModel): ModelRow {
  const companyId = raw.company_id?.trim() || 'unassigned'
  const isAssigned = Boolean(raw.company_id?.trim())
  const companyName =
    raw.company_name || (isAssigned ? companyId : 'Unassigned')
  return {
    model: raw.model_name,
    companyId,
    companyName,
    groupName: raw.group_name || raw.group || raw.group_id || 'Unassigned',
    isAssigned,
    requests: raw.request_count ?? 0,
    successRate: raw.success_rate,
    firstAttemptRate: raw.first_attempt_success_rate ?? Number.NaN,
    retryRecoveryRate: raw.retry_recovery_rate ?? Number.NaN,
    p95LatencyMs: raw.p95_latency_ms ?? Number.NaN,
    avgLatencyMs: raw.avg_latency_ms,
    p95TtftMs: raw.p95_ttft_ms ?? Number.NaN,
    tps: raw.avg_tps,
    incidents: raw.incidents ?? Number.NaN,
    series: raw.recent_success_series ?? [],
  }
}

function weighted(
  rows: ModelRow[],
  key: keyof Pick<
    ModelRow,
    | 'successRate'
    | 'firstAttemptRate'
    | 'retryRecoveryRate'
    | 'p95LatencyMs'
    | 'p95TtftMs'
  >
) {
  const eligible = rows.filter((row) => Number.isFinite(row[key]))
  const total = eligible.reduce((sum, row) => sum + row.requests, 0)
  if (!total) return Number.NaN
  return (
    eligible.reduce((sum, row) => sum + Number(row[key]) * row.requests, 0) /
    total
  )
}

function statusForRate(rate: number) {
  if (!Number.isFinite(rate)) return { label: 'Unknown', tone: 'unknown' }
  if (rate >= 99.5) return { label: 'Healthy', tone: 'healthy' }
  if (rate >= 99) return { label: 'Watch', tone: 'watch' }
  return { label: 'Degraded', tone: 'degraded' }
}

function HealthStatus(props: { rate: number; t: (key: string) => string }) {
  const status = statusForRate(props.rate)
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] font-medium',
        status.tone === 'healthy' &&
          'border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-300',
        status.tone === 'watch' &&
          'border-amber-200 bg-amber-50 text-amber-700 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-300',
        status.tone === 'degraded' &&
          'border-red-200 bg-red-50 text-red-700 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300',
        status.tone === 'unknown' &&
          'border-border bg-muted text-muted-foreground'
      )}
    >
      <span className='size-1.5 rounded-full bg-current' aria-hidden='true' />
      {props.t(status.label)}
    </span>
  )
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
    <div className='flex items-center justify-between gap-3 border-b py-2.5 last:border-b-0'>
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

function formatDataFreshness(
  value: number | string | undefined,
  locale?: string
): string {
  const timestamp = Number(value)
  if (!Number.isFinite(timestamp) || timestamp <= 0) return '—'
  return new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(new Date(timestamp * 1000))
}

function ReliabilityChart(props: {
  model?: ModelRow
  t: (key: string) => string
}) {
  const values = props.model?.series ?? []
  if (values.length < 2) {
    return (
      <div className='text-muted-foreground flex h-64 items-center justify-center rounded-xl border text-sm'>
        {props.t('No performance data available')}
      </div>
    )
  }
  const width = 720
  const height = 206
  const min = 98
  const max = 100
  const path = values
    .map((point, index) => {
      const x = (index / (values.length - 1)) * width
      const y = height - ((point.success_rate - min) / (max - min)) * height
      return `${index === 0 ? 'M' : 'L'} ${x.toFixed(1)} ${y.toFixed(1)}`
    })
    .join(' ')
  return (
    <div className='space-y-3'>
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <div>
          <p className='text-sm font-semibold'>
            {props.model?.model ?? props.t('All models')}
          </p>
          <p className='text-muted-foreground text-xs'>
            {props.model?.companyName ?? props.t('All companies')} ·{' '}
            {formatUptimePct(props.model?.successRate ?? Number.NaN)}
          </p>
        </div>
        <span className='text-muted-foreground inline-flex items-center gap-1.5 text-xs'>
          <span className='bg-primary size-2 rounded-full' />
          {props.t('Final success rate')}
        </span>
      </div>
      <div className='bg-muted/20 overflow-hidden rounded-xl border px-2 py-3 sm:px-4'>
        <svg
          viewBox='0 0 720 240'
          className='h-56 w-full'
          role='img'
          aria-label={props.t('24-hour success rate curve')}
          preserveAspectRatio='none'
        >
          {[0, 1, 2, 3, 4].map((line) => {
            const y = 16 + line * 47
            return (
              <g key={line}>
                <line
                  x1='0'
                  x2='720'
                  y1={y}
                  y2={y}
                  className='text-border'
                  stroke='currentColor'
                  strokeDasharray='3 7'
                />
                <text
                  x='716'
                  y={y - 4}
                  textAnchor='end'
                  className='fill-muted-foreground font-mono text-[11px]'
                >
                  {(100 - line * 0.5).toFixed(1)}%
                </text>
              </g>
            )
          })}
          <path d={`${path} L 720 206 L 0 206 Z`} className='fill-primary/10' />
          <path
            d={path}
            fill='none'
            stroke='currentColor'
            className='text-primary'
            strokeLinecap='round'
            strokeLinejoin='round'
            strokeWidth='3'
            vectorEffect='non-scaling-stroke'
          />
        </svg>
        <div className='text-muted-foreground mt-1 flex justify-between px-1 font-mono text-[10px]'>
          <span>{props.t('24 hours ago')}</span>
          <span>{props.t('Now')}</span>
        </div>
      </div>
    </div>
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
    setDraftOrder(DEFAULT_COLUMNS)
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

function ConfigDialog(props: {
  open: boolean
  onOpenChange: (open: boolean) => void
  companies: PerformanceHealthCompany[]
  onSaved: () => void
  t: (key: string) => string
}) {
  const [draft, setDraft] = useState<PerformanceHealthCompany[]>([])
  const saveMutation = useMutation({
    mutationFn: () => savePerformanceHealthConfig({ companies: draft }),
    onSuccess: props.onSaved,
  })
  useEffect(() => {
    if (props.open) setDraft(structuredClone(props.companies))
  }, [props.open, props.companies])
  const updateCompany = (
    id: string,
    patch: Partial<PerformanceHealthCompany>
  ) => {
    setDraft((items) =>
      items.map((item) => (item.id === id ? { ...item, ...patch } : item))
    )
  }
  const updateGroup = (
    companyId: string,
    groupId: string,
    patch: Record<string, unknown>
  ) => {
    setDraft((items) =>
      items.map((company) =>
        company.id !== companyId
          ? company
          : {
              ...company,
              groups: (company.groups ?? []).map((group) =>
                group.id === groupId ? { ...group, ...patch } : group
              ),
            }
      )
    )
  }
  const createId = (prefix: string) =>
    `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`
  const addCompany = () =>
    setDraft((items) => [
      ...items,
      {
        id: createId('company'),
        name: props.t('New company'),
        status: 'active',
        slo: 99.9,
        groups: [],
      },
    ])
  const addGroup = (companyId: string) =>
    setDraft((items) =>
      items.map((company) =>
        company.id !== companyId
          ? company
          : {
              ...company,
              groups: [
                ...(company.groups ?? []),
                {
                  id: createId('group'),
                  name: props.t('New model group'),
                  models: [],
                  enabled: true,
                  slo: company.slo ?? 99.9,
                  p95_latency_ms: 0,
                  min_samples: 10,
                },
              ],
            }
      )
    )
  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className='sm:max-w-2xl'>
        <DialogHeader>
          <DialogTitle>{props.t('Company and model groups')}</DialogTitle>
          <DialogDescription>
            {props.t('Configure companies, groups, models and SLO policies')}
          </DialogDescription>
        </DialogHeader>
        {saveMutation.isError && (
          <p className='text-destructive text-sm' role='alert'>
            {props.t('Save failed, please retry')}
          </p>
        )}
        <div className='max-h-[55vh] space-y-3 overflow-y-auto'>
          {draft.length === 0 && (
            <p className='text-muted-foreground text-sm'>
              {props.t('No company configuration available')}
            </p>
          )}
          {draft.map((company) => (
            <div key={company.id} className='space-y-3 rounded-xl border p-3'>
              <div className='grid gap-2 sm:grid-cols-[minmax(0,1fr)_7rem]'>
                <Input
                  value={company.name}
                  aria-label={props.t('Company name')}
                  onChange={(event) =>
                    updateCompany(company.id, { name: event.target.value })
                  }
                />
                <Input
                  type='number'
                  min='0'
                  max='100'
                  step='0.01'
                  value={company.slo ?? ''}
                  aria-label={props.t('SLO')}
                  onChange={(event) =>
                    updateCompany(company.id, {
                      slo: Number(event.target.value),
                    })
                  }
                />
              </div>
              <div className='space-y-2'>
                {(company.groups ?? []).map((group) => (
                  <div
                    key={group.id}
                    className='bg-muted/40 grid gap-2 rounded-lg p-2 sm:grid-cols-[minmax(0,0.8fr)_minmax(0,1.4fr)_6rem]'
                  >
                    <Input
                      value={group.name}
                      aria-label={props.t('Model group')}
                      onChange={(event) =>
                        updateGroup(company.id, group.id, {
                          name: event.target.value,
                        })
                      }
                    />
                    <Input
                      value={group.models.join(', ')}
                      aria-label={props.t('Models')}
                      placeholder='gpt-4.1, claude-3-7-sonnet'
                      onChange={(event) =>
                        updateGroup(company.id, group.id, {
                          models: event.target.value
                            .split(',')
                            .map((model) => model.trim())
                            .filter(Boolean),
                        })
                      }
                    />
                    <Input
                      type='number'
                      min='0'
                      max='100'
                      step='0.01'
                      value={group.slo ?? ''}
                      aria-label={props.t('SLO')}
                      onChange={(event) =>
                        updateGroup(company.id, group.id, {
                          slo: Number(event.target.value),
                        })
                      }
                    />
                  </div>
                ))}
                <Button
                  variant='ghost'
                  size='sm'
                  onClick={() => addGroup(company.id)}
                >
                  {props.t('Add model group')}
                </Button>
              </div>
            </div>
          ))}
        </div>
        <DialogFooter>
          <Button variant='ghost' onClick={addCompany}>
            {props.t('Add company')}
          </Button>
          <Button variant='outline' onClick={() => props.onOpenChange(false)}>
            {props.t('Cancel')}
          </Button>
          <Button
            disabled={saveMutation.isPending}
            onClick={() => saveMutation.mutate()}
          >
            {saveMutation.isPending
              ? props.t('Saving')
              : props.t('Save changes')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function ModelReliabilityDemo() {
  const { t, i18n } = useTranslation()
  const queryClient = useQueryClient()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [selectedCompany, setSelectedCompany] = useState('all')
  const [selectedModel, setSelectedModel] = useState('')
  const [columnPreferences, setColumnPreferences] = useState(
    loadColumnPreferences
  )
  const [columnDialogOpen, setColumnDialogOpen] = useState(false)
  const [configDialogOpen, setConfigDialogOpen] = useState(false)
  const query = useQuery({
    queryKey: ['performance-health-summary', PERFORMANCE_WINDOW_HOURS],
    queryFn: async () =>
      requireServerSuccess(
        await getPerformanceHealthSummary(PERFORMANCE_WINDOW_HOURS)
      ),
    staleTime: 60_000,
    retry: false,
  })
  const configQuery = useQuery({
    queryKey: ['performance-health-companies'],
    queryFn: async () =>
      requireServerSuccess(await getPerformanceHealthCompanies()),
    staleTime: 60_000,
    retry: false,
  })
  const rows = useMemo(
    () => (query.data?.data.models ?? []).map(normalizeModel),
    [query.data]
  )
  const summaryCompanies = query.data?.data.companies
  const configuredCompanies = configQuery.data?.data.companies
  const filteredRows = useMemo(
    () =>
      selectedCompany === 'all'
        ? rows
        : rows.filter((row) => row.companyId === selectedCompany),
    [rows, selectedCompany]
  )
  const activeModel =
    filteredRows.find((row) => row.model === selectedModel) ?? filteredRows[0]
  useEffect(() => {
    if (activeModel && activeModel.model !== selectedModel) {
      setSelectedModel(activeModel.model)
    }
  }, [activeModel, selectedModel])
  const summary = query.data?.data.summary
  const requests = filteredRows.reduce((sum, row) => sum + row.requests, 0)
  const successRate =
    Number.isFinite(summary?.success_rate) && selectedCompany === 'all'
      ? (summary?.success_rate ?? Number.NaN)
      : weighted(filteredRows, 'successRate')
  const firstRate = weighted(filteredRows, 'firstAttemptRate')
  const retryRate = weighted(filteredRows, 'retryRecoveryRate')
  const p95 = weighted(filteredRows, 'p95LatencyMs')
  const p95Ttft = weighted(filteredRows, 'p95TtftMs')
  const visibleColumns = columnPreferences.order.filter((id) =>
    columnPreferences.visible.includes(id)
  )
  const saveColumns = (order: ColumnId[], visible: ColumnId[]) => {
    const next = { order, visible }
    setColumnPreferences(next)
    persistColumnPreferences(order, visible)
  }
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
          slo: Number.NaN,
        })
      }
    }
    return [...merged.values()]
  }, [configuredCompanies, rows, summaryCompanies])
  const configuredModelCount = useMemo(
    () =>
      new Set(
        availableCompanies.flatMap((company) =>
          (company.groups ?? []).flatMap((group) => group.models)
        )
      ).size,
    [availableCompanies]
  )
  const mappedModelCount = rows.filter((row) => row.isAssigned).length
  const degradedRows = rows.filter(
    (row) => Number.isFinite(row.successRate) && row.successRate < 99
  )
  const unassignedRows = rows.filter((row) => !row.isAssigned)
  const hasIncidentTelemetry =
    rows.length > 0 && rows.every((row) => Number.isFinite(row.incidents))
  const incidentCount = hasIncidentTelemetry
    ? rows.reduce((sum, row) => sum + row.incidents, 0)
    : Number.NaN
  const terminalCoverage = query.data?.data.terminal_coverage
  const hasTerminalCoverage =
    query.data?.data.terminal_coverage_available === true &&
    Number.isFinite(terminalCoverage)
  let incidentValue = t('Not collected yet')
  let incidentDetail = t('Attempt-level telemetry is not available yet')
  let incidentTone: 'success' | 'warning' | 'danger' = 'warning'
  if (hasIncidentTelemetry) {
    incidentValue = formatNumber(incidentCount, locale)
    if (incidentCount > 0) {
      incidentDetail = t('{{count}} incidents in the last 24 hours', {
        count: incidentCount,
      })
      incidentTone = 'danger'
    } else {
      incidentDetail = t('No incidents in the last 24 hours')
      incidentTone = 'success'
    }
  }
  const dataFreshness = formatDataFreshness(
    query.data?.data.data_freshness,
    locale
  )
  const modelCoverage = configuredModelCount
    ? Math.min(100, (mappedModelCount / configuredModelCount) * 100)
    : Number.NaN
  const configuredSlo = useMemo(() => {
    const candidates = availableCompanies
      .map((company) => {
        const companyRequests = rows
          .filter((row) => row.companyId === company.id)
          .reduce((sum, row) => sum + row.requests, 0)
        return {
          requests: companyRequests,
          slo: company.slo,
        }
      })
      .filter(
        (item) =>
          item.requests > 0 && Number.isFinite(item.slo) && (item.slo ?? 0) > 0
      )
    if (selectedCompany !== 'all') {
      return (
        availableCompanies.find((company) => company.id === selectedCompany)
          ?.slo ?? Number.NaN
      )
    }
    const totalRequests = candidates.reduce(
      (sum, item) => sum + item.requests,
      0
    )
    if (!totalRequests) return Number.NaN
    return (
      candidates.reduce(
        (sum, item) => sum + (item.slo ?? 0) * item.requests,
        0
      ) / totalRequests
    )
  }, [availableCompanies, rows, selectedCompany])
  const errorBudget = useMemo(() => {
    if (!Number.isFinite(configuredSlo) || !Number.isFinite(successRate)) {
      return Number.NaN
    }
    const allowedErrorRate = 100 - configuredSlo
    if (allowedErrorRate <= 0) return successRate >= 100 ? 100 : 0
    return Math.max(
      0,
      Math.min(
        100,
        ((allowedErrorRate - (100 - successRate)) / allowedErrorRate) * 100
      )
    )
  }, [configuredSlo, successRate])

  if (query.isLoading) {
    return (
      <div className='space-y-4'>
        <Skeleton className='h-28 w-full rounded-2xl' />
        <div className='grid grid-cols-2 gap-2 sm:grid-cols-4'>
          <Skeleton className='h-24 rounded-xl' />
          <Skeleton className='h-24 rounded-xl' />
          <Skeleton className='h-24 rounded-xl' />
          <Skeleton className='h-24 rounded-xl' />
        </div>
        <Skeleton className='h-96 w-full rounded-2xl' />
      </div>
    )
  }
  if (query.isError) {
    return (
      <Card>
        <CardContent className='flex flex-col items-center gap-3 py-16 text-center'>
          <IconBadge tone='destructive' size='lg'>
            <HeartPulse />
          </IconBadge>
          <CardTitle>{t('Unable to load model reliability')}</CardTitle>
          <CardDescription>
            {t(
              'Refresh the page or check that performance monitoring is enabled.'
            )}
          </CardDescription>
          <Button variant='outline' onClick={() => query.refetch()}>
            <RefreshCcw />
            {t('Retry')}
          </Button>
        </CardContent>
      </Card>
    )
  }

  return (
    <div className='space-y-4 sm:space-y-5'>
      <div className='flex flex-col justify-between gap-3 rounded-2xl border bg-[linear-gradient(120deg,color-mix(in_oklch,var(--card)_96%,var(--chart-2)_4%),color-mix(in_oklch,var(--card)_88%,var(--chart-4)_12%))] p-4 shadow-xs sm:flex-row sm:items-end sm:p-5'>
        <div>
          <div className='mb-2 flex items-center gap-2'>
            <IconBadge tone='success' size='sm'>
              <HeartPulse />
            </IconBadge>
            <span className='text-primary rounded-full border px-2 py-0.5 text-[11px] font-medium'>
              {t('Live data')}
            </span>
          </div>
          <h3 className='text-xl font-semibold tracking-tight sm:text-2xl'>
            {t('Model reliability')}
          </h3>
          <p className='text-muted-foreground mt-1 max-w-2xl text-sm'>
            {t(
              'Final request success counts a retry-recovered request once, while upstream attempts remain visible for diagnosis.'
            )}
          </p>
        </div>
        <div className='flex flex-wrap items-end gap-2'>
          <label
            className='text-muted-foreground text-xs'
            htmlFor='company-filter'
          >
            {t('Company')}
          </label>
          <NativeSelect
            id='company-filter'
            value={selectedCompany}
            onChange={(event) => {
              setSelectedCompany(event.target.value)
              setSelectedModel('')
            }}
            size='sm'
          >
            <NativeSelectOption value='all'>
              {t('All companies')}
            </NativeSelectOption>
            {availableCompanies.map((company) => (
              <NativeSelectOption key={company.id} value={company.id}>
                {company.name}
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
            onClick={() => void query.refetch()}
            disabled={query.isFetching}
            aria-label={t('Refresh')}
            title={t('Refresh')}
          >
            <RefreshCcw className={cn(query.isFetching && 'animate-spin')} />
          </Button>
        </div>
      </div>
      <div className='grid grid-cols-2 gap-2 sm:grid-cols-3 sm:gap-3 xl:grid-cols-7'>
        <MetricTile
          label={t('Final request success rate')}
          value={formatUptimePct(successRate)}
          hint={`${formatNumber(requests, locale)} ${t('requests')}`}
          tone='success'
        />
        <MetricTile
          label={t('First-attempt success')}
          value={formatUptimePct(firstRate)}
          hint={
            Number.isFinite(firstRate)
              ? t('Direct first route success')
              : t('Not collected yet')
          }
          tone='success'
        />
        <MetricTile
          label={t('Retry recovery')}
          value={formatUptimePct(retryRate)}
          hint={
            Number.isFinite(retryRate)
              ? t('Recovered after first failure')
              : t('Not collected yet')
          }
          tone='warning'
        />
        <MetricTile
          label={t('P95 latency')}
          value={formatLatency(p95)}
          hint={
            Number.isFinite(p95)
              ? t('Includes retry wait')
              : t('Not collected yet')
          }
          tone='warning'
        />
        <MetricTile
          label={t('P95 TTFT')}
          value={formatLatency(p95Ttft)}
          hint={t('Time to first token')}
          tone='warning'
        />
        <MetricTile
          label={t('Error budget')}
          value={formatUptimePct(errorBudget)}
          hint={
            Number.isFinite(configuredSlo)
              ? `${t('SLO')}: ${formatUptimePct(configuredSlo)}`
              : t('Not available')
          }
          tone={
            !Number.isFinite(errorBudget) || errorBudget >= 50
              ? 'info'
              : 'warning'
          }
        />
        <MetricTile
          label={t('Terminal coverage')}
          value={
            hasTerminalCoverage
              ? formatUptimePct(terminalCoverage ?? Number.NaN)
              : t('Not collected yet')
          }
          hint={
            hasTerminalCoverage
              ? t('Data quality')
              : t('Attempt-level telemetry is not available yet')
          }
          tone='info'
        />
      </div>
      <div className='grid gap-4 lg:grid-cols-2'>
        <Card>
          <CardHeader className='border-b pb-3'>
            <CardTitle className='flex items-center gap-2 text-base'>
              <IconBadge tone='info' size='sm'>
                <DatabaseZap />
              </IconBadge>
              {t('Data quality')}
            </CardTitle>
            <CardDescription>
              {t('Performance metrics for the last 24 hours')}
            </CardDescription>
          </CardHeader>
          <CardContent className='pt-2'>
            <SignalLine
              label={t('Terminal coverage')}
              value={
                hasTerminalCoverage
                  ? formatUptimePct(terminalCoverage ?? Number.NaN)
                  : t('Not collected yet')
              }
              detail={
                hasTerminalCoverage
                  ? t('All requests have a terminal outcome')
                  : t('Attempt-level telemetry is not available yet')
              }
              tone={
                hasTerminalCoverage && (terminalCoverage ?? 0) >= 99
                  ? 'success'
                  : 'warning'
              }
            />
            <SignalLine
              label={t('Models')}
              value={`${formatNumber(mappedModelCount, locale)} / ${formatNumber(configuredModelCount, locale)}`}
              detail={t('Configured models with observed traffic')}
              tone={
                Number.isFinite(modelCoverage) && modelCoverage >= 99
                  ? 'success'
                  : 'warning'
              }
            />
            <SignalLine
              label={t('Last updated')}
              value={dataFreshness}
              detail={t('Data freshness')}
              tone={dataFreshness === '—' ? 'warning' : 'success'}
            />
          </CardContent>
        </Card>
        <Card>
          <CardHeader className='border-b pb-3'>
            <CardTitle className='flex items-center gap-2 text-base'>
              <IconBadge
                tone={
                  degradedRows.length || unassignedRows.length
                    ? 'warning'
                    : 'success'
                }
                size='sm'
              >
                {degradedRows.length || unassignedRows.length ? (
                  <AlertTriangle />
                ) : (
                  <Activity />
                )}
              </IconBadge>
              {t('Routing watchpoints')}
            </CardTitle>
            <CardDescription>
              {t(
                'Watch short-window failures, latency drift, capacity saturation and traffic concentration.'
              )}
            </CardDescription>
          </CardHeader>
          <CardContent className='pt-2'>
            <SignalLine
              label={t('Degraded performance recently')}
              value={formatNumber(degradedRows.length, locale)}
              detail={t('Models below the 99% success watch threshold')}
              tone={degradedRows.length ? 'danger' : 'success'}
            />
            <SignalLine
              label={t('Unassigned')}
              value={formatNumber(unassignedRows.length, locale)}
              detail={t(
                'Models that are being used but not configured in the system'
              )}
              tone={unassignedRows.length ? 'warning' : 'success'}
            />
            <SignalLine
              label={t('incidents')}
              value={incidentValue}
              detail={incidentDetail}
              tone={incidentTone}
            />
          </CardContent>
        </Card>
      </div>
      <div className='grid gap-4 lg:grid-cols-3'>
        <DiagnosticCard
          title={t('Error breakdown')}
          description={t('Attempt-level telemetry is not available yet')}
          icon={<TriangleAlert className='size-4' />}
          rows={[
            { label: t('429 rate'), value: t('Not collected yet') },
            { label: t('5xx rate'), value: t('Not collected yet') },
            { label: t('Timeout rate'), value: t('Not collected yet') },
          ]}
        />
        <DiagnosticCard
          title={t('Routing resilience')}
          description={t('Retries, fallback and circuit breaker telemetry')}
          icon={<RotateCcw className='size-4' />}
          rows={[
            { label: t('Retry request rate'), value: t('Not collected yet') },
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
          description={t('Percentiles require mergeable latency histograms')}
          icon={<Gauge className='size-4' />}
          rows={[
            { label: t('P50 latency'), value: t('Not collected yet') },
            { label: t('P95 latency'), value: formatLatency(p95) },
            { label: t('P99 latency'), value: t('Not collected yet') },
          ]}
        />
      </div>
      <div className='grid gap-4 xl:grid-cols-[minmax(0,1.35fr)_minmax(18rem,0.65fr)]'>
        <Card>
          <CardHeader className='border-b'>
            <CardTitle className='flex items-center gap-2'>
              <IconBadge tone='success' size='sm'>
                <ShieldCheck />
              </IconBadge>
              {t('24-hour success rate curve')}
            </CardTitle>
            <CardDescription>
              {activeModel
                ? `${activeModel.model} · ${activeModel.companyName}`
                : t('Select a model to inspect its trend')}
            </CardDescription>
          </CardHeader>
          <CardContent className='pt-4'>
            <ReliabilityChart model={activeModel} t={t} />
          </CardContent>
        </Card>
        <Card>
          <CardHeader className='border-b'>
            <CardTitle className='flex items-center gap-2'>
              <IconBadge tone='info' size='sm'>
                <Settings2 />
              </IconBadge>
              {t('Company model groups')}
            </CardTitle>
            <CardDescription>
              {t('Health grouped by configured company')}
            </CardDescription>
          </CardHeader>
          <CardContent className='space-y-2 pt-4'>
            {availableCompanies.map((company) => {
              const companyRows = rows.filter(
                (row) => row.companyId === company.id
              )
              const rate = weighted(companyRows, 'successRate')
              return (
                <button
                  key={company.id}
                  type='button'
                  onClick={() => {
                    setSelectedCompany(company.id)
                    setSelectedModel('')
                  }}
                  className={cn(
                    'hover:bg-muted/60 flex w-full items-center justify-between gap-3 rounded-xl border p-3 text-left transition-colors',
                    selectedCompany === company.id &&
                      'border-primary/40 bg-primary/5'
                  )}
                  aria-pressed={selectedCompany === company.id}
                >
                  <span className='min-w-0'>
                    <span className='block font-medium'>{company.name}</span>
                    <span className='text-muted-foreground block truncate text-xs'>
                      {company.groups?.map((group) => group.name).join(' · ') ||
                        `${companyRows.length} ${t('models')}`}
                    </span>
                  </span>
                  <span className='shrink-0 text-right'>
                    <span
                      className={cn(
                        'block font-mono text-sm font-semibold',
                        getSuccessRateTextClass(rate)
                      )}
                    >
                      {formatUptimePct(rate)}
                    </span>
                    <span className='text-muted-foreground text-[10px]'>
                      {formatNumber(
                        companyRows.reduce((sum, row) => sum + row.requests, 0),
                        locale
                      )}{' '}
                      {t('requests')}
                    </span>
                  </span>
                </button>
              )
            })}
          </CardContent>
        </Card>
      </div>
      <Card>
        <CardHeader className='border-b'>
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
        <CardContent className='pt-2'>
          <div className='overflow-x-auto'>
            <table className='w-full min-w-[780px] text-left text-sm'>
              <thead className='text-muted-foreground border-b text-xs'>
                <tr>
                  {visibleColumns.map((id) => {
                    const column = ALL_COLUMNS.find((item) => item.id === id)
                    return (
                      <th key={id} className='px-2 py-3 font-medium'>
                        {column ? t(column.label) : id}
                      </th>
                    )
                  })}
                </tr>
              </thead>
              <tbody>
                {filteredRows.length === 0 ? (
                  <tr>
                    <td
                      colSpan={visibleColumns.length}
                      className='text-muted-foreground px-2 py-12 text-center'
                    >
                      {t('No performance data available')}
                    </td>
                  </tr>
                ) : (
                  filteredRows.map((row) => (
                    <tr
                      key={row.model}
                      className={cn(
                        'border-b last:border-0',
                        row.model === activeModel?.model && 'bg-primary/5'
                      )}
                      onClick={() => setSelectedModel(row.model)}
                    >
                      {visibleColumns.map((id) => (
                        <td
                          key={id}
                          className={cn(
                            'px-2 py-3',
                            [
                              'model',
                              'requests',
                              'success',
                              'first',
                              'retry',
                              'p95',
                              'avg',
                              'ttft',
                              'tps',
                              'incidents',
                            ].includes(id) && 'font-mono tabular-nums'
                          )}
                        >
                          {id === 'model' && (
                            <span className='font-semibold'>{row.model}</span>
                          )}
                          {id === 'company' && (
                            <span>
                              <span className='block'>{row.companyName}</span>
                              <span className='text-muted-foreground text-xs'>
                                {row.groupName}
                              </span>
                            </span>
                          )}
                          {id === 'requests' &&
                            formatNumber(row.requests, locale)}
                          {id === 'success' && (
                            <span
                              className={cn(
                                'font-semibold',
                                getSuccessRateTextClass(row.successRate)
                              )}
                            >
                              {formatUptimePct(row.successRate)}
                            </span>
                          )}
                          {id === 'first' &&
                            formatUptimePct(row.firstAttemptRate)}
                          {id === 'retry' &&
                            formatUptimePct(row.retryRecoveryRate)}
                          {id === 'p95' && formatLatency(row.p95LatencyMs)}
                          {id === 'avg' && formatLatency(row.avgLatencyMs)}
                          {id === 'ttft' && formatLatency(row.p95TtftMs)}
                          {id === 'tps' && formatThroughput(row.tps)}
                          {id === 'incidents' && (row.incidents || '—')}
                          {id === 'status' && (
                            <HealthStatus rate={row.successRate} t={t} />
                          )}
                        </td>
                      ))}
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
        </CardContent>
      </Card>
      <ColumnSettings
        open={columnDialogOpen}
        onOpenChange={setColumnDialogOpen}
        order={columnPreferences.order}
        visible={columnPreferences.visible}
        onSave={saveColumns}
        t={t}
      />
      <ConfigDialog
        open={configDialogOpen}
        onOpenChange={setConfigDialogOpen}
        companies={configQuery.data?.data.companies ?? []}
        onSaved={() => {
          setConfigDialogOpen(false)
          void queryClient.invalidateQueries({
            queryKey: ['performance-health-summary'],
          })
          void queryClient.invalidateQueries({
            queryKey: ['performance-health-companies'],
          })
        }}
        t={t}
      />
    </div>
  )
}
