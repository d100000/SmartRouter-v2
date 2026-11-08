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
import { useQuery } from '@tanstack/react-query'
import { lazy, Suspense, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Card, CardContent } from '@/components/ui/card'
import { toIntlLocale } from '@/i18n/languages'
import { formatFixedUSD } from '@/lib/currency'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { getSupplyOverview, supplyQueryKey } from '../api'
import type { SupplierMode, SupplyLogFilters, SupplyOverview } from '../types'
import { SupplyAsyncMetricsPanel } from './async-metrics'
import { SupplyFilters } from './supply-filters'

const SupplyTrendChart = lazy(() => import('./supply-trend'))

export function SupplyOverviewPanel(props: { mode: SupplierMode }) {
  const userId = useAuthStore((s) => s.auth.user?.id)
  const [filters, setFilters] = useState<SupplyLogFilters>({ days: 7 })
  const query = useQuery({
    queryKey: [...supplyQueryKey, userId, props.mode, 'overview', filters],
    queryFn: ({ signal }) => getSupplyOverview(props.mode, filters, signal),
    meta: { errorRedirect: false },
  })
  return (
    <div className='space-y-4'>
      <SupplyFilters onChange={setFilters} />
      {query.isPending && <LoadingState />}
      {query.isError && <ErrorState onRetry={() => void query.refetch()} />}
      {query.isSuccess && <SupplyOverviewData overview={query.data} />}
    </div>
  )
}

