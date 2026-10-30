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
import { lazy, Suspense, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { EmptyState } from '@/components/empty-state'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber, formatTimestamp } from '@/lib/format'

import { useAnalysisLabels } from './labels'
import type { FailureLog, Snapshot } from './types'

const TrendChart = lazy(() => import('./trend-chart'))

export function FailureDashboard(props: {
  data: Snapshot
  total: number
  offset: number
  category: string
  onPage: (offset: number) => void
  onCategory: (category: string) => void
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const labels = useAnalysisLabels()
  const [dimension, setDimension] = useState<'models' | 'channels' | 'reasons'>(
    'models'
  )
  const [detail, setDetail] = useState<FailureLog | null>(null)
  const data = props.data
  const metrics = [
    { label: t('Recorded requests'), value: data.requests },
    { label: t('Zero output'), value: data.zero_output },
    { label: t('Interrupted streams'), value: data.interrupted },
    { label: t('Request errors'), value: data.errors },
  ]
  return (
    <div className='min-w-0 space-y-6'>
      <div className='grid grid-cols-2 gap-4 border-y py-5 lg:grid-cols-4'>
        {metrics.map((item) => (
          <div key={item.label} className='space-y-1'>
            <div className='text-muted-foreground text-sm'>{item.label}</div>
            <div className='text-2xl font-semibold tabular-nums'>
              {formatNumber(item.value, locale)}
            </div>
          </div>
        ))}
      </div>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Failure categories may overlap. Retries are grouped by request ID; client cancellations and missing evidence are listed separately.'
        )}
      </p>
      <div className='flex flex-wrap gap-x-6 gap-y-2 text-sm'>
        {[
          { label: t('Affected requests'), value: data.affected },
          { label: t('Client cancelled'), value: data.client_cancelled },
          { label: t('Recovered after retry'), value: data.recovered },
          {
            label: t('Insufficient output evidence'),
            value: data.unknown_output,
          },
        ].map((item) => (
          <span key={item.label}>
            {item.label}{' '}
            <strong className='tabular-nums'>
              {formatNumber(item.value, locale)}
            </strong>
          </span>
        ))}
      </div>
      {(data.truncated ||
        !data.log_recording_enabled ||
        !data.error_recording_enabled) && (
        <div role='status' className='bg-muted rounded-lg p-3 text-sm'>
          {data.truncated && (
            <p>
              {t(
                'Only the latest {{scanned}} of {{total}} log rows were scanned. Narrow the time range for complete statistics.',
                {
                  scanned: formatNumber(data.scanned_log_rows, locale),
                  total: formatNumber(data.total_log_rows, locale),
                }
              )}
            </p>
          )}
          {!data.log_recording_enabled && (
            <p>
              {t(
                'Consumption logging is disabled. Counts and failure rates do not cover all requests.'
              )}
            </p>
          )}
          {!data.error_recording_enabled && (
            <p>
              {t(
                'Request error logging is disabled. Enable ERROR_LOG_ENABLED on gateway nodes to include ordinary request failures.'
              )}
            </p>
          )}
        </div>
      )}
      {data.requests === 0 ? (
        <EmptyState
          title={t('No requests in this time range')}
          description={t('Check log recording or select another time range.')}
        />
      ) : (
        <>
          <Suspense fallback={<LoadingState />}>
            <TrendChart data={data.timeline} />
          </Suspense>
          <section className='space-y-3'>
            <div className='flex flex-wrap items-center justify-between gap-3'>
              <h2 className='font-semibold'>{t('Failure distribution')}</h2>
              <NativeSelect
                aria-label={t('Distribution dimension')}
                value={dimension}
                onChange={(event) =>
                  setDimension(event.target.value as typeof dimension)
                }
              >
                <NativeSelectOption value='models'>
                  {t('Model')}
                </NativeSelectOption>
                <NativeSelectOption value='channels'>
                  {t('Channel')}
                </NativeSelectOption>
                <NativeSelectOption value='reasons'>
                  {t('Error code / stream end reason')}
                </NativeSelectOption>
              </NativeSelect>
            </div>
            <StaticDataTable
              data={data[dimension]}
              getRowKey={(row) => row.name}
              columns={[
                {
                  id: 'name',
                  header: t('Name'),
                  cell: (row) => (
                    <span className='break-all'>
                      {row.name || t('Unknown')}
                    </span>
                  ),
                },
                {
                  id: 'requests',
                  header: t('Requests'),
                  cell: (row) => formatNumber(row.requests, locale),
                },
                {
                  id: 'affected',
                  header: t('Affected requests'),
                  cell: (row) =>
                    `${formatNumber(row.affected, locale)} (${formatNumber(row.requests ? (row.affected / row.requests) * 100 : 0, locale)}%)`,
                },
                {
                  id: 'zero',
                  header: t('Zero output'),
                  cell: (row) => formatNumber(row.zero_output, locale),
                },
                {
                  id: 'interrupted',
                  header: t('Interrupted streams'),
                  cell: (row) => formatNumber(row.interrupted, locale),
                },
                {
                  id: 'errors',
                  header: t('Request errors'),
                  cell: (row) => formatNumber(row.errors, locale),
                },
              ]}
            />
          </section>
        </>
      )}
      <section className='space-y-3'>
        <div className='flex flex-wrap items-center justify-between gap-3'>
          <h2 className='font-semibold'>{t('Diagnostic logs')}</h2>
          <div className='flex items-center gap-2'>
            <Label htmlFor='failure-category'>{t('Category')}</Label>
            <NativeSelect
              id='failure-category'
              value={props.category}
              onChange={(event) => props.onCategory(event.target.value)}
            >
              <NativeSelectOption value=''>{t('All')}</NativeSelectOption>
              {[
                'zero_output',
                'interrupted',
                'error',
                'client_cancelled',
                'unknown_output',
                'recovered',
              ].map((category) => (
                <NativeSelectOption key={category} value={category}>
                  {labels[category]}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </div>
        </div>
        <StaticDataTable
          data={data.logs}
          getRowKey={(row) => `${row.request_id}:${row.id}`}
          emptyContent={<EmptyState title={t('No matching diagnostic logs')} />}
          columns={[
            {
              id: 'time',
              header: t('Time'),
              cell: (row) => formatTimestamp(row.created_at),
            },
            {
              id: 'model',
              header: t('Model / Channel'),
              cell: (row) => (
                <div className='max-w-64 break-all'>
                  {row.model}
                  <div className='text-muted-foreground text-xs'>
                    {t('Channel')} {formatNumber(row.channel, locale)}
                  </div>
                </div>
              ),
            },
            {
              id: 'category',
              header: t('Category'),
              cell: (row) => (
                <div className='flex flex-wrap gap-1'>
                  {row.categories.map((kind) => (
                    <Badge key={kind} variant='outline'>
                      {labels[kind] ?? kind}
                    </Badge>
                  ))}
                </div>
              ),
            },
            {
              id: 'details',
              header: t('Details'),
              cell: (row) => (
                <Button
                  variant='ghost'
                  size='sm'
                  onClick={() => setDetail(row)}
                >
                  {t('View details')}
                </Button>
              ),
            },
          ]}
        />
        <div className='flex flex-wrap items-center justify-between gap-2 text-sm'>
          <span>
            {t('{{count}} diagnostic records', { count: props.total })}
          </span>
          <div className='flex gap-2'>
            <Button
              variant='outline'
              size='sm'
              disabled={props.offset === 0}
              onClick={() => props.onPage(Math.max(0, props.offset - 50))}
            >
              {t('Previous')}
            </Button>
            <Button
              variant='outline'
              size='sm'
              disabled={props.offset + 50 >= props.total}
              onClick={() => props.onPage(props.offset + 50)}
            >
              {t('Next')}
            </Button>
          </div>
        </div>
      </section>
      {detail && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) setDetail(null)
          }}
          title={t('Request diagnostics')}
          description={t(
            'Diagnostic facts only; request bodies and credentials are not recorded here.'
          )}
          footer={
            <CopyButton
              value={JSON.stringify(detail, null, 2)}
              size='sm'
              variant='outline'
            >
              {t('Copy diagnostics')}
            </CopyButton>
          }
        >
          <div className='space-y-4'>
            <p className='font-mono text-xs break-all'>{detail.request_id}</p>
            <dl className='grid grid-cols-1 gap-3 text-sm sm:grid-cols-2'>
              {Object.entries(detail.diagnostic).map(([key, value]) => (
                <div key={key} className='min-w-0'>
                  <dt className='text-muted-foreground font-mono text-xs'>
                    {key}
                  </dt>
                  <dd className='break-all'>{String(value ?? '—')}</dd>
                </div>
              ))}
            </dl>
          </div>
        </Dialog>
      )}
    </div>
  )
}
