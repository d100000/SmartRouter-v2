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

import { Badge } from '@/components/ui/badge'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import type { SchedulingSnapshot } from '../types'

export function HealthOverview(props: { data: SchedulingSnapshot }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const summary = props.data.summary
  const number = (value: number | null) =>
    value == null ? '—' : formatNumber(value, locale)
  const percent = (value: number | null) =>
    value == null ? '—' : `${number(value * 100)}%`
  const stats = [
    {
      label: t('Original requests (30m)'),
      value: number(summary.requests_30m),
      detail: t('{{count}} channel attempts', { count: summary.attempts_30m }),
    },
    {
      label: t('Success rate (5m)'),
      value: percent(summary.success_rate_5m),
      detail: `${t('Success rate (30m)')}: ${percent(summary.success_rate_30m)}`,
    },
    {
      label: t('Average time to first output (5m)'),
      value:
        summary.avg_ttft_ms_5m == null
          ? '—'
          : t('{{value}} ms', { value: number(summary.avg_ttft_ms_5m) }),
      detail: t('Latency target: {{value}} ms', {
        value: number(props.data.config.target_ttft_ms),
      }),
    },
    {
      label: t('Healthy / eligible channels'),
      value: `${number(summary.healthy_channels)} / ${number(summary.eligible_channels)}`,
      detail: t('{{count}} downweighted channels', {
        count: summary.degraded_channels,
      }),
    },
    {
      label: t('In-flight requests'),
      value: number(summary.in_flight),
      detail: t('Shared upstream capacity across groups'),
    },
    {
      label: t('Largest traffic share (30m)'),
      value: percent(summary.top_traffic_share),
      detail: t('Observed dispatch share, including retries'),
    },
  ]
  return (
    <div className='space-y-4'>
      <dl className='grid grid-cols-1 divide-y rounded-lg border sm:grid-cols-2 xl:grid-cols-3'>
        {stats.map((stat) => (
          <div key={stat.label} className='min-w-0 p-4'>
            <dt className='text-muted-foreground text-xs'>{stat.label}</dt>
            <dd className='mt-2 text-2xl font-semibold tabular-nums'>
              {stat.value}
            </dd>
            <p className='text-muted-foreground mt-1 text-xs'>{stat.detail}</p>
          </div>
        ))}
      </dl>
      <div className='flex flex-wrap items-center gap-2 rounded-lg border p-3 text-xs'>
        <span className='font-medium'>{t('Routing watchpoints')}</span>
        {summary.eligible_channels === 0 && (
          <Badge variant='destructive'>{t('No eligible channels')}</Badge>
        )}
        {summary.eligible_channels === 1 && (
          <Badge variant='outline'>{t('Single-channel dependency')}</Badge>
        )}
        {summary.degraded_channels > 0 && (
          <Badge variant='destructive'>
            {t('{{count}} downweighted channels', {
              count: summary.degraded_channels,
            })}
          </Badge>
        )}
        {summary.top_traffic_share > 0.9 && summary.eligible_channels > 1 && (
          <Badge variant='outline'>
            {t('Traffic concentration above 90%')}
          </Badge>
        )}
        {summary.success_rate_5m != null &&
          summary.success_rate_5m < props.data.config.target_success_rate && (
            <Badge variant='destructive'>
              {t('Short-window success below target')}
            </Badge>
          )}
        {summary.avg_ttft_ms_5m != null &&
          summary.avg_ttft_ms_5m > props.data.config.target_ttft_ms && (
            <Badge variant='outline'>
              {t('First-output latency above target')}
            </Badge>
          )}
        <span className='text-muted-foreground'>
          {t(
            'Watch short-window failures, latency drift, capacity saturation and traffic concentration.'
          )}
        </span>
      </div>
    </div>
  )
}
