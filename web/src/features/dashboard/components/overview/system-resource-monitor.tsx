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
import { Cpu, HardDrive, MemoryStick, Network } from 'lucide-react'
import { lazy, Suspense, useId, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'
import { cn } from '@/lib/utils'

import { getSystemResourceHistory, getSystemResources } from '../../api'
import {
  buildResourceTimeline,
  formatResourceBytes,
} from '../../lib/system-resources'
import { StatCard } from '../ui/stat-card'

const SystemResourceCharts = lazy(() => import('./system-resource-charts'))

export function SystemResourceMonitor() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const titleId = useId()
  const [hours, setHours] = useState(12)
  const current = useQuery({
    queryKey: ['system-resources', 'current'],
    queryFn: ({ signal }) => getSystemResources(signal),
    refetchInterval: 5000,
    staleTime: 4000,
    retry: 1,
  })
  const node = current.data?.node ?? ''
  const history = useQuery({
    queryKey: ['system-resources', 'history', node, hours],
    queryFn: ({ signal }) => getSystemResourceHistory(node, hours, signal),
    enabled: Boolean(node),
    refetchInterval: 60000,
    staleTime: 55000,
    retry: 1,
  })
  const sample = current.data?.sample
  const now = current.data
    ? current.data.server_time +
      Math.max(0, Math.floor((Date.now() - current.dataUpdatedAt) / 1000))
    : Math.floor(Date.now() / 1000)
  const from = now - hours * 3600
  const stale = current.isError || !sample || now - sample.timestamp > 20
  const points = useMemo(
    () => buildResourceTimeline(history.data, current.data, from, now),
    [history.data, current.data, from, now]
  )
  const timeFormat = new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
  let status = t('Waiting for samples')
  if (sample) status = stale ? t('Updates paused') : t('Live · every 5 seconds')
  const cards = [
    {
      title: t('CPU usage'),
      icon: Cpu,
      value: sample?.cpu_percent,
      description: t('All visible CPU cores'),
      tone: 'chart-1' as const,
    },
    {
      title: t('Memory usage'),
      icon: MemoryStick,
      value: sample?.memory_percent,
      description: t('{{used}} / {{total}} used', {
        used: formatResourceBytes(sample?.memory_used, locale),
        total: formatResourceBytes(sample?.memory_total, locale),
      }),
      tone: 'chart-2' as const,
    },
    {
      title: t('Disk usage'),
      icon: HardDrive,
      value: sample?.disk_percent,
      description: t('{{used}} / {{total}} used', {
        used: formatResourceBytes(sample?.disk_used, locale),
        total: formatResourceBytes(sample?.disk_total, locale),
      }),
      tone: 'chart-3' as const,
    },
  ]

  return (
    <section aria-labelledby={titleId} className='min-w-0 space-y-4'>
      <div className='bg-card overflow-hidden rounded-2xl border shadow-xs'>
        <div className='flex flex-wrap items-center justify-between gap-3 border-b px-4 py-3 sm:px-5'>
          <div className='min-w-0'>
            <div className='flex flex-wrap items-center gap-x-3 gap-y-1'>
              <h2 id={titleId} className='text-sm font-semibold'>
                {t('Server resources')}
              </h2>
              <span
                className='text-muted-foreground flex items-center gap-1.5 text-xs'
                role='status'
              >
                <span
                  className={cn(
                    'size-1.5 rounded-full',
                    stale ? 'bg-warning' : 'bg-success'
                  )}
                  aria-hidden='true'
                />
                {status}
              </span>
            </div>
            <p className='text-muted-foreground mt-1 text-xs break-all'>
              {node || t('Connecting to server')}
              {sample && (
                <span>
                  {' '}
                  ·{' '}
                  {t('Updated at {{time}}', {
                    time: timeFormat.format(new Date(sample.timestamp * 1000)),
                  })}
                </span>
              )}
            </p>
          </div>
          <Select
            value={hours}
            onValueChange={(value) => {
              if (value != null) setHours(value)
            }}
          >
            <SelectTrigger aria-label={t('Monitoring time range')}>
              <SelectValue>{t('Last {{hours}} hours', { hours })}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {[1, 6, 12, 24].map((value) => (
                <SelectItem key={value} value={value}>
                  {t('Last {{hours}} hours', { hours: value })}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        {current.isError && !current.data ? (
          <ErrorState
            className='min-h-40'
            description={t('Unable to load server resources')}
            onRetry={() => {
              void current.refetch()
            }}
          />
        ) : (
          <div className='grid grid-cols-2 gap-5 p-4 sm:p-5 lg:grid-cols-4'>
            {cards.map((card) => (
              <StatCard
                key={card.title}
                title={card.title}
                icon={card.icon}
                iconTone={card.tone}
                density='compact'
                value={
                  card.value == null
                    ? '—'
                    : `${formatNumber(card.value, locale, { maximumFractionDigits: 1 })}%`
                }
                description={card.description}
                loading={current.isPending}
              />
            ))}
            <StatCard
              title={t('Network bandwidth')}
              icon={Network}
              iconTone='chart-4'
              density='compact'
              value={`↓ ${formatResourceBytes(sample?.network_receive, locale)}/s`}
              description={`${t('Upload')}: ${formatResourceBytes(sample?.network_send, locale)}/s`}
              loading={current.isPending}
            />
          </div>
        )}
      </div>
      {history.isError && (
        <div
          className='border-warning/30 bg-warning/5 flex flex-wrap items-center justify-between gap-2 rounded-lg border px-4 py-2 text-sm'
          role='alert'
        >
          <span>
            {t(
              'History could not be refreshed. Displayed samples may be incomplete.'
            )}
          </span>
          <Button
            variant='outline'
            size='sm'
            onClick={() => {
              void history.refetch()
            }}
          >
            {t('Retry')}
          </Button>
        </div>
      )}
      <Suspense fallback={<Skeleton className='h-64 rounded-2xl' />}>
        <SystemResourceCharts
          points={points}
          from={from}
          to={now}
          loading={current.isPending || (Boolean(node) && history.isPending)}
        />
      </Suspense>
      <p className='text-muted-foreground text-xs leading-relaxed'>
        {t(
          'History is sampled every minute and retained for 24 hours. Gaps indicate missing samples.'
        )}{' '}
        {t(
          'CPU and memory reflect the host visible to the gateway. Disk uses the cache filesystem; network sums active non-loopback interfaces.'
        )}
      </p>
    </section>
  )
}