function SupplyOverviewData(props: { overview: SupplyOverview }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const overview = props.overview
  const date = new Intl.DateTimeFormat(locale, {
    dateStyle: 'short',
    timeStyle: 'short',
    timeZone: overview.coverage?.timezone ?? 'UTC',
  })
  const metrics = [
    {
      label: t('Logical requests reached'),
      value: formatNumber(overview.logical_request_count, locale),
    },
    {
      label: t('Last upstream call'),
      value: overview.last_called_at
        ? date.format(new Date(overview.last_called_at * 1000))
        : t('No traffic yet'),
    },
    {
      label: t('Recorded images'),
      value:
        overview.image_count == null
          ? t('Unknown')
          : formatNumber(overview.image_count, locale),
      description: t('Recorded samples: {{count}}', {
        count: overview.image_usage_known_count ?? 0,
      }),
    },
    {
      label: t('Audio duration (seconds)'),
      value:
        overview.audio_seconds == null
          ? t('Unknown')
          : formatNumber(overview.audio_seconds, locale),
      description: t('Recorded samples: {{count}}', {
        count: overview.audio_usage_known_count ?? 0,
      }),
    },
    {
      label: t('Video duration (seconds)'),
      value:
        overview.video_seconds == null
          ? t('Unknown')
          : formatNumber(overview.video_seconds, locale),
      description: t('Recorded samples: {{count}}', {
        count: overview.video_usage_known_count ?? 0,
      }),
    },
    {
      label: t('Upstream usage credits'),
      value: overview.credits ?? t('Unknown'),
      description: t('Recorded samples: {{count}}', {
        count: overview.credit_usage_known_count ?? 0,
      }),
    },
    {
      label: t('Realtime generations'),
      value: formatNumber(overview.realtime_generation_count, locale),
    },
    {
      label: t('Channels'),
      value: formatNumber(overview.channel_count, locale),
    },
    {
      label: t('Available channels'),
      value: formatNumber(overview.available_channels, locale),
    },
    {
      label: t('Upstream attempts'),
      value: formatNumber(overview.request_count, locale),
    },
    {
      label: t('Successful synchronous attempts'),
      value: formatNumber(overview.success_count, locale),
    },
    {
      label: t('Failed synchronous attempts'),
      value: formatNumber(overview.failure_count, locale),
    },
    {
      label: t('Unknown synchronous results'),
      value: formatNumber(overview.unknown_count, locale),
    },
    {
      label: t('Input Tokens'),
      value:
        overview.usage_known_count === 0
          ? t('Unknown')
          : formatNumber(overview.input_tokens, locale),
    },
    {
      label: t('Output Tokens'),
      value:
        overview.usage_known_count === 0
          ? t('Unknown')
          : formatNumber(overview.output_tokens, locale),
    },
    {
      label: t('Average synchronous latency (ms)'),
      value: formatNumber(overview.avg_latency_ms, locale),
    },
    {
      label: t('Cache Read Tokens'),
      value: formatNumber(overview.cache_read_tokens, locale),
    },
    {
      label: t('Cache Write Tokens'),
      value: formatNumber(overview.cache_write_tokens, locale),
    },
    {
      label: t('Average first token latency (ms)'),
      value: formatNumber(overview.avg_first_token_ms, locale),
    },
    {
      label: t('Daily procurement cost'),
      value:
        overview.daily_used_usd == null
          ? t('Unknown')
          : `${formatFixedUSD(overview.daily_used_usd, { locale, digitsSmall: 6, digitsLarge: 6 })} USD`,
    },
  ]
  return (
    <div className='space-y-4'>
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <p className='text-muted-foreground text-sm'>
          {t('Budget date (Beijing time)')}: {overview.budget_day || '—'}
        </p>
        <p className='text-muted-foreground text-xs'>
          {t('Updated at')}:{' '}
          {overview.updated_at
            ? date.format(new Date(overview.updated_at * 1000))
            : t('Unknown')}{' '}
          · {t('Statistics timezone')}: {overview.coverage?.timezone ?? 'UTC'}
        </p>
      </div>
      <p className='text-muted-foreground text-xs'>
        {t('Usage coverage')}:{' '}
        {overview.usage_known_count == null
          ? t('Unknown')
          : `${formatNumber(overview.usage_known_count, locale)} / ${formatNumber(overview.request_count, locale)}`}{' '}
        · {t('Collected since')}:{' '}
        {overview.coverage?.collected_since
          ? date.format(new Date(overview.coverage.collected_since * 1000))
          : t('Unknown')}
      </p>
      {overview.coverage?.limitations.length ? (
        <p role='status' className='text-muted-foreground text-sm'>
          {t(
            'Some metrics are not collected. Unknown usage is not counted as zero.'
          )}{' '}
          {overview.coverage.limitations.includes('image_video_quantities') &&
            t('Image and video quantities are not collected.')}{' '}
          {overview.coverage.limitations.includes('video_quantities') &&
            t('Video quantities are not collected.')}{' '}
          {overview.coverage.limitations.includes('async_completion') &&
            t('Asynchronous completion is not collected.')}
        </p>
      ) : null}
      <div
        role='list'
        aria-label={t('Overview')}
        className='grid grid-cols-2 gap-3 sm:grid-cols-3'
      >
        {metrics.map((metric) => (
          <Card key={metric.label} role='listitem' className='min-w-0 py-0'>
            <CardContent className='p-3 sm:p-4'>
              <p className='text-muted-foreground text-sm'>{metric.label}</p>
              <p className='mt-1 text-xl font-semibold break-words tabular-nums'>
                {metric.value}
              </p>
              {metric.description && (
                <p className='text-muted-foreground mt-1 text-xs'>
                  {metric.description}
                </p>
              )}
            </CardContent>
          </Card>
        ))}
      </div>
      {overview.async && <SupplyAsyncMetricsPanel metrics={overview.async} />}
      <section className='space-y-2'>
        <h3 className='text-sm font-semibold'>{t('HTTP error categories')}</h3>
        <dl className='grid grid-cols-2 gap-3 text-sm sm:grid-cols-3'>
          {[
            {
              label: t('Timeouts (HTTP 408/504)'),
              value: overview.timeout_count,
            },
            {
              label: t('Rate limited (HTTP 429)'),
              value: overview.rate_limited_count,
            },
            {
              label: t('Upstream errors (HTTP 5xx)'),
              value: overview.upstream_error_count,
            },
            {
              label: t('Upstream authentication errors (HTTP 401/403)'),
              value: overview.auth_error_count,
            },
            {
              label: t('Request errors (HTTP 4xx)'),
              value: overview.request_error_count,
            },
            {
              label: t('Unclassified errors'),
              value: overview.unknown_error_count,
            },
          ].map((metric) => (
            <div key={metric.label}>
              <dt className='text-muted-foreground'>{metric.label}</dt>
              <dd className='font-medium tabular-nums'>
                {formatNumber(metric.value, locale)}
              </dd>
            </div>
          ))}
        </dl>
      </section>
      {overview.trend && overview.trend.length > 0 && (
        <Suspense fallback={<LoadingState />}>
          <SupplyTrendChart
            data={overview.trend}
            timezone={overview.coverage?.timezone ?? 'UTC'}
          />
        </Suspense>
      )}
      <h3 className='text-sm font-semibold'>{t('Channel stability')}</h3>
      <StaticDataTable
        data={overview.channels ?? []}
        getRowKey={(row) => row.channel_id}
        emptyContent={t('No traffic yet')}
        columns={[
          { id: 'name', header: t('Channel'), cell: (row) => row.channel_name },
          {
            id: 'requests',
            header: t('Upstream attempts'),
            cell: (row) => formatNumber(row.request_count, locale),
          },
          {
            id: 'logical',
            header: t('Logical requests reached'),
            cell: (row) => formatNumber(row.logical_request_count, locale),
          },
          {
            id: 'last_call',
            header: t('Last upstream call'),
            cell: (row) =>
              row.last_called_at
                ? date.format(new Date(row.last_called_at * 1000))
                : t('No traffic yet'),
          },
          {
            id: 'success',
            header: t('Synchronous success rate'),
            cell: (row) =>
              row.success_count + row.failure_count > 0
                ? `${formatNumber((row.success_count / (row.success_count + row.failure_count)) * 100, locale)}%`
                : t('Insufficient samples'),
          },
          {
            id: 'unknown',
            header: t('Unknown synchronous results'),
            cell: (row) => formatNumber(row.unknown_count, locale),
          },
          {
            id: 'latency',
            header: t('Average synchronous latency (ms)'),
            cell: (row) => formatNumber(row.avg_latency_ms, locale),
          },
        ]}
      />
    </div>
  )
}
