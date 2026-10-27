/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.
*/
import { useTranslation } from 'react-i18next'

import { toIntlLocale } from '@/i18n/languages'

import { formatUptimePct } from '../lib/format'
import type { PerformanceModelRow } from '../lib/model-status'

export function PerformanceReliabilityChart(props: {
  model?: PerformanceModelRow
  windowStart?: number
  windowEnd?: number
  bucketSeconds?: number
  locale: string
}) {
  const { t } = useTranslation()
  const values = (props.model?.series ?? [])
    .filter(
      (point) =>
        Number.isFinite(point.ts) && Number.isFinite(point.success_rate)
    )
    .sort((a, b) => a.ts - b.ts)
  if (values.length === 0) {
    return (
      <div className='text-muted-foreground flex h-64 items-center justify-center rounded-xl border text-sm'>
        {t('No performance data available')}
      </div>
    )
  }

  const start = props.windowStart ?? values[0].ts
  const end = Math.max(
    start + 1,
    props.windowEnd ?? (values.at(-1)?.ts ?? start) + 3600
  )
  const bucketSeconds = props.bucketSeconds ?? 3600
  const points = values.filter((point) => point.ts >= start && point.ts <= end)
  const dateFormat = new Intl.DateTimeFormat(toIntlLocale(props.locale), {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    timeZoneName: 'short',
  })
  const width = 720
  const top = 16
  const bottom = 206
  const plotted = points.map((point) => ({
    ...point,
    x: ((point.ts - start) / (end - start)) * width,
    y:
      bottom -
      (Math.max(0, Math.min(100, point.success_rate)) / 100) * (bottom - top),
  }))
  const path = plotted
    .map((point, index) => {
      const move =
        index === 0 || point.ts - plotted[index - 1].ts > bucketSeconds * 1.5
      return `${move ? 'M' : 'L'} ${point.x.toFixed(1)} ${point.y.toFixed(1)}`
    })
    .join(' ')

  return (
    <div className='space-y-3'>
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <div>
          <p className='text-sm font-semibold'>{props.model?.model}</p>
          <p className='text-muted-foreground text-xs'>
            {props.model?.companyName} ·{' '}
            {formatUptimePct(props.model?.successRate ?? Number.NaN)}
          </p>
        </div>
        <span className='text-muted-foreground text-xs'>
          {t('Final success rate')}
        </span>
      </div>
      <div className='bg-muted/20 rounded-xl border px-2 py-3 sm:px-4'>
        <svg
          viewBox='0 0 720 230'
          className='h-56 w-full'
          role='img'
          aria-label={t('24-hour success rate curve')}
          preserveAspectRatio='none'
        >
          <desc>
            {points
              .map(
                (point) =>
                  `${dateFormat.format(new Date(point.ts * 1000))}: ${formatUptimePct(point.success_rate)}`
              )
              .join('; ')}
          </desc>
          {[100, 75, 50, 25, 0].map((rate) => {
            const y = bottom - (rate / 100) * (bottom - top)
            return (
              <g key={rate}>
                <line
                  x1='0'
                  x2='720'
                  y1={y}
                  y2={y}
                  stroke='currentColor'
                  className='text-border'
                  strokeDasharray='3 7'
                />
                <text
                  x='716'
                  y={y - 3}
                  textAnchor='end'
                  className='fill-muted-foreground font-mono text-[11px]'
                >
                  {rate}%
                </text>
              </g>
            )
          })}
          <path
            d={path}
            fill='none'
            stroke='currentColor'
            className='text-primary'
            strokeWidth='3'
            strokeLinecap='round'
            strokeLinejoin='round'
            vectorEffect='non-scaling-stroke'
          />
          {plotted.map((point) => (
            <circle
              key={point.ts}
              cx={point.x}
              cy={point.y}
              r='3'
              className='fill-primary'
            >
              <title>
                {dateFormat.format(new Date(point.ts * 1000))}:{' '}
                {formatUptimePct(point.success_rate)}
              </title>
            </circle>
          ))}
        </svg>
        <div className='text-muted-foreground flex justify-between gap-3 text-[10px]'>
          <span>{dateFormat.format(new Date(start * 1000))}</span>
          <span className='text-right'>
            {dateFormat.format(new Date(end * 1000))}
          </span>
        </div>
      </div>
    </div>
  )
}
