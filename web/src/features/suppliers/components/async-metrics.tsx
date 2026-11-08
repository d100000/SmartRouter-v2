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
import { useTranslation } from 'react-i18next'

import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import type { SupplyAsyncMetrics } from '../types'

export function SupplyAsyncMetricsPanel(props: {
  metrics: SupplyAsyncMetrics
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const data = props.metrics
  const submissions = data.accepted_count + data.rejected_count
  const completed = data.succeeded_count + data.failed_count
  const metrics = [
    {
      label: t('Task submissions'),
      value: formatNumber(data.submitted_count, locale),
    },
    {
      label: t('Accepted submissions'),
      value: formatNumber(data.accepted_count, locale),
    },
    {
      label: t('Rejected submissions'),
      value: formatNumber(data.rejected_count, locale),
    },
    {
      label: t('Unknown submission results'),
      value: formatNumber(data.unknown_submission_count, locale),
    },
    {
      label: t('Submission acceptance rate'),
      value:
        submissions > 0
          ? `${formatNumber((data.accepted_count / submissions) * 100, locale)}%`
          : t('Insufficient samples'),
    },
    {
      label: t('Queued tasks'),
      value: formatNumber(data.queued_count, locale),
    },
    {
      label: t('Running tasks'),
      value: formatNumber(data.running_count, locale),
    },
    {
      label: t('Confirmed successful tasks'),
      value: formatNumber(data.succeeded_count, locale),
    },
    {
      label: t('Confirmed failed tasks'),
      value: formatNumber(data.failed_count, locale),
    },
    {
      label: t('Unknown task results'),
      value: formatNumber(data.unknown_count, locale),
    },
    {
      label: t('Task success rate'),
      value:
        completed > 0
          ? `${formatNumber((data.succeeded_count / completed) * 100, locale)}%`
          : t('Insufficient samples'),
    },
    {
      label: t('Average task submission latency (ms)'),
      value:
        data.avg_submit_latency_ms == null
          ? t('Unknown')
          : formatNumber(data.avg_submit_latency_ms, locale),
    },
    {
      label: t('Average task completion time (ms)'),
      value:
        data.avg_completion_ms == null
          ? t('Unknown')
          : formatNumber(data.avg_completion_ms, locale),
    },
  ]
  return (
    <section aria-label={t('Asynchronous tasks')} className='space-y-3'>
      <h3 className='text-sm font-semibold'>{t('Asynchronous tasks')}</h3>
      <p className='text-muted-foreground text-xs'>
        {t(
          'An accepted submission is not a successful task. Only upstream-confirmed results count toward task success rate.'
        )}
      </p>
      <dl className='grid grid-cols-2 gap-3 text-sm sm:grid-cols-3'>
        {metrics.map((metric) => (
          <div key={metric.label} className='min-w-0'>
            <dt className='text-muted-foreground'>{metric.label}</dt>
            <dd className='font-medium break-words tabular-nums'>
              {metric.value}
            </dd>
          </div>
        ))}
      </dl>
    </section>
  )
}
