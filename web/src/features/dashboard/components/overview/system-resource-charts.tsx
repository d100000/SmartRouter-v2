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
  type ChartConfig,
} from '@/components/ui/chart'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import {
  formatResourceBytes,
  type SystemResourcePoint,
} from '../../lib/system-resources'
import { PanelWrapper } from '../ui/panel-wrapper'

export default function SystemResourceCharts(props: {
  points: SystemResourcePoint[]
  from: number
  to: number
  loading: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const timeFormat = new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
  })
  const tooltipTime = new Intl.DateTimeFormat(locale, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
  const charts = [
    {
      title: t('CPU usage'),
      keys: ['cpu_percent'],
      rate: false,
      colors: ['var(--chart-1)'],
      labels: [t('CPU usage')],
    },
    {
      title: t('Memory usage'),
      keys: ['memory_percent'],
      rate: false,
      colors: ['var(--chart-2)'],
      labels: [t('Memory usage')],
    },
    {
      title: t('Disk usage'),
      keys: ['disk_percent'],
      rate: false,
      colors: ['var(--chart-3)'],
      labels: [t('Disk usage')],
    },
    {
      title: t('Network bandwidth'),
      keys: ['network_receive', 'network_send'],
      rate: true,
      colors: ['var(--chart-4)', 'var(--chart-5)'],
      labels: [t('Download'), t('Upload')],
    },
  ]
  return (
    <div className='grid min-w-0 gap-4 lg:grid-cols-2'>
      {charts.map((chart) => {
        const config: ChartConfig = Object.fromEntries(
          chart.keys.map((key, index) => [
            key,
            { label: chart.labels[index], color: chart.colors[index] },
          ])
        )
        const hasSamples = props.points.some((point) =>
          chart.keys.some(
            (key) => point[key as keyof SystemResourcePoint] != null
          )
        )
        return (
          <PanelWrapper
            key={chart.title}
            title={chart.title}
            height='h-44'
            loading={props.loading}
            empty={!hasSamples}
            emptyMessage={t(
              'Collecting resource history. Samples appear as they arrive.'
            )}
            headerActions={
              <span className='text-muted-foreground flex flex-wrap gap-x-3 gap-y-1 text-xs'>
                {chart.keys.map((key, index) => (
                  <span key={key} className='flex items-center gap-1.5'>
                    <span
                      className='h-0.5 w-3 rounded-full'
                      style={{ backgroundColor: chart.colors[index] }}
                    />
                    {chart.rate ? chart.labels[index] : '%'}
                  </span>
                ))}
              </span>
            }
          >
            <ChartContainer config={config} className='h-44 w-full'>
              <LineChart
                accessibilityLayer
                aria-label={chart.title}
                data={props.points}
                margin={{ top: 8, right: 12, bottom: 0, left: 0 }}
              >
                <CartesianGrid vertical={false} />
                <XAxis
                  dataKey='timestamp'
                  type='number'
                  domain={[props.from, props.to]}
                  allowDataOverflow
                  tickFormatter={(value: number) =>
                    timeFormat.format(new Date(value * 1000))
                  }
                  tickLine={false}
                  axisLine={false}
                  minTickGap={36}
                  tickCount={5}
                />
                <YAxis
                  width={chart.rate ? 76 : 40}
                  domain={chart.rate ? [0, 'auto'] : [0, 100]}
                  tickFormatter={(value: number) =>
                    chart.rate
                      ? `${formatResourceBytes(value, locale)}/s`
                      : `${formatNumber(value, locale)}%`
                  }
                  tickLine={false}
                  axisLine={false}
                  tickCount={4}
                />
                <ChartTooltip
                  content={
                    <ChartTooltipContent
                      labelFormatter={(_, payload) =>
                        tooltipTime.format(
                          new Date(Number(payload[0]?.payload.timestamp) * 1000)
                        )
                      }
                      formatter={(value, name) => (
                        <div className='flex w-full items-center justify-between gap-4'>
                          <span className='text-muted-foreground'>
                            {config[String(name)]?.label}
                          </span>
                          <span className='font-mono tabular-nums'>
                            {chart.rate
                              ? `${formatResourceBytes(Number(value), locale)}/s`
                              : `${formatNumber(Number(value), locale, { maximumFractionDigits: 1 })}%`}
                          </span>
                        </div>
                      )}
                    />
                  }
                />
                {chart.keys.map((key, index) => (
                  <Line
                    key={key}
                    type='linear'
                    dataKey={key}
                    stroke={`var(--color-${key})`}
                    strokeWidth={2}
                    strokeDasharray={index === 1 ? '5 3' : undefined}
                    dot={
                      props.points.filter(
                        (point) =>
                          point[key as keyof SystemResourcePoint] != null
                      ).length <= 1
                        ? { r: 3 }
                        : false
                    }
                    activeDot={{ r: 4 }}
                    connectNulls={false}
                    isAnimationActive={false}
                  />
                ))}
              </LineChart>
            </ChartContainer>
          </PanelWrapper>
        )
      })}
    </div>
  )
}
