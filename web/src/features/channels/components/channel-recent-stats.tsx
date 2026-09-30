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
import { AlertTriangle } from 'lucide-react'
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
import {
  formatCompactNumber,
  formatNumber,
  formatTimestampToDate,
} from '@/lib/format'

import { getChannelRecentStats } from '../api'
import { isTagAggregateRow } from '../lib/channel-utils'
import type { Channel, ChannelRecentStat } from '../types'

type RecentStatsState = {
  status: 'loading' | 'unavailable' | 'ready'
  delayed: boolean
  refreshedAt: number
  byChannel: ReadonlyMap<number, ChannelRecentStat>
}

const RecentStatsContext = createContext<RecentStatsState>({
  status: 'loading',
  delayed: false,
  refreshedAt: 0,
  byChannel: new Map(),
})

// One independent query for the page: table loading and row identity never
// depend on statistics, and memoized cards receive updates through context.
export function ChannelRecentStatsProvider(props: { children: ReactNode }) {
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30_000)
    return () => clearInterval(timer)
  }, [])
  const query = useQuery({
    queryKey: ['channel-recent-stats'],
    queryFn: ({ signal }) => getChannelRecentStats(signal),
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
    }
  }, [data, isError, dataUpdatedAt, byChannel, now])

  return (
    <RecentStatsContext.Provider value={value}>
      {props.children}
    </RecentStatsContext.Provider>
  )
}

export function ChannelRecentStatsCell(props: { channel: Channel }) {
  const { t, i18n } = useTranslation()
  const stats = useContext(RecentStatsContext)
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const channels = isTagAggregateRow(props.channel)
    ? props.channel.children
    : [props.channel]
  let requests = 0
  let successes = 0
  for (const channel of channels) {
    const item = stats.byChannel.get(channel.id)
    requests += item?.requests ?? 0
    successes += item?.successes ?? 0
  }
  const rate =
    requests > 0
      ? `${formatNumber((successes / requests) * 100, locale)}%`
      : '—'
  let label = `${rate} / ${formatCompactNumber(requests, locale)}`
  let description = `${t('Success / Requests (10m)')}: ${rate} / ${formatNumber(requests, locale)}`
  if (stats.status === 'loading') {
    label = '…'
    description = t('Channel statistics are loading')
  } else if (stats.status === 'unavailable') {
    label = '— / —'
    description = t('Channel statistics are unavailable')
  }
  const warning = stats.status === 'unavailable' || stats.delayed

  return (
    <Tooltip>
      <TooltipTrigger
        render={<span tabIndex={0} />}
        aria-label={description}
        className='inline-flex max-w-full items-center gap-1 text-xs whitespace-nowrap tabular-nums'
      >
        <span className='truncate'>{label}</span>
        {warning && (
          <AlertTriangle
            className='text-muted-foreground size-3 shrink-0'
            aria-hidden='true'
          />
        )}
      </TooltipTrigger>
      <TooltipContent className='block space-y-1'>
        <p>{description}</p>
        {stats.status === 'ready' && stats.delayed && (
          <p>{t('Statistics are delayed; showing the last snapshot.')}</p>
        )}
        <p>
          {t(
            'Completed valid attempts across all groups and models in the last 10 minutes. Retries count separately; client cancellations and user errors are excluded.'
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
  )
}
