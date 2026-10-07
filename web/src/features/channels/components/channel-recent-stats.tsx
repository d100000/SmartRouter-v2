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
import { AlertTriangle, Clock } from 'lucide-react'
import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react'
import { useTranslation } from 'react-i18next'

import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber, formatTimestampToDate } from '@/lib/format'
import { cn } from '@/lib/utils'

import { getChannelRecentStats } from '../api'
import { CHANNEL_RECENT_STATS_QUERY_KEY } from '../constants'
import { isTagAggregateRow } from '../lib/channel-utils'
import type { Channel, ChannelRecentStat, ChannelRecentStats } from '../types'
import { ChannelRowActionsLayoutContext } from './channel-row-actions-context'

const HISTORY_OFFSETS_SECONDS = [3000, 2400, 1800, 1200, 600, 0] as const

type RecentStatsState = {
  status: 'loading' | 'unavailable' | 'ready'
  delayed: boolean
  refreshedAt: number
  byChannel: ReadonlyMap<number, ChannelRecentStat>
  snapshot?: ChannelRecentStats
}

const RecentStatsContext = createContext<RecentStatsState>({
  status: 'loading',
  delayed: false,
  refreshedAt: 0,
  byChannel: new Map(),
})

// One independent query for the page: table loading and row identity never
// depend on statistics, and memoized cards receive updates through context.
export function ChannelRecentStatsProvider(props: {
  children: ReactNode
  channels?: readonly Channel[]
}) {
  const channelIds = useMemo(() => {
    if (!props.channels) return undefined
    const ids = [
      ...new Set(
        props.channels.flatMap((channel) =>
          isTagAggregateRow(channel)
            ? channel.children.map((child) => child.id)
            : [channel.id]
        )
      ),
    ]
      .filter((id) => Number.isSafeInteger(id) && id > 0)
      .sort((left, right) => left - right)
    // The endpoint caps both IDs and CSV length. Oversized pages still make
    // one request, using the endpoint's unfiltered snapshot fallback.
    return ids.length <= 500 && ids.join(',').length <= 8192 ? ids : undefined
  }, [props.channels])
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30_000)
    return () => clearInterval(timer)
  }, [])
  const query = useQuery({
    queryKey: channelIds
      ? [...CHANNEL_RECENT_STATS_QUERY_KEY, channelIds]
      : CHANNEL_RECENT_STATS_QUERY_KEY,
    queryFn: ({ signal }) => getChannelRecentStats(signal, channelIds),
    enabled: !props.channels || props.channels.length > 0,
    staleTime: 30_000,
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    retry: 1,
    meta: { errorToast: false, errorRedirect: false },
  })
  const byChannel = useMemo(
    () => new Map(query.data?.items.map((item) => [item.channel_id, item])),
    [query.data]
  )
  const { data, isError, dataUpdatedAt } = query
  const value = useMemo<RecentStatsState>(() => {
    let status: RecentStatsState['status'] = 'loading'
    if (data?.ready) status = 'ready'
    else if (isError) status = 'unavailable'
    return {
      status,
      delayed:
        isError ||
        (Boolean(data?.ready) &&
          Math.max(now, dataUpdatedAt) / 1000 - (data?.refreshed_at ?? 0) > 90),
      refreshedAt: data?.refreshed_at ?? 0,
      byChannel,
      snapshot: data,
    }
  }, [data, isError, dataUpdatedAt, byChannel, now])

  return (
    <RecentStatsContext.Provider value={value}>
      {props.children}
    </RecentStatsContext.Provider>
  )
}

