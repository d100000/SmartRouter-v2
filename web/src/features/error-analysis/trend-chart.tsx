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

import type { Distribution } from './types'

export default function TrendChart(props: { data: Distribution[] }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const date = new Intl.DateTimeFormat(locale, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
  })
  const config = {
    zero_output: { label: t('Zero output'), color: 'var(--chart-1)' },
    interrupted: { label: t('Interrupted streams'), color: 'var(--chart-2)' },
    errors: { label: t('Request errors'), color: 'var(--chart-3)' },
  }
  return (
    <section
      className='min-w-0 space-y-4'
      aria-label={t('Hourly failure distribution')}
    >
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <h2 className='font-semibold'>{t('Hourly failure distribution')}</h2>
        <div className='flex flex-wrap gap-4 text-xs'>
          {Object.values(config).map((series) => (
            <span key={series.label} className='flex items-center gap-1.5'>
              <span
                aria-hidden
                className='size-2 rounded-full'
                style={{ backgroundColor: series.color }}
              />
              {series.label}
            </span>
          ))}
        </div>
      </div>
      <ChartContainer
        config={config}
        className='h-56 w-full'
        aria-label={t('Hourly failure distribution')}
      >
        <LineChart accessibilityLayer data={props.data}>
          <CartesianGrid vertical={false} />
          <XAxis
            dataKey='name'
            tickFormatter={(value: string) =>
              date.format(new Date(Number(value) * 1000))
            }
            minTickGap={45}
            tickLine={false}
            axisLine={false}
          />
          <YAxis
            allowDecimals={false}
            tickFormatter={(value: number) => formatNumber(value, locale)}
            width={45}
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
          {Object.entries(config).map(([key, series]) => (
            <Line
              key={key}
              dataKey={key}
              stroke={series.color}
              dot={false}
              strokeWidth={2}
              isAnimationActive={false}
            />
          ))}
        </LineChart>
      </ChartContainer>
    </section>
  )
}
