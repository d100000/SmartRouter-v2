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
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Bar,
  BarChart,
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  XAxis,
  YAxis,
} from 'recharts'

import { EmptyState } from '@/components/empty-state'
import { Button } from '@/components/ui/button'
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from '@/components/ui/chart'
import { PanelWrapper } from '@/features/dashboard/components/ui/panel-wrapper'
import { toIntlLocale } from '@/i18n/languages'
import { formatBillingCurrencyFromUSD } from '@/lib/currency'
import { formatNumber } from '@/lib/format'

import { getOverviewObjectLabel } from '../lib/object-label'
import type { OverviewSnapshot } from '../types'
import { OverviewHelp } from './overview-help'

const SERIES_COLORS = [
  'var(--chart-1)',
  'var(--chart-2)',
  'var(--chart-3)',
  'var(--chart-4)',
  'var(--chart-5)',
]
const DISTRIBUTION_DIMENSIONS = {
  channel: 'group',
  group: 'channel',
  key: 'channel',
  supplier: 'key',
} as const

export default function OverviewCharts(props: { snapshot: OverviewSnapshot }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [metric, setMetric] = useState<'revenue' | 'cost' | 'margin'>('revenue')
  const labels = {
    revenue: t('Billing Revenue'),
    cost: t('Known Upstream Cost'),
    margin: t('Estimated Gross Profit'),
  }
  const daily = props.snapshot.daily.map((point) => ({
    ...point,
    margin: point.complete_attempts > 0 ? point.margin : null,
    cost: point.cost_coverage == null ? null : point.cost,
  }))
  const amount = (value: number) =>
    formatBillingCurrencyFromUSD(value, {
      locale,
      compact: Math.abs(value) >= 1000,
      abbreviate: false,
      digitsSmall: 8,
    })
  return (
    <div className='grid min-w-0 gap-4 lg:grid-cols-2'>
      <PanelWrapper
        className='lg:row-span-2 lg:grid lg:grid-rows-subgrid lg:gap-0'
        title={
          <span className='inline-flex items-center gap-1'>
            {t('Daily Spending and Profit')}
            <OverviewHelp title={t('Daily Spending and Profit')}>
              <p>{t('Settled usage value, not cash receipts.')}</p>
              <p>
                {t(
                  'Profit is shown only for requests with complete cost records.'
                )}
              </p>
              <p>{t('Daily buckets use UTC.')}</p>
            </OverviewHelp>
          </span>
        }
        headerClassName='flex min-h-16 flex-col justify-center'
        headerActions={
          <div
            className='flex flex-wrap gap-1'
            role='group'
            aria-label={t('Spending Metric')}
          >
            {(['revenue', 'cost', 'margin'] as const).map((key) => (
              <Button
                key={key}
                size='xs'
                variant={metric === key ? 'secondary' : 'ghost'}
                aria-pressed={metric === key}
                onClick={() => setMetric(key)}
              >
                {labels[key]}
              </Button>
            ))}
          </div>
        }
      >
        <ChartContainer
          className='h-64 w-full'
          config={{
            [metric]: {
              label: labels[metric],
              color: SERIES_COLORS[metric === 'cost' ? 3 : 0],
            },
          }}
          aria-label={t('Daily Spending and Profit')}
        >
          <LineChart
            accessibilityLayer
            data={daily}
            margin={{ left: 0, right: 12 }}
          >
            <CartesianGrid vertical={false} />
            <XAxis
              dataKey='day'
              tickLine={false}
              axisLine={false}
              tickFormatter={(value: string) => value.slice(5)}
              minTickGap={28}
            />
            <YAxis
              width={70}
              tickLine={false}
              axisLine={false}
              tickFormatter={amount}
            />
            <ChartTooltip
              content={
                <ChartTooltipContent
                  formatter={(value) =>
                    formatBillingCurrencyFromUSD(Number(value), {
                      locale,
                      abbreviate: false,
                      digitsSmall: 8,
                    })
                  }
                />
              }
            />
            <Line
              type='linear'
              dataKey={metric}
              stroke={`var(--color-${metric})`}
              strokeWidth={2}
              dot={{ r: 2 }}
              connectNulls={false}
              isAnimationActive={false}
            />
          </LineChart>
        </ChartContainer>
      </PanelWrapper>
      <PanelWrapper
        className='lg:row-span-2 lg:grid lg:grid-rows-subgrid lg:gap-0'
        title={
          <span className='inline-flex items-center gap-1'>
            {t('Daily Health')}
            <OverviewHelp title={t('Daily Health')}>
              {t(
                'Success rate of completed, classified upstream attempts. Business rejections and client cancellations are excluded.'
              )}
            </OverviewHelp>
          </span>
        }
        headerClassName='flex min-h-16 flex-col justify-center'
      >
        {daily.some((point) => point.health != null) ? (
          <ChartContainer
            className='h-64 w-full'
            config={{
              health: {
                label: t('Attempt Success Rate'),
                color: 'var(--success)',
              },
            }}
            aria-label={t('Daily Health')}
          >
            <LineChart
              accessibilityLayer
              data={daily}
              margin={{ left: 0, right: 12 }}
            >
              <CartesianGrid vertical={false} />
              <XAxis
                dataKey='day'
                tickLine={false}
                axisLine={false}
                tickFormatter={(value: string) => value.slice(5)}
                minTickGap={28}
              />
              <YAxis
                width={48}
                tickLine={false}
                axisLine={false}
                domain={[0, 100]}
                tickFormatter={(value: number) =>
                  `${formatNumber(value, locale)}%`
                }
              />
              <ChartTooltip
                content={
                  <ChartTooltipContent
                    formatter={(value) =>
                      `${formatNumber(Number(value), locale)}%`
                    }
                  />
                }
              />
              <Line
                type='linear'
                dataKey='health'
                stroke='var(--color-health)'
                strokeWidth={2}
                dot={{ r: 2 }}
                connectNulls={false}
                isAnimationActive={false}
              />
            </LineChart>
          </ChartContainer>
        ) : (
          <EmptyState
            className='min-h-64'
            title={t('No samples yet')}
            description={t('Missing measurements are not counted as zero.')}
          />
        )}
      </PanelWrapper>
      <DistributionCharts snapshot={props.snapshot} />
    </div>
  )
}