export function ChannelRecentStatsCell(props: {
  channel: Channel
  placement?: 'name' | 'footer'
}) {
  const { t, i18n } = useTranslation()
  const stats = useContext(RecentStatsContext)
  const layout = useContext(ChannelRowActionsLayoutContext)
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  if (props.placement === 'name' && layout === 'card') return null

  const channels = isTagAggregateRow(props.channel)
    ? props.channel.children
    : [props.channel]
  const items = channels.map((channel) => stats.byChannel.get(channel.id))
  let requests = 0
  let successes = 0
  let ttftSamples = 0
  let ttftSum = 0
  let responseSamples = 0
  let responseSum = 0
  for (const item of items) {
    requests += item?.requests_1h ?? 0
    successes += item?.successes_1h ?? 0
    ttftSamples += item?.ttft_samples_5m ?? 0
    ttftSum += item?.ttft_sum_ms_5m ?? 0
    responseSamples += item?.response_samples_5m ?? 0
    responseSum += item?.response_sum_ms_5m ?? 0
  }
  const collectedSince = stats.snapshot?.collected_since ?? 0
  const reference = items.find((item) => item?.history?.length === 6)?.history
  const history = HISTORY_OFFSETS_SECONDS.map((offsetSeconds, index) => {
    const end = stats.refreshedAt - offsetSeconds
    const bucket = {
      offsetSeconds,
      start_time: reference?.[index]?.start_time ?? end - 600,
      end_time: reference?.[index]?.end_time ?? end,
      requests: 0,
      successes: 0,
    }
    for (const item of items) {
      bucket.requests += item?.history?.[index]?.requests ?? 0
      bucket.successes += item?.history?.[index]?.successes ?? 0
    }
    return bucket
  })
  const rate =
    requests > 0
      ? `${formatNumber((successes / requests) * 100, locale)}%`
      : '—'
  const ttftMs = ttftSamples > 0 ? ttftSum / ttftSamples : null
  const responseMs = responseSamples > 0 ? responseSum / responseSamples : null
  let ttft = '—'
  if (ttftMs !== null) {
    ttft =
      ttftMs < 1000
        ? t('{{value}}ms', { value: formatNumber(ttftMs, locale) })
        : t('{{value}}s', { value: formatNumber(ttftMs / 1000, locale) })
  }
  const response =
    responseMs === null
      ? '—'
      : t('{{value}}ms', { value: formatNumber(responseMs, locale) })
  const unavailable = stats.status !== 'ready'
  const placeholder = stats.status === 'loading' ? '…' : '—'
  const warning = stats.status === 'unavailable' || stats.delayed
  const rateDescription = t('Success rate (1h): {{rate}}', {
    rate: unavailable ? placeholder : rate,
  })
  const latencyDescription = t(
    'Average streaming first-token time (5m): {{value}}',
    {
      value: unavailable ? placeholder : ttft,
    }
  )
  const partialHistory = collectedSince > stats.refreshedAt - 3600
  let description = t('Channel health (1h)')
  if (stats.status === 'loading') {
    description = t('Channel statistics are loading')
  } else if (stats.status === 'unavailable') {
    description = t('Channel statistics are unavailable')
  }

  return (
    <span
      role='group'
      aria-label={description}
      data-table-text='secondary'
      aria-description={
        stats.status === 'ready' && stats.delayed
          ? t('Statistics are delayed; showing the last snapshot.')
          : undefined
      }
      className='inline-grid h-5 w-50 shrink-0 grid-cols-[46px_56px_minmax(0,1fr)] items-center gap-1 text-xs whitespace-nowrap tabular-nums'
    >
      <span className='flex items-center gap-0.5'>
        {history.map((bucket) => {
          const notCollected =
            bucket.requests === 0 &&
            (collectedSince === 0 || bucket.end_time <= collectedSince)
          const partial = !notCollected && bucket.start_time < collectedSince
          let collectionDescription = ''
          if (unavailable) collectionDescription = description
          else if (notCollected) collectionDescription = t('Not collected yet')
          const bucketRate =
            bucket.requests > 0 ? bucket.successes / bucket.requests : null
          const label = unavailable
            ? description
            : t('{{start}}–{{end}}: {{rate}}', {
                start: formatTimestampToDate(bucket.start_time),
                end: formatTimestampToDate(bucket.end_time),
                rate:
                  bucketRate === null
                    ? '—'
                    : `${formatNumber(bucketRate * 100, locale)}%`,
              })
          let color = 'bg-muted-foreground/25'
          if (!unavailable && !notCollected && bucketRate !== null) {
            if (bucketRate >= 0.99) color = 'bg-success'
            else if (bucketRate >= 0.95) color = 'bg-warning'
            else color = 'bg-destructive'
          }
          return (
            <Tooltip key={bucket.offsetSeconds}>
              <TooltipTrigger
                render={<span role='img' tabIndex={0} />}
                aria-label={label}
                className={cn(
                  'block h-3 w-1.5 shrink-0 rounded-[2px] outline-offset-2',
                  color,
                  !unavailable &&
                    notCollected &&
                    'border-muted-foreground/50 border border-dashed bg-transparent',
                  !unavailable &&
                    partial &&
                    'ring-muted-foreground/40 ring-1 ring-inset'
                )}
              />
              <TooltipContent className='block space-y-1'>
                <p>{label}</p>
                {collectionDescription ? (
                  <p>{collectionDescription}</p>
                ) : (
                  <>
                    <p>
                      {t('Successful attempts: {{successes}} / {{requests}}', {
                        successes: formatNumber(bucket.successes, locale),
                        requests: formatNumber(bucket.requests, locale),
                      })}
                    </p>
                    {bucket.requests === 0 && (
                      <p>{t('No requests in this interval')}</p>
                    )}
                    {partial && <p>{t('Partially collected interval')}</p>}
                  </>
                )}
                {!unavailable && stats.delayed && (
                  <p>
                    {t('Statistics are delayed; showing the last snapshot.')}
                  </p>
                )}
              </TooltipContent>
            </Tooltip>
          )
        })}
      </span>
      <Tooltip>
        <TooltipTrigger
          render={<span tabIndex={0} />}
          aria-label={rateDescription}
          className='min-w-0 truncate text-right'
        >
          {unavailable ? placeholder : rate}
        </TooltipTrigger>
        <TooltipContent className='block space-y-1'>
          <p>{unavailable ? description : rateDescription}</p>
          {!unavailable && (
            <p>
              {t('Successful attempts: {{successes}} / {{requests}}', {
                successes: formatNumber(successes, locale),
                requests: formatNumber(requests, locale),
              })}
            </p>
          )}
          {stats.status === 'ready' && partialHistory && (
            <p>
              {t('History is incomplete; collection started at {{time}}.', {
                time: formatTimestampToDate(collectedSince),
              })}
            </p>
          )}
          {!unavailable && warning && (
            <p>{t('Statistics are delayed; showing the last snapshot.')}</p>
          )}
          <p>
            {t(
              'Completed valid attempts across all groups and models in the last hour. Retries count separately; client cancellations and user errors are excluded.'
            )}
          </p>
          <p>
            {t(
              'Updated every 30 seconds on this instance; collection restarts when the service restarts.'
            )}
          </p>
          {stats.refreshedAt > 0 && (
            <p>
              {t('Updated at {{time}}', {
                time: formatTimestampToDate(stats.refreshedAt),
              })}
            </p>
          )}
        </TooltipContent>
      </Tooltip>
      <Tooltip>
        <TooltipTrigger
          render={<span tabIndex={0} />}
          aria-label={latencyDescription}
          className='inline-flex min-w-0 items-center gap-1'
        >
          {warning ? (
            <AlertTriangle
              className='text-muted-foreground size-3 shrink-0'
              aria-hidden='true'
            />
          ) : (
            <Clock
              className='text-muted-foreground size-3 shrink-0'
              aria-hidden='true'
            />
          )}
          <span className='min-w-0 truncate'>
            {unavailable ? placeholder : ttft}
          </span>
        </TooltipTrigger>
        <TooltipContent className='block space-y-1'>
          <p>{unavailable ? description : latencyDescription}</p>
          {!unavailable && (
            <>
              <p>
                {ttftSamples > 0
                  ? t(
                      'Based on {{count}} completed successful streaming attempts.',
                      { replace: { count: formatNumber(ttftSamples, locale) } }
                    )
                  : t(
                      'No successful streaming measurements in the last 5 minutes.'
                    )}
              </p>
              <p>
                {t(
                  'Average non-streaming first-response time (5m): {{value}}',
                  { value: response }
                )}
              </p>
              <p>
                {responseSamples > 0
                  ? t(
                      'Based on {{count}} completed successful non-streaming attempts.',
                      {
                        replace: {
                          count: formatNumber(responseSamples, locale),
                        },
                      }
                    )
                  : t(
                      'No successful non-streaming measurements in the last 5 minutes.'
                    )}
              </p>
            </>
          )}
          {!unavailable && warning && (
            <p>{t('Statistics are delayed; showing the last snapshot.')}</p>
          )}
          {stats.refreshedAt > 0 && (
            <p>
              {t('Updated at {{time}}', {
                time: formatTimestampToDate(stats.refreshedAt),
              })}
            </p>
          )}
        </TooltipContent>
      </Tooltip>
    </span>
  )
}
