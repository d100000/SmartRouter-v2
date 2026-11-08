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

import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from '@/components/ui/chart'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import type { SupplyTrend } from '../types'

export default function SupplyTrendChart(props: {
  data: SupplyTrend[]
  timezone: string
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const date = new Intl.DateTimeFormat(locale, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    timeZone: props.timezone,
  })
  const config = {
    success_count: {
      label: t('Successful synchronous attempts'),
      color: 'var(--chart-1)',
    },
    failure_count: {
      label: t('Failed synchronous attempts'),
      color: 'var(--chart-2)',
    },
    unknown_count: {
      label: t('Unknown synchronous results'),
      color: 'var(--chart-3)',
    },
  }
  return (
    <div className='grid min-w-0 gap-6 lg:grid-cols-2'>
      {[
        { title: t('Synchronous request trends'), series: config },
        {
          title: t('Average synchronous latency (ms)'),
          series: {
            avg_latency_ms: {
              label: t('Average synchronous latency (ms)'),
              color: 'var(--chart-4)',
            },
          },
        },
      ].map((chart) => (
        <section
          key={chart.title}
          className='min-w-0 space-y-3'
          aria-label={chart.title}
        >
          <h3 className='text-sm font-semibold'>{chart.title}</h3>
          <div className='flex flex-wrap gap-3 text-xs'>
            {Object.values(chart.series).map((series) => (
              <span
                key={series.label}
                className='inline-flex items-center gap-1'
              >
                <span
                  aria-hidden
                  className='size-2 rounded-full'
                  style={{ backgroundColor: series.color }}
                />
                {series.label}
              </span>
            ))}
          </div>
          <ChartContainer
            config={chart.series}
            className='h-56 w-full'
            aria-label={chart.title}
          >
            <LineChart accessibilityLayer data={props.data}>
              <CartesianGrid vertical={false} />
              <XAxis
                dataKey='timestamp'
                tickFormatter={(value: number) =>
                  date.format(new Date(value * 1000))
                }
                minTickGap={45}
                tickLine={false}
                axisLine={false}
              />
              <YAxis
                allowDecimals={false}
                tickFormatter={(value: number) => formatNumber(value, locale)}
                width={50}
                tickLine={false}
                axisLine={false}
              />
              <ChartTooltip
                content={
                  <ChartTooltipContent
                    labelFormatter={(value) =>
                      date.format(new Date(Number(value) * 1000))
                    }
                  />
                }
              />
              {Object.entries(chart.series).map(([key, series]) => (
                <Line
                  key={key}
                  dataKey={key}
                  stroke={series.color}
                  dot={false}
                  strokeWidth={2}
                  isAnimationActive={false}
                  connectNulls={false}
                />
              ))}
            </LineChart>
          </ChartContainer>
        </section>
      ))}
    </div>
  )
}
