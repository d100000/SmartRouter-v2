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
import { useMutation, useQuery } from '@tanstack/react-query'
import type { ColumnDef } from '@tanstack/react-table'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { DataTablePage, useDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import { downloadCsv } from '@/features/usage-logs/lib/export'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber, formatTimestampToDate } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { exportSupplyLogs, getSupplyLogs, supplyQueryKey } from '../api'
import type { SupplyLog, SupplyLogFilters } from '../types'
import { SupplyFilters } from './supply-filters'
import { SupplyTaskStatus } from './task-status'

export function SupplyLogs() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const userId = useAuthStore((s) => s.auth.user?.id)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const [filters, setFilters] = useState<SupplyLogFilters>({ days: 7 })
  const query = useQuery({
    meta: { errorRedirect: false },
    queryKey: [...supplyQueryKey, userId, 'logs', pagination, filters],
    queryFn: ({ signal }) =>
      getSupplyLogs(
        {
          p: pagination.pageIndex + 1,
          page_size: pagination.pageSize,
          ...filters,
        },
        signal
      ),
  })
  const exporting = useMutation({
    mutationFn: () => exportSupplyLogs(filters),
    onSuccess: (csv) => downloadCsv('supplier-logs.csv', [csv]),
  })
  const columns = useMemo<ColumnDef<SupplyLog>[]>(
    () => [
      {
        accessorKey: 'created_at',
        header: t('Time'),
        cell: ({ row }) => formatTimestampToDate(row.original.created_at),
      },
      { accessorKey: 'channel_name', header: t('Channel') },
      { accessorKey: 'model', header: t('Model') },
      {
        accessorKey: 'stage',
        header: t('Stage'),
        cell: ({ row }) =>
          ({
            test: t('Test'),
            task_submit: t('Task submission'),
            realtime_generation: t('Realtime generation'),
            realtime_transcription: t('Realtime transcription'),
            embedding: t('Embedding'),
            rerank: t('Rerank'),
            image: t('Image'),
            audio: t('Audio'),
            polling: t('Task polling'),
            inference: t('Inference'),
            unknown: t('Unknown'),
          })[row.original.stage] ?? t('Unknown'),
      },
      {
        accessorKey: 'result',
        header: t('Result'),
        cell: ({ row }) => {
          if (row.original.stage === 'task_submit') {
            return (
              {
                success: t('Submission accepted'),
                failure: t('Submission rejected'),
              }[row.original.result] ?? t('Unknown submission result')
            )
          }
          return (
            { success: t('Success'), failure: t('Failed') }[
              row.original.result
            ] ?? t('Unknown')
          )
        },
      },
      {
        id: 'task_status',
        header: t('Task status'),
        cell: ({ row }) => <SupplyTaskStatus log={row.original} />,
      },
      {
        accessorKey: 'status_code',
        header: t('Status Code'),
        cell: ({ row }) =>
          row.original.status_code > 0 ? row.original.status_code : '—',
      },
      {
        accessorKey: 'error_category',
        header: t('Error category'),
        cell: ({ row }) =>
          ({
            none: t('None'),
            timeout: t('Timeouts (HTTP 408/504)'),
            rate_limited: t('Rate limited (HTTP 429)'),
            upstream_error: t('Upstream errors (HTTP 5xx)'),
            auth_error: t('Upstream authentication errors (HTTP 401/403)'),
            request_error: t('Request errors (HTTP 4xx)'),
            unknown: t('Unclassified errors'),
          })[row.original.error_category ?? 'unknown'] ?? t('Unknown'),
      },
      {
        accessorKey: 'image_count',
        header: t('Recorded images'),
        cell: ({ row }) =>
          row.original.image_count == null
            ? t('Unknown')
            : formatNumber(row.original.image_count, locale),
      },
      {
        accessorKey: 'image_usage_source',
        header: t('Image quantity source'),
        cell: ({ row }) => mediaSourceLabel(row.original.image_usage_source, t),
      },
      {
        accessorKey: 'audio_seconds',
        header: t('Audio duration (seconds)'),
        cell: ({ row }) =>
          row.original.audio_seconds == null
            ? t('Unknown')
            : formatNumber(row.original.audio_seconds, locale),
      },
      {
        accessorKey: 'audio_usage_source',
        header: t('Audio quantity source'),
        cell: ({ row }) => mediaSourceLabel(row.original.audio_usage_source, t),
      },
      {
        accessorKey: 'video_seconds',
        header: t('Video duration (seconds)'),
        cell: ({ row }) =>
          row.original.video_seconds == null
            ? t('Unknown')
            : formatNumber(row.original.video_seconds, locale),
      },
      {
        accessorKey: 'video_usage_source',
        header: t('Video quantity source'),
        cell: ({ row }) => mediaSourceLabel(row.original.video_usage_source, t),
      },
      {
        accessorKey: 'credits',
        header: t('Upstream usage credits'),
        // Credits are decimal quantities, not currency or text tokens. Preserve precision.
        cell: ({ row }) => row.original.credits ?? t('Unknown'),
      },
      {
        accessorKey: 'credit_usage_source',
        header: t('Credits quantity source'),
        cell: ({ row }) =>
          mediaSourceLabel(row.original.credit_usage_source, t),
      },
      {
        accessorKey: 'is_stream',
        header: t('Stream'),
        cell: ({ row }) => (row.original.is_stream ? t('Yes') : t('No')),
      },
      {
        accessorKey: 'first_token_ms',
        header: t('First token latency (ms)'),
        cell: ({ row }) => formatNumber(row.original.first_token_ms, locale),
      },
      {
        accessorKey: 'cache_read_tokens',
        header: t('Cache Read Tokens'),
        cell: ({ row }) => formatNumber(row.original.cache_read_tokens, locale),
      },
      {
        accessorKey: 'cache_write_tokens',
        header: t('Cache Write Tokens'),
        cell: ({ row }) =>
          formatNumber(row.original.cache_write_tokens, locale),
      },
      {
        accessorKey: 'duration_ms',
        header: t('Duration (ms)'),
        cell: ({ row }) => formatNumber(row.original.duration_ms, locale),
      },
      {
        accessorKey: 'input_tokens',
        header: t('Input Tokens'),
        cell: ({ row }) => formatNumber(row.original.input_tokens, locale),
      },
      {
        accessorKey: 'output_tokens',
        header: t('Output Tokens'),
        cell: ({ row }) => formatNumber(row.original.output_tokens, locale),
      },
      {
        accessorKey: 'usage_source',
        header: t('Usage source'),
        cell: ({ row }) =>
          ({
            actual: t('Actual'),
            upstream: t('Actual'),
            upstream_realtime: t('Actual'),
            estimated: t('Estimated'),
            local_estimate: t('Estimated'),
          })[row.original.usage_source] ?? t('Unknown'),
      },
      {
        accessorKey: 'correlation_id',
        header: t('Reference'),
        cell: ({ row }) => (
          <dl className='max-w-72 space-y-1 text-xs'>
            <div>
              <dt className='text-muted-foreground'>{t('Reference')}</dt>
              <dd className='flex items-center gap-1'>
                <span className='min-w-0 break-all'>
                  {row.original.correlation_id || '—'}
                </span>
                {row.original.correlation_id && (
                  <CopyButton
                    value={row.original.correlation_id}
                    className='size-6'
                    iconClassName='size-3'
                    aria-label={t('Copy {{field}}', { field: t('Reference') })}
                  />
                )}
              </dd>
            </div>
            <div>
              <dt className='text-muted-foreground'>
                {t('Upstream request ID')}
              </dt>
              <dd className='flex items-center gap-1'>
                <span className='min-w-0 break-all'>
                  {row.original.upstream_request_id || '—'}
                </span>
                {row.original.upstream_request_id && (
                  <CopyButton
                    value={row.original.upstream_request_id}
                    className='size-6'
                    iconClassName='size-3'
                    aria-label={t('Copy {{field}}', {
                      field: t('Upstream request ID'),
                    })}
                  />
                )}
              </dd>
            </div>
          </dl>
        ),
      },
    ],
    [locale, t]
  )
  const { table } = useDataTable({
    data: query.data?.items ?? [],
    columns,
    pagination,
    onPaginationChange: setPagination,
    manualPagination: true,
    totalCount: query.data?.total ?? 0,
    enableRowSelection: false,
    columnVisibilityStorageKey: false,
    columnSizingStorageKey: false,
  })
  return (
    <div className='space-y-4'>
      <SupplyFilters
        logs
        onChange={(next) => {
          setFilters(next)
          setPagination((p) => ({ ...p, pageIndex: 0 }))
        }}
      >
        <Button
          type='button'
          variant='outline'
          disabled={exporting.isPending}
          onClick={() => exporting.mutate()}
        >
          {exporting.isPending ? t('Exporting...') : t('Export CSV')}
        </Button>
      </SupplyFilters>
      {query.isError ? (
        <ErrorState onRetry={() => void query.refetch()} />
      ) : (
        <DataTablePage
          table={table}
          columns={columns}
          isLoading={query.isPending}
          isFetching={query.isFetching}
          fixedHeight={false}
          emptyTitle={t('No supply logs')}
        />
      )}
    </div>
  )
}

function mediaSourceLabel(
  source: string | undefined,
  t: (key: string) => string
): string {
  switch (source) {
    case 'upstream_payload':
      return t('Upstream response')
    case 'upstream_usage':
      return t('Upstream usage')
    case 'media_metadata':
      return t('Media metadata')
    default:
      return t('Unknown')
  }
}
