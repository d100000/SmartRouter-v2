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
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowUpRight, ChevronDown, RefreshCw } from 'lucide-react'
import { useId, useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { getAllLogs } from '@/features/usage-logs/api'
import { LOG_TYPE_ENUM } from '@/features/usage-logs/constants'
import type { UsageLog } from '@/features/usage-logs/data/schema'
import {
  logTokenName,
  parseLogOther,
  renderLogContent,
} from '@/features/usage-logs/lib/format'
import type { LogOtherData } from '@/features/usage-logs/types'
import { toIntlLocale } from '@/i18n/languages'
import dayjs from '@/lib/dayjs'
import {
  formatLogQuota,
  formatNumber,
  formatTimestampToDate,
  formatUseTime,
} from '@/lib/format'
import { requireServerSuccess } from '@/lib/server-error-message'
import { cn } from '@/lib/utils'

import {
  getChannelStatusBadge,
  getChannelTypeLabel,
} from '../../lib/channel-utils'
import type { Channel } from '../../types'
import {
  ChannelHealthOverview,
  ChannelLogRecordingNotice,
} from '../channel-recent-stats'
import { useChannels } from '../channels-provider'

const RANGE_SECONDS = { '1h': 3600, '24h': 86400 } as const
type RequestRange = keyof typeof RANGE_SECONDS

const PAGE_SIZE = 20
const REFRESH_SECONDS = 30
const SENSITIVE_MASK = '••••'

type RecentRequest = {
  key: string
  log: UsageLog
  other: LogOtherData | null
  failed: boolean
}

// Successful and failed requests are separate log types, and the log list
// filters by one type, so each is read on its own. That also gives each count.
async function fetchRecentRequests(channelId: number, seconds: number) {
  const until = Math.floor(Date.now() / 1000)
  const since = until - seconds
  const read = async (type: number) => {
    const response = requireServerSuccess(
      await getAllLogs({
        p: 1,
        page_size: PAGE_SIZE,
        channel: channelId,
        type,
        start_timestamp: since,
      })
    )
    return {
      logs: (response.data?.items ?? []) as UsageLog[],
      total: response.data?.total ?? 0,
    }
  }
  const [succeeded, failed] = await Promise.all([
    read(LOG_TYPE_ENUM.CONSUME),
    read(LOG_TYPE_ENUM.ERROR),
  ])
  return { since, until, succeeded, failed }
}

function toRecentRequest(log: UsageLog): RecentRequest {
  const other = parseLogOther(log.other)
  return {
    // With a ClickHouse log store the id is the row's position in one result,
    // so it does not identify a log on its own.
    key: [log.type, log.id, log.created_at, log.request_id].join(':'),
    log,
    other,
    failed: log.type === LOG_TYPE_ENUM.ERROR || other?.stream_failure != null,
  }
}

function DetailItem(props: { label: string; children: ReactNode }) {
  return (
    <div className='flex min-w-0 flex-col gap-0.5'>
      <dt className='text-muted-foreground text-xs font-normal'>
        {props.label}
      </dt>
      <dd className='break-words'>{props.children}</dd>
    </div>
  )
}

function RecentRequestRow(props: {
  request: RecentRequest
  open: boolean
  onToggle: () => void
  /** The list spans more than one day. */
  showDate: boolean
  masked: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const panelId = useId()
  const { log, other, failed } = props.request

  const status = other?.status_code
  let result = t('Success')
  if (failed) result = status ? `${t('Failed')} ${status}` : t('Failed')
  // Durations are logged in whole seconds, first-token times in milliseconds.
  const duration = log.use_time > 0 ? formatUseTime(log.use_time) : '<1s'
  const firstToken =
    log.is_stream && other?.frt != null && other.frt > 0
      ? formatUseTime(other.frt / 1000)
      : '—'
  const tokens =
    log.prompt_tokens > 0 || log.completion_tokens > 0
      ? `${formatNumber(log.prompt_tokens, locale)} / ${formatNumber(log.completion_tokens, locale)}`
      : '—'

  const errorCode = other?.error_code || other?.stream_failure?.code
  const errorMessage =
    other?.stream_failure?.message || renderLogContent(other, t) || log.content
  // The channels the request had been sent to when this attempt ended. What
  // happened to it afterwards is on its other log rows.
  const attempts = (other?.admin_info?.use_channel ?? []).map((id) => `#${id}`)
  let cost = formatLogQuota(log.quota)
  if (failed && log.quota === 0) cost = t('Not charged')
  const mask = (value: string) => {
    if (!value) return '—'
    return props.masked ? SENSITIVE_MASK : value
  }

  return (
    <>
      <TableRow>
        <TableCell title={formatTimestampToDate(log.created_at)}>
          {dayjs(log.created_at * 1000).format(
            props.showDate ? 'MM-DD HH:mm:ss' : 'HH:mm:ss'
          )}
        </TableCell>
        <TableCell>
          <StatusBadge
            label={result}
            variant={failed ? 'danger' : 'success'}
            size='sm'
            copyable={false}
          />
        </TableCell>
        <TableCell>
          <span className='block max-w-56 truncate' title={log.model_name}>
            {log.model_name || '—'}
          </span>
          <span data-table-text='secondary' className='text-muted-foreground'>
            {log.is_stream ? t('Stream') : t('Non-stream')}
          </span>
        </TableCell>
        <TableCell>
          {duration} / {firstToken}
        </TableCell>
        <TableCell>{tokens}</TableCell>
        <TableCell>
          <span
            className='block max-w-36 truncate font-mono'
            title={log.request_id}
          >
            {log.request_id || '—'}
          </span>
        </TableCell>
        <TableCell className='w-10 text-right'>
          <Button
            variant='ghost'
            size='icon-sm'
            aria-expanded={props.open}
            aria-controls={props.open ? panelId : undefined}
            aria-label={props.open ? t('Hide details') : t('Show details')}
            onClick={props.onToggle}
          >
            <ChevronDown
              aria-hidden='true'
              className={cn('transition-transform', props.open && 'rotate-180')}
            />
          </Button>
        </TableCell>
      </TableRow>
      {props.open && (
        <TableRow id={panelId} className='bg-muted/50 hover:bg-muted/50'>
          <TableCell colSpan={7} className='p-3 whitespace-normal'>
            <div className='flex flex-col gap-3'>
              {failed && (
                <div className='flex flex-col gap-0.5'>
                  <span className='text-muted-foreground text-xs font-normal'>
                    {t('Error message')}
                  </span>
                  <p className='text-destructive font-mono text-xs font-normal break-words'>
                    {errorCode ? `${errorCode}: ` : ''}
                    {errorMessage || '—'}
                  </p>
                </div>
              )}
              <dl className='grid grid-cols-2 gap-3 sm:grid-cols-4'>
                <DetailItem label={t('Retry Chain')}>
                  {attempts.length > 1
                    ? attempts.join(' → ')
                    : t('First attempt')}
                </DetailItem>
                <DetailItem label={`${t('User')} / ${t('Token')}`}>
                  {mask(log.username)} / {mask(logTokenName(log, t))}
                </DetailItem>
                <DetailItem label={t('Group')}>{mask(log.group)}</DetailItem>
                <DetailItem label={t('Cost')}>{cost}</DetailItem>
              </dl>
              {log.request_id && (
                <div className='flex flex-wrap gap-2'>
                  <CopyButton
                    value={log.request_id}
                    variant='outline'
                    size='sm'
                    aria-label={t('Copy request ID')}
                  >
                    {t('Copy request ID')}
                  </CopyButton>
                  <Link
                    to='/usage-logs/$section'
                    params={{ section: 'common' }}
                    search={{
                      requestId: log.request_id,
                      startTime: (log.created_at - 3600) * 1000,
                      endTime: (log.created_at + 3600) * 1000,
                    }}
                    className={buttonVariants({
                      variant: 'outline',
                      size: 'sm',
                    })}
                  >
                    {t('Open this request in usage logs')}
                  </Link>
                </div>
              )}
            </div>
          </TableCell>
        </TableRow>
      )}
    </>
  )
}

/**
 * A channel's figures and its latest requests. The requests come from the
 * usage log, filtered by this channel, and the page links there for the
 * channel's whole history.
 */
export function ChannelDetailsDialog(props: {
  channel: Channel
  onClose: () => void
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const { sensitiveVisible } = useChannels()
  const [range, setRange] = useState<RequestRange>('1h')
  const [failedOnly, setFailedOnly] = useState(false)
  const [expanded, setExpanded] = useState<string | null>(null)

  const requests = useQuery({
    queryKey: ['channel-recent-requests', props.channel.id, range],
    queryFn: () => fetchRecentRequests(props.channel.id, RANGE_SECONDS[range]),
    placeholderData: keepPreviousData,
    staleTime: 0,
    refetchInterval: REFRESH_SECONDS * 1000,
    refetchIntervalInBackground: false,
    retry: 1,
    meta: { errorToast: false, errorRedirect: false },
  })
  const data = requests.data
  const rows = useMemo(() => {
    if (!data) return []
    const logs = failedOnly
      ? data.failed.logs
      : [...data.succeeded.logs, ...data.failed.logs]
          .sort(
            (left, right) =>
              right.created_at - left.created_at || right.id - left.id
          )
          .slice(0, PAGE_SIZE)
    return logs.map(toRecentRequest)
  }, [data, failedOnly])
  const failedTotal = data?.failed.total ?? 0
  const allTotal = (data?.succeeded.total ?? 0) + failedTotal
  const total = failedOnly ? failedTotal : allTotal
  const shown = failedOnly ? 'failed' : 'all'
  const status = getChannelStatusBadge(props.channel.status)

  return (
    <Dialog
      open
      onOpenChange={(open) => !open && props.onClose()}
      title={t('Channel details')}
      description={
        <span className='flex flex-wrap items-center gap-2'>
          <span className='font-mono'>
            #{sensitiveVisible ? props.channel.id : SENSITIVE_MASK}
          </span>
          <span className='text-foreground font-medium'>
            {sensitiveVisible ? props.channel.name : SENSITIVE_MASK}
          </span>
          <StatusBadge
            label={t(getChannelTypeLabel(props.channel.type))}
            variant='neutral'
            size='sm'
            copyable={false}
          />
          <StatusBadge
            label={t(status.label)}
            variant={status.variant}
            size='sm'
            copyable={false}
          />
        </span>
      }
      contentClassName='sm:max-w-4xl'
      footer={
        <Button variant='outline' onClick={props.onClose}>
          {t('Close')}
        </Button>
      }
    >
      <div className='flex flex-col gap-4'>
        <ChannelHealthOverview channel={props.channel} />

        <div className='flex flex-wrap items-center gap-2'>
          <ToggleGroup
            value={[shown]}
            onValueChange={(values) => {
              const next = values.find((value) => value !== shown)
              if (next) setFailedOnly(next === 'failed')
            }}
            aria-label={t('Result')}
            variant='outline'
            size='sm'
          >
            <ToggleGroupItem value='all'>
              {t('All')}
              {data && (
                <span className='tabular-nums'>
                  {formatNumber(allTotal, locale)}
                </span>
              )}
            </ToggleGroupItem>
            <ToggleGroupItem value='failed'>
              {t('Failed only')}
              {data && (
                <span className='tabular-nums'>
                  {formatNumber(failedTotal, locale)}
                </span>
              )}
            </ToggleGroupItem>
          </ToggleGroup>
          <label className='flex items-center gap-2 text-sm'>
            <span className='text-muted-foreground'>{t('Time range')}</span>
            <NativeSelect
              size='sm'
              value={range}
              onChange={(event) => {
                const value = event.target.value
                if (value === '1h' || value === '24h') setRange(value)
              }}
            >
              <NativeSelectOption value='1h'>
                {t('Last 1 hour')}
              </NativeSelectOption>
              <NativeSelectOption value='24h'>
                {t('Last 24 hours')}
              </NativeSelectOption>
            </NativeSelect>
          </label>
          <span className='text-muted-foreground ml-auto text-xs'>
            {requests.dataUpdatedAt > 0 &&
              `${t('Updated at {{time}}', {
                time: dayjs(requests.dataUpdatedAt).format('HH:mm:ss'),
              })} · `}
            {t('Refreshes every {{seconds}} seconds', {
              seconds: REFRESH_SECONDS,
            })}
          </span>
          <Button
            variant='outline'
            size='icon-sm'
            aria-label={t('Refresh')}
            aria-busy={requests.isFetching}
            disabled={requests.isFetching}
            onClick={() => void requests.refetch()}
          >
            <RefreshCw
              aria-hidden='true'
              className={cn(requests.isFetching && 'animate-spin')}
            />
          </Button>
          <Link
            to='/usage-logs/$section'
            params={{ section: 'common' }}
            search={{
              channel: String(props.channel.id),
              startTime: data ? data.since * 1000 : undefined,
              endTime: data ? (data.until + 3600) * 1000 : undefined,
              type: failedOnly ? ['5'] : undefined,
            }}
            className={buttonVariants({ size: 'sm' })}
          >
            {t('View in usage logs')}
            <ArrowUpRight data-icon='inline-end' aria-hidden='true' />
          </Link>
        </div>

        <ChannelLogRecordingNotice />

        {requests.isPending && <LoadingState className='min-h-40' />}
        {requests.isError && !data && (
          <ErrorState
            className='min-h-40'
            description={t('Unable to load recent requests.')}
            onRetry={() => void requests.refetch()}
          />
        )}
        {requests.isError && data && (
          <p role='alert' className='text-destructive text-sm'>
            {t('Refresh failed. Showing the requests loaded earlier.')}
          </p>
        )}
        {data && rows.length === 0 && (
          <EmptyState
            className='min-h-40'
            title={
              failedOnly
                ? t('No failed requests in this time range')
                : t('No requests in this time range')
            }
          />
        )}
        {data && rows.length > 0 && (
          <div className='flex flex-col gap-2'>
            <Table className='min-w-[720px]'>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('Time')}</TableHead>
                  <TableHead>{t('Result')}</TableHead>
                  <TableHead>{t('Model')}</TableHead>
                  <TableHead>
                    {t('Duration')} / {t('First token')}
                  </TableHead>
                  <TableHead>
                    {t('Input')} / {t('Output')}
                  </TableHead>
                  <TableHead>{t('Request ID')}</TableHead>
                  <TableHead>
                    <span className='sr-only'>{t('Details')}</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className='[&>tr]:h-11'>
                {rows.map((request) => (
                  <RecentRequestRow
                    key={request.key}
                    request={request}
                    open={expanded === request.key}
                    onToggle={() =>
                      setExpanded(expanded === request.key ? null : request.key)
                    }
                    showDate={range !== '1h'}
                    masked={!sensitiveVisible}
                  />
                ))}
              </TableBody>
            </Table>
            {total > rows.length && (
              <p className='text-muted-foreground text-xs'>
                {t('Showing the latest {{shown}} of {{total}} requests.', {
                  shown: formatNumber(rows.length, locale),
                  total: formatNumber(total, locale),
                })}
              </p>
            )}
          </div>
        )}
      </div>
    </Dialog>
  )
}
