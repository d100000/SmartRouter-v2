/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.
*/
import { Link } from '@tanstack/react-router'
import { useId } from 'react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import { formatPerformanceTimestamp } from '../lib/format'
import type { PerformanceCollectionHealth } from '../types'

export function PerformanceCollectionHealthStatus(props: {
  collection?: PerformanceCollectionHealth
  lastSampleAt?: number | null
  stale?: boolean
}) {
  const { t, i18n } = useTranslation()
  const id = useId()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const collection = props.collection
  const quarantined = collection?.quarantined_events ?? 0
  const partial = Boolean(
    collection &&
    (collection.availability === 'partial' ||
      collection.unpersisted_events > 0 ||
      collection.dropped_events > 0 ||
      collection.stale_collectors > 0 ||
      quarantined > 0)
  )
  const delayed = collection?.availability === 'delayed'
  let title = t('Collection status unavailable')
  let description = t(
    'The server does not report collection health. Existing metrics remain available.'
  )
  if (collection) {
    title = t('Collection is active')
    description = t(
      'Newly accepted samples normally appear within {{seconds}} seconds.',
      { seconds: formatNumber(collection.target_visibility_seconds, locale) }
    )
    if (!collection.enabled || collection.availability === 'disabled') {
      title = t('Collection is disabled')
      description = t(
        'New samples are not collected. Previously persisted data remains available.'
      )
    }
    if (delayed) {
      title = t('Projection is delayed')
      description = t(
        'Samples are stored, but aggregate metrics are still catching up.'
      )
    }
    if (partial) {
      title = t('Collection may be incomplete')
      description = t(
        'Some samples are unpersisted or quarantined, or a collector has stopped reporting. Persisted metrics remain available.'
      )
    }
  }
  if (props.stale) {
    title = t('Collection status unavailable')
    description = t(
      'Collection health could not be refreshed. Times below are from the last successful query.'
    )
  }
  const lastSampleAt = collection?.last_sample_at ?? props.lastSampleAt
  const fields: Array<{
    key: string
    label: string
    value: string
    timestamp?: number | null
    detail?: string
  }> = [
    {
      key: 'sample',
      label: t('Latest sample'),
      timestamp: lastSampleAt,
      value: formatPerformanceTimestamp(lastSampleAt, locale),
    },
  ]
  if (collection) {
    fields.push(
      {
        key: 'enabled',
        label: t('New sample collection'),
        value: collection.enabled ? t('Enabled') : t('Disabled'),
      },
      {
        key: 'accepted',
        label: t('Durably accepted at'),
        timestamp: collection.last_accepted_at,
        value: formatPerformanceTimestamp(collection.last_accepted_at, locale),
      },
      {
        key: 'projected',
        label: t('Last projection'),
        timestamp: collection.last_projected_at,
        value: formatPerformanceTimestamp(collection.last_projected_at, locale),
      },
      {
        key: 'pending',
        label: t('Pending projection'),
        value: formatNumber(collection.pending_events, locale),
        detail:
          collection.pending_events > 0
            ? t('Oldest pending sample: {{time}}', {
                time: formatPerformanceTimestamp(
                  collection.oldest_pending_at,
                  locale
                ),
              })
            : undefined,
      },
      {
        key: 'unpersisted',
        label: t('Unpersisted samples'),
        value: formatNumber(collection.unpersisted_events, locale),
        detail:
          collection.unpersisted_events > 0
            ? t('Oldest unpersisted sample: {{time}}', {
                time: formatPerformanceTimestamp(
                  collection.oldest_unpersisted_at,
                  locale
                ),
              })
            : undefined,
      },
      {
        key: 'heartbeat',
        label: t('Collector heartbeat'),
        timestamp: collection.collector_heartbeat_at,
        value: formatPerformanceTimestamp(
          collection.collector_heartbeat_at,
          locale
        ),
      },
      {
        key: 'watermark',
        label: t('Persistence watermark'),
        timestamp: collection.persisted_through,
        value: collection.persisted_through
          ? formatPerformanceTimestamp(collection.persisted_through, locale)
          : t('Not available'),
      }
    )
    if (collection.local_unpersisted_events > 0) {
      fields.push({
        key: 'local',
        label: t('Unpersisted on this instance'),
        value: formatNumber(collection.local_unpersisted_events, locale),
      })
    }
    if (collection.dropped_events > 0) {
      fields.push({
        key: 'dropped',
        label: t('Dropped samples'),
        value: formatNumber(collection.dropped_events, locale),
      })
    }
    if (collection.stale_collectors > 0) {
      fields.push({
        key: 'stale',
        label: t('Stale collectors'),
        value: formatNumber(collection.stale_collectors, locale),
      })
    }
    if (quarantined > 0) {
      fields.push({
        key: 'quarantined',
        label: t('Quarantined samples'),
        value: formatNumber(quarantined, locale),
        detail: t('See the server logs for the quarantine reason.'),
      })
    }
  }

  return (
    <section className='space-y-2 border-t pt-3'>
      <h4 className='text-sm font-medium'>
        {t('Collection across all instances')}
      </h4>
      <Alert
        role={props.stale || partial || delayed ? 'alert' : 'status'}
        aria-label={t('Collection health')}
        variant={partial ? 'destructive' : 'default'}
      >
        <AlertTitle>{title}</AlertTitle>
        <AlertDescription>{description}</AlertDescription>
      </Alert>
      {collection && !collection.enabled && (
        <Button
          variant='link'
          size='sm'
          role='link'
          className='h-auto max-w-full px-0 whitespace-normal'
          render={
            <Link
              to='/system-settings/operations/$section'
              params={{ section: 'alerts' }}
            />
          }
        >
          {t('Open collection settings')}
        </Button>
      )}
      <dl>
        {fields.map((field) => (
          <div
            key={field.key}
            className='grid min-w-0 gap-1 border-b py-2 last:border-0 sm:grid-cols-2'
          >
            <dt
              id={`${id}-${field.key}`}
              className='text-muted-foreground text-xs'
            >
              {field.label}
            </dt>
            <dd
              aria-labelledby={`${id}-${field.key}`}
              className='min-w-0 text-sm break-words sm:text-right'
            >
              {field.timestamp && field.value !== '—' ? (
                <time dateTime={new Date(field.timestamp * 1000).toISOString()}>
                  {field.value}
                </time>
              ) : (
                field.value
              )}
              {field.detail && (
                <p className='text-muted-foreground text-xs'>{field.detail}</p>
              )}
            </dd>
          </div>
        ))}
      </dl>
      {!lastSampleAt && (
        <p className='text-muted-foreground text-xs'>
          {t(
            'No sample timestamp is available. Older buckets may not include one.'
          )}
        </p>
      )}
      {collection && !collection.persisted_through && (
        <p className='text-muted-foreground text-xs'>
          {t(
            'Acceptance and projection timestamps do not establish a complete persistence watermark.'
          )}
        </p>
      )}
    </section>
  )
}
