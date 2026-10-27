/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import type { ColumnDef, ColumnFiltersState } from '@tanstack/react-table'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import {
  DataTableColumnHeader,
  DataTablePagination,
  DataTableToolbar,
  DataTableView,
  useDataTable,
} from '@/components/data-table'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'
import { cn } from '@/lib/utils'

import { formatLatency, formatThroughput, formatUptimePct } from '../lib/format'
import {
  getPerformanceModelStatus,
  type PerformanceColumnId,
  type PerformanceModelRow,
} from '../lib/model-status'

const numericFields = {
  requests: 'requests',
  success: 'successRate',
  first: 'firstAttemptRate',
  retry: 'retryRecoveryRate',
  p95: 'p95LatencyMs',
  avg: 'avgLatencyMs',
  ttft: 'p95TtftMs',
  tps: 'tps',
  incidents: 'incidents',
} as const
const columnIds: PerformanceColumnId[] = [
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
  'incidents',
  'status',
]

type PerformanceModelTableProps = {
  rows: PerformanceModelRow[]
  columns: Array<{ id: PerformanceColumnId; label: string }>
  selectedModel?: string
  onSelectModel: (name: string) => void
  locale: string
}

export function PerformanceModelTable({
  rows,
  columns,
  selectedModel,
  onSelectModel,
  locale,
}: PerformanceModelTableProps) {
  const { t } = useTranslation()
  const [globalFilter, setGlobalFilter] = useState('')
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([])
  const numberLocale = toIntlLocale(locale)
  const columnVisibility = useMemo(() => {
    const visible = new Set(columns.map((column) => column.id))
    return Object.fromEntries(columnIds.map((id) => [id, visible.has(id)]))
  }, [columns])
  const definitions = useMemo<ColumnDef<PerformanceModelRow, unknown>[]>(() => {
    const labels = new Map(columns.map((column) => [column.id, column.label]))
    // Keep hidden numeric columns available so request sorting survives field changes.
    const orderedIds = [
      ...new Set([...columns.map((column) => column.id), ...columnIds]),
    ]
    return orderedIds.map((id) => ({
      id,
      enableHiding: false,
      enableGlobalFilter: true,
      sortUndefined: 'last',
      accessorFn: (row) => {
        if (id === 'model') return row.model
        if (id === 'company') return `${row.companyName} ${row.groupName}`
        if (id === 'status') return getPerformanceModelStatus(row).label
        const value = row[numericFields[id]]
        return Number.isFinite(value) ? value : undefined
      },
      header: ({ column }) => (
        <DataTableColumnHeader
          column={column}
          title={t(labels.get(id) ?? id)}
        />
      ),
      cell: ({ row }) => {
        const model = row.original
        if (id === 'model') {
          return (
            <div className='flex min-w-0 items-center gap-1'>
              <Button
                type='button'
                variant='link'
                className='text-foreground h-auto max-w-64 min-w-0 justify-start p-0 font-mono text-xs'
                title={model.model}
                aria-pressed={selectedModel === model.model}
                onClick={() => onSelectModel(model.model)}
              >
                <span className='truncate'>{model.model}</span>
              </Button>
              <CopyButton value={model.model} className='size-8' />
            </div>
          )
        }
        if (id === 'company') {
          return (
            <div className='max-w-48 space-y-1'>
              <div className='truncate' title={model.companyName}>
                {model.isAssigned ? model.companyName : t('Unassigned')}
              </div>
              <div
                className='text-muted-foreground truncate text-xs'
                title={model.groupName}
              >
                {model.groupName === 'Unassigned'
                  ? t('Unassigned')
                  : model.groupName}
              </div>
            </div>
          )
        }
        if (id === 'status') {
          const status = getPerformanceModelStatus(model)
          return (
            <span
              className={cn('whitespace-nowrap', {
                'text-success': status.tone === 'healthy',
                'text-destructive': status.tone === 'degraded',
                'text-muted-foreground': status.tone === 'unknown',
              })}
            >
              {t(status.label)}
            </span>
          )
        }
        const value = model[numericFields[id]]
        let text = '—'
        if (Number.isFinite(value)) {
          if (id === 'success' || id === 'first' || id === 'retry') {
            text = formatUptimePct(value)
          } else if (id === 'p95' || id === 'avg' || id === 'ttft') {
            text = formatLatency(value)
          } else if (id === 'tps') {
            text = formatThroughput(value, true)
          } else {
            text = formatNumber(value, numberLocale, {
              maximumFractionDigits: 0,
            })
          }
        }
        return <span className='whitespace-nowrap tabular-nums'>{text}</span>
      },
    }))
  }, [columns, numberLocale, onSelectModel, selectedModel, t])
  const { table } = useDataTable({
    data: rows,
    columns: definitions,
    getRowId: (row) => row.model,
    initialSorting: [{ id: 'requests', desc: true }],
    columnVisibility,
    columnFilters,
    onColumnFiltersChange: setColumnFilters,
    globalFilter,
    onGlobalFilterChange: setGlobalFilter,
    globalFilterFn: (row, _columnId, value: string) => {
      const query = value.trim().toLocaleLowerCase()
      return [
        row.original.model,
        row.original.companyName,
        row.original.groupName,
      ].some((text) => text.toLocaleLowerCase().includes(query))
    },
  })

  return (
    <div className='min-w-0 space-y-4'>
      <DataTableToolbar
        table={table}
        searchPlaceholder={t('Search models')}
        hideViewOptions
      />
      <DataTableView
        table={table}
        emptyTitle={
          globalFilter.trim()
            ? t('No models found')
            : t('No channel models or request data')
        }
        emptyDescription={
          globalFilter.trim()
            ? t('No records found. Try adjusting your filters.')
            : t(
                'Configure a channel or show all preset models to review model ownership.'
              )
        }
        getRowClassName={(row) =>
          row.original.model === selectedModel ? 'bg-accent/50' : ''
        }
      />
      <DataTablePagination table={table} compact />
    </div>
  )
}