function DistributionCharts(props: { snapshot: OverviewSnapshot }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const distribution = useMemo(() => {
    const totals = new Map<string, { name: string; revenue: number }>()
    for (const row of props.snapshot.distribution) {
      const total = totals.get(row.id) ?? {
        name: getOverviewObjectLabel(
          DISTRIBUTION_DIMENSIONS[props.snapshot.dimension],
          row.name,
          row.id,
          t
        ),
        revenue: 0,
      }
      total.revenue += row.revenue
      totals.set(row.id, total)
    }
    const objects = [...totals.entries()]
      .sort((left, right) => right[1].revenue - left[1].revenue)
      .slice(0, 5)
      .map(([id, total]) => [id, total.name] as const)
    const seriesById = new Map(
      objects.map(([id], index) => [id, `series_${index}`])
    )
    const byDay = new Map<string, Record<string, string | number>>()
    for (const row of props.snapshot.distribution) {
      const day = byDay.get(row.day) ?? { day: row.day }
      const key = seriesById.get(row.id) ?? 'other'
      day[key] = Number(day[key] ?? 0) + row.revenue
      byDay.set(row.day, day)
    }
    return {
      objects,
      days: [...byDay.values()].sort((left, right) =>
        String(left.day).localeCompare(String(right.day))
      ),
    }
  }, [props.snapshot.distribution, props.snapshot.dimension, t])
  const hasOther = distribution.days.some((day) => 'other' in day)
  const config = Object.fromEntries(
    distribution.objects.map(([, name], index) => [
      `series_${index}`,
      { label: name, color: SERIES_COLORS[index] },
    ])
  )
  if (hasOther) {
    config.other = { label: t('Other'), color: 'var(--muted-foreground)' }
  }
  const titles = {
    channel: t('Daily Group Distribution'),
    group: t('Daily Channel Distribution'),
    key: t('Daily Channel Distribution'),
    supplier: t('Daily Upstream Key Distribution'),
  }
  const models = [...props.snapshot.models]
    .sort((left, right) => right.attempts - left.attempts)
    .slice(0, 10)
    .map((model) => ({
      ...model,
      share:
        props.snapshot.summary.attempts > 0
          ? (model.attempts / props.snapshot.summary.attempts) * 100
          : null,
    }))
  return (
    <>
      <PanelWrapper
        className='lg:row-span-2 lg:grid lg:grid-rows-subgrid lg:gap-0'
        title={
          <span className='inline-flex items-center gap-1'>
            {titles[props.snapshot.dimension]}
            <OverviewHelp title={titles[props.snapshot.dimension]}>
              {t(
                'Distribution by billing revenue; the largest returned categories are shown.'
              )}
            </OverviewHelp>
          </span>
        }
        headerClassName='flex min-h-16 flex-col justify-center'
        empty={distribution.days.length === 0}
      >
        <ChartContainer
          className='h-64 w-full'
          config={config}
          aria-label={titles[props.snapshot.dimension]}
        >
          <BarChart
            accessibilityLayer
            data={distribution.days}
            margin={{ left: 0, right: 12 }}
          >
            <CartesianGrid vertical={false} />
            <XAxis
              dataKey='day'
              tickLine={false}
              axisLine={false}
              tickFormatter={(value: string) => value.slice(5)}
              minTickGap={28}
            />
            <YAxis
              width={70}
              tickLine={false}
              axisLine={false}
              tickFormatter={(value: number) =>
                formatBillingCurrencyFromUSD(value, {
                  locale,
                  compact: Math.abs(value) >= 1000,
                  abbreviate: false,
                  digitsSmall: 8,
                })
              }
            />
            <ChartTooltip
              content={
                <ChartTooltipContent
                  formatter={(value) =>
                    formatBillingCurrencyFromUSD(Number(value), {
                      locale,
                      abbreviate: false,
                      digitsSmall: 8,
                    })
                  }
                />
              }
            />
            <Legend />
            {distribution.objects.map(([id, name], index) => (
              <Bar
                key={id}
                dataKey={`series_${index}`}
                name={name}
                stackId='distribution'
                fill={SERIES_COLORS[index]}
                isAnimationActive={false}
              />
            ))}
            {hasOther && (
              <Bar
                dataKey='other'
                name={t('Other')}
                stackId='distribution'
                fill='var(--muted-foreground)'
                isAnimationActive={false}
              />
            )}
          </BarChart>
        </ChartContainer>
      </PanelWrapper>
      <PanelWrapper
        className='lg:row-span-2 lg:grid lg:grid-rows-subgrid lg:gap-0'
        title={
          <span className='inline-flex items-center gap-1'>
            {t('Model Call Distribution')}
            <OverviewHelp title={t('Model Call Distribution')}>
              {t('Upstream attempts by model, including retries.')}
            </OverviewHelp>
          </span>
        }
        headerClassName='flex min-h-16 flex-col justify-center'
        empty={models.length === 0}
      >
        <ChartContainer
          className='h-64 w-full'
          config={{
            attempts: { label: t('Attempts'), color: 'var(--chart-2)' },
          }}
          aria-label={t('Model Call Distribution')}
        >
          <BarChart
            accessibilityLayer
            data={models}
            layout='vertical'
            margin={{ left: 0, right: 12 }}
          >
            <CartesianGrid horizontal={false} />
            <XAxis
              type='number'
              allowDecimals={false}
              tickLine={false}
              axisLine={false}
              tickFormatter={(value: number) => formatNumber(value, locale)}
            />
            <YAxis
              type='category'
              dataKey='name'
              width={140}
              tickLine={false}
              axisLine={false}
            />
            <ChartTooltip
              content={
                <ChartTooltipContent
                  formatter={(value, _name, item) => (
                    <span>
                      {formatNumber(Number(value), locale)} ·{' '}
                      {item.payload.share == null
                        ? '—'
                        : `${formatNumber(item.payload.share, locale)}%`}
                    </span>
                  )}
                />
              }
            />
            <Bar
              dataKey='attempts'
              fill='var(--color-attempts)'
              radius={3}
              isAnimationActive={false}
            />
          </BarChart>
        </ChartContainer>
      </PanelWrapper>
    </>
  )
}
