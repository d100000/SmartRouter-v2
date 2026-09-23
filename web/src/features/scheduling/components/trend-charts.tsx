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
import { CartesianGrid, Line, LineChart, XAxis, YAxis } from 'recharts'

import { EmptyState } from '@/components/empty-state'
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from '@/components/ui/chart'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import type { SchedulingTrendPoint } from '../types'

export default function TrendCharts(props: { points: SchedulingTrendPoint[] }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const time = new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
  })
  const data = props.points.map((point) => ({
    ...point,
    time: time.format(new Date(point.timestamp * 1000)),
    success_rate: point.success_rate == null ? null : point.success_rate * 100,
  }))
  const charts = [
    {
      key: 'requests',
      title: t('Original requests per minute'),
      unit: '',
      color: 'var(--chart-1)',
    },
    {
      key: 'success_rate',
      title: t('Channel success rate'),
      unit: '%',
      color: 'var(--chart-2)',
    },
    {
      key: 'avg_ttft_ms',
      title: t('First-output latency'),
      unit: t('ms'),
      color: 'var(--chart-3)',
    },
    {
      key: 'in_flight',
      title: t('Peak in-flight requests per minute'),
      unit: '',
      color: 'var(--chart-4)',
    },
  ] as const
  return (
    <section aria-label={t('Last 30 minutes trends')} className='space-y-3'>
      <h3 className='font-semibold'>{t('Last 30 minutes trends')}</h3>
      <div className='grid gap-4 lg:grid-cols-2'>
        {charts.map((chart) => (
          <div key={chart.key} className='min-w-0 rounded-lg border p-4'>
            <h4 className='mb-3 text-sm font-medium'>{chart.title}</h4>
            {data.some((point) => point[chart.key] != null) ? (
              <ChartContainer
                className='h-44 w-full'
                config={{
                  [chart.key]: { label: chart.title, color: chart.color },
                }}
                aria-label={chart.title}
              >
                <LineChart
                  accessibilityLayer
                  data={data}
                  margin={{ left: 0, right: 12 }}
                >
                  <CartesianGrid vertical={false} />
                  <XAxis
                    dataKey='time'
                    tickLine={false}
                    axisLine={false}
                    minTickGap={28}
                  />
                  <YAxis
                    width={48}
                    tickLine={false}
                    axisLine={false}
                    tickFormatter={(value: number) =>
                      formatNumber(value, locale)
                    }
                    domain={
                      chart.key === 'success_rate' ? [0, 100] : [0, 'auto']
                    }
                  />
                  <ChartTooltip
                    content={
                      <ChartTooltipContent
                        formatter={(value) =>
                          `${formatNumber(Number(value), locale)} ${chart.unit}`
                        }
                      />
                    }
                  />
                  <Line
                    type='linear'
                    dataKey={chart.key}
                    stroke={`var(--color-${chart.key})`}
                    strokeWidth={2}
                    dot={false}
                    connectNulls={false}
                    isAnimationActive={false}
                  />
                </LineChart>
              </ChartContainer>
            ) : (
              <EmptyState
                className='min-h-44'
                title={t('No samples yet')}
                description={t('Missing measurements are not counted as zero.')}
              />
            )}
          </div>
        ))}
      </div>
    </section>
  )
}
