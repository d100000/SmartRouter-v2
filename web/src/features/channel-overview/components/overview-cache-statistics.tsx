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
import { Database, DatabaseZap, Gauge, Percent, Repeat } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { PanelWrapper } from '@/features/dashboard/components/ui/panel-wrapper'
import { StatCard } from '@/features/dashboard/components/ui/stat-card'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import type { OverviewSnapshot } from '../types'
import { OverviewCacheTable } from './overview-cache-table'
import { OverviewHelp } from './overview-help'

export function OverviewCacheStatistics(props: { snapshot: OverviewSnapshot }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [view, setView] = useState('model')
  const metrics = props.snapshot.summary
  const hasSamples = metrics.cache_usage_attempts > 0
  const cards = [
    {
      title: t('Cache Token Hit Rate'),
      value:
        hasSamples && metrics.cache_hit_rate != null
          ? `${formatNumber(metrics.cache_hit_rate, locale)}%`
          : '—',
      icon: Percent,
    },
    {
      title: t('Cache Request Hit Rate'),
      value:
        hasSamples && metrics.cache_attempt_hit_rate != null
          ? `${formatNumber(metrics.cache_attempt_hit_rate, locale)}%`
          : '—',
      icon: Repeat,
    },
    {
      title: t('Cache Read Tokens'),
      value: hasSamples ? formatNumber(metrics.cache_read_tokens, locale) : '—',
      icon: DatabaseZap,
    },
    {
      title: t('Cache Write Tokens'),
      value: hasSamples
        ? formatNumber(metrics.cache_write_tokens, locale)
        : '—',
      icon: Database,
    },
    {
      title: t('Cache Usage Coverage'),
      value:
        metrics.cache_usage_coverage == null
          ? '—'
          : `${formatNumber(metrics.cache_usage_coverage, locale)}%`,
      description:
        metrics.cache_usage_attempts == null
          ? undefined
          : t('{{known}} / {{total}} attempts', {
              known: formatNumber(metrics.cache_usage_attempts, locale),
              total: formatNumber(metrics.attempts, locale),
            }),
      icon: Gauge,
    },
  ]

  return (
    <section aria-label={t('Cache Statistics')} className='min-w-0'>
      <PanelWrapper
        title={
          <span className='inline-flex items-center gap-1'>
            {t('Cache Statistics')}
            <OverviewHelp title={t('Cache Statistics')}>
              <p>
                {t(
                  'Cache hit rate is cached read tokens divided by total input tokens in measured upstream usage. Retries are counted separately.'
                )}
              </p>
              <p>
                {t(
                  'Request hit rate is attempts with cache reads divided by attempts with measured cache usage. Retries are counted separately.'
                )}
              </p>
              <p>
                {t(
                  'Models use the requested model name. Cache writes are not cache hits.'
                )}
              </p>
              <p>
                {t('Cache statistics are limited to {{count}} rows per view.', {
                  count: formatNumber(props.snapshot.limits.max_rows, locale),
                })}
              </p>
            </OverviewHelp>
          </span>
        }
        contentClassName='p-0 sm:p-0'
      >
        <section
          aria-label={t('Cache Usage Samples')}
          className='grid min-w-0 grid-cols-2 gap-4 border-b p-4 sm:px-5 lg:grid-cols-5'
        >
          {cards.map((card) => (
            <StatCard key={card.title} {...card} density='compact' />
          ))}
        </section>
        <div className='space-y-2 px-4 pt-3 sm:px-5'>
          <p className='text-muted-foreground text-xs leading-relaxed'>
            {t(
              'Only actual upstream usage is included. Historical calls and local estimates without cache measurements are excluded.'
            )}
          </p>
          {!hasSamples && (
            <p className='text-muted-foreground text-xs'>
              {t('No cache usage samples yet.')}
            </p>
          )}
        </div>
        <Tabs value={view} onValueChange={(value) => setView(String(value))}>
          <div className='overflow-x-auto px-4 pt-3 sm:px-5'>
            <TabsList aria-label={t('Cache Statistics')}>
              <TabsTrigger value='model'>{t('By Model')}</TabsTrigger>
              <TabsTrigger value='channel'>{t('By Channel')}</TabsTrigger>
              <TabsTrigger value='channel_model'>
                {t('By Channel and Model')}
              </TabsTrigger>
            </TabsList>
          </div>
          <TabsContent value='model' className='min-w-0'>
            <OverviewCacheTable
              dimension='model'
              rows={props.snapshot.models ?? []}
            />
          </TabsContent>
          <TabsContent value='channel_model' className='min-w-0'>
            {props.snapshot.channel_models_truncated && (
              <p
                role='status'
                className='text-muted-foreground px-4 py-2 text-xs sm:px-5'
              >
                {t(
                  'Showing the {{count}} busiest channel/model combinations. Select a channel to narrow the results. Summary metrics include all combinations.',
                  {
                    count: formatNumber(props.snapshot.limits.max_rows, locale),
                  }
                )}
              </p>
            )}
            <OverviewCacheTable
              dimension='channel_model'
              rows={props.snapshot.channel_models ?? []}
            />
          </TabsContent>
          <TabsContent value='channel' className='min-w-0'>
            <OverviewCacheTable
              dimension='channel'
              rows={props.snapshot.channels ?? []}
            />
          </TabsContent>
        </Tabs>
      </PanelWrapper>
    </section>
  )
}
