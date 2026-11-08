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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { hasPermission } from '@/lib/admin-permissions'
import { api } from '@/lib/api'
import { formatNumber, formatTimestampToDate } from '@/lib/format'
import { handleServerError } from '@/lib/handle-server-error'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import type { Channel } from '../types'

type FundingEvent = {
  id: number
  revision: number
  state: string
  category: string
  key_index: number
  key_alias: string
  model: string
  endpoint: string
  source: string
  status_code: number
  error_code: string
  evidence: string
  first_seen: number
  last_seen: number
  hits: number
  auto_disabled: boolean
  probe_status: string
  probe_detail: string
  next_probe_at: number
  last_probe_at: number
  resolution: string
}
type FundingDelivery = {
  id: number
  incident_id: number
  kind: string
  status: string
  attempts: number
  next_attempt_at: number
  sent_at: number
}
type FundingData = {
  incidents: FundingEvent[]
  deliveries: FundingDelivery[]
  monitoring_enabled: boolean
}
const categoryLabels: Record<string, string> = {
  balance_insufficient: 'Insufficient funding',
  quota_limited: 'Spending limit reached',
  suspected: 'Funding review needed',
}
const statusLabels: Record<string, string> = {
  suspected: 'Funding review needed',
  confirmed: 'Confirmed',
  resolved: 'Recovered',
  dismissed: 'Cleared',
  pending: 'Pending',
  running: 'Running',
  failed: 'Failed',
  paused: 'Paused',
  blocked: 'Blocked',
  manual_required: 'Manual review required',
  recovered: 'Recovered',
  cancelled: 'Cancelled',
  sent: 'Sent',
  sending: 'Sending',
  missing_address: 'No notification address',
  limited: 'Rate limited',
  initial: 'Initial notification',
  reminder: 'Reminder',
  recovery: 'Recovery notification',
}

export function FundingStatusBadge({
  funding,
}: {
  funding: NonNullable<Channel['funding_status']>
}) {
  const { t } = useTranslation()
  const status = t(categoryLabels[funding.status])
  const label =
    funding.total_keys > 1
      ? t('{{status}} · {{affected}}/{{total}} keys', {
          status,
          affected: formatNumber(funding.affected_keys),
          total: formatNumber(funding.total_keys),
        })
      : status
  return (
    <StatusBadge
      label={label}
      variant={funding.status === 'suspected' ? 'warning' : 'danger'}
      copyable={false}
    />
  )
}

export function ChannelFundingBadge({ channel }: { channel: Channel }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [dismiss, setDismiss] = useState<FundingEvent | null>(null)
  const [reason, setReason] = useState('')
  const user = useAuthStore((state) => state.auth.user)
  const canOperate = hasPermission(user, 'channel', 'operate')
  const client = useQueryClient()
  const queryKey = ['channel-funding', channel.id]
  const query = useQuery({
    queryKey,
    enabled: open,
    queryFn: async ({ signal }) => {
      const response = await api.get<{ success: boolean; data: FundingData }>(
        `/api/channel/${channel.id}/funding`,
        { signal, disableDuplicate: true }
      )
      return requireServerSuccess(response.data).data
    },
    refetchInterval: open ? 5000 : false,
  })
  const mutation = useMutation({
    mutationFn: async ({
      event,
      action,
    }: {
      event: FundingEvent
      action: 'probe' | 'dismiss'
    }) => {
      const response = await api.post<{
        success: boolean
        data?: { task_id: string }
      }>(
        `/api/channel/${channel.id}/funding/${action}`,
        { incident_id: event.id, revision: event.revision, reason },
        { skipBusinessError: true, skipErrorHandler: true }
      )
      return { ...requireServerSuccess(response.data), action }
    },
    onSuccess: (result) => {
      if (result.action === 'probe') {
        toast.success(
          t('Funding check queued: {{id}}', { id: result.data?.task_id })
        )
      } else {
        setDismiss(null)
        setReason('')
        toast.success(t('Funding marker cleared'))
      }
      void client.invalidateQueries({
        queryKey: ['channel-funding', channel.id],
      })
      void client.invalidateQueries({ queryKey: ['channels'] })
    },
    onError: (error) => {
      handleServerError(error)
      void query.refetch()
    },
    meta: { errorToast: false },
  })
  const funding = channel.funding_status
  return (
    <>
      <Button
        variant='ghost'
        size='sm'
        onClick={(event) => {
          event.stopPropagation()
          setOpen(true)
        }}
        aria-label={t('Funding status')}
      >
        {funding ? (
          <FundingStatusBadge funding={funding} />
        ) : (
          <span className='text-muted-foreground'>{t('View')}</span>
        )}
      </Button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={t('Funding status')}
        description={channel.name}
      >
        <div className='space-y-4'>
          {query.isPending && <p role='status'>{t('Loading...')}</p>}
          {query.isError && (
            <div role='alert' className='space-y-2'>
              <p>{t('Failed to load funding events')}</p>
              <Button variant='outline' onClick={() => void query.refetch()}>
                {t('Retry')}
              </Button>
            </div>
          )}
          {query.data && (
            <>
              {!query.data.monitoring_enabled && (
                <p className='text-muted-foreground text-sm'>
                  {t('Channel funding monitoring is disabled')}
                </p>
              )}
              {query.data.incidents.length === 0 && (
                <p className='text-muted-foreground text-sm'>
                  {t('No active funding event')}
                </p>
              )}
              {query.data.incidents.map((event) => {
                const active =
                  event.state === 'confirmed' || event.state === 'suspected'
                const deliveries = query.data.deliveries.filter(
                  (delivery) => delivery.incident_id === event.id
                )
                return (
                  <article
                    key={event.id}
                    className='space-y-3 rounded-lg border p-4'
                  >
                    <div className='flex flex-wrap items-center justify-between gap-2'>
                      <h4 className='text-sm font-medium'>
                        {t(
                          categoryLabels[event.category] ??
                            'Funding review needed'
                        )}{' '}
                        · #{event.id} ·{' '}
                        {event.key_alias ||
                          t('Key {{index}}', { index: event.key_index + 1 })}
                      </h4>
                      <StatusBadge
                        label={t(statusLabels[event.state] ?? 'Unknown')}
                        variant={active ? 'warning' : 'neutral'}
                        copyable={false}
                      />
                    </div>
                    <p className='text-muted-foreground text-sm'>
                      {event.model} {event.endpoint} · HTTP {event.status_code}{' '}
                      {event.error_code}
                    </p>
                    <p className='text-sm break-all whitespace-pre-wrap'>
                      {event.evidence}
                    </p>
                    <dl className='grid grid-cols-1 gap-2 text-sm sm:grid-cols-2'>
                      <div>
                        <dt className='text-muted-foreground'>
                          {t('First detected')}
                        </dt>
                        <dd>{formatTimestampToDate(event.first_seen)}</dd>
                      </div>
                      <div>
                        <dt className='text-muted-foreground'>
                          {t('Last detected')}
                        </dt>
                        <dd>{formatTimestampToDate(event.last_seen)}</dd>
                      </div>
                      <div>
                        <dt className='text-muted-foreground'>
                          {t('Disabled by this event')}
                        </dt>
                        <dd>{event.auto_disabled ? t('Yes') : t('No')}</dd>
                      </div>
                      <div>
                        <dt className='text-muted-foreground'>
                          {t('Recovery check')}
                        </dt>
                        <dd>
                          {t(statusLabels[event.probe_status] ?? 'Unknown')}
                        </dd>
                      </div>
                      <div>
                        <dt className='text-muted-foreground'>
                          {t('Last check')}
                        </dt>
                        <dd>
                          {event.last_probe_at
                            ? formatTimestampToDate(event.last_probe_at)
                            : '—'}
                        </dd>
                      </div>
                      <div>
                        <dt className='text-muted-foreground'>
                          {t('Next check')}
                        </dt>
                        <dd>
                          {event.next_probe_at
                            ? formatTimestampToDate(event.next_probe_at)
                            : '—'}
                        </dd>
                      </div>
                    </dl>
                    {event.probe_detail && (
                      <p className='text-muted-foreground text-sm'>
                        {t(event.probe_detail)}
                      </p>
                    )}
                    {event.resolution && (
                      <p className='text-muted-foreground text-sm'>
                        {t(event.resolution)}
                      </p>
                    )}
                    <div className='space-y-1 text-sm'>
                      <h5 className='font-medium'>
                        {t('Notification delivery')}
                      </h5>
                      {deliveries.length ? (
                        deliveries.map((delivery) => (
                          <p
                            key={delivery.id}
                            className='text-muted-foreground'
                          >
                            {t(statusLabels[delivery.kind] ?? 'Unknown')} ·{' '}
                            {t(statusLabels[delivery.status] ?? 'Unknown')} ·{' '}
                            {t('Attempt {{count}} of 5', {
                              count: formatNumber(delivery.attempts),
                            })}
                            {delivery.sent_at
                              ? ` · ${formatTimestampToDate(delivery.sent_at)}`
                              : ''}
                          </p>
                        ))
                      ) : (
                        <p className='text-muted-foreground'>
                          {t('No notification queued')}
                        </p>
                      )}
                    </div>
                    {active && canOperate && (
                      <div className='flex flex-wrap gap-2'>
                        <Button
                          size='sm'
                          variant='outline'
                          disabled={
                            mutation.isPending ||
                            event.probe_status === 'running' ||
                            !query.data.monitoring_enabled
                          }
                          onClick={() =>
                            mutation.mutate({ event, action: 'probe' })
                          }
                        >
                          {t('Check recovery now')}
                        </Button>
                        <Button
                          size='sm'
                          variant='outline'
                          disabled={mutation.isPending}
                          onClick={() => {
                            setReason('')
                            setDismiss(event)
                          }}
                        >
                          {t('Clear funding marker')}
                        </Button>
                      </div>
                    )}
                  </article>
                )
              })}
            </>
          )}
        </div>
      </Dialog>
      <ConfirmDialog
        open={dismiss !== null}
        onOpenChange={(value) => {
          if (!value) setDismiss(null)
        }}
        title={t('Clear funding marker')}
        desc={t(
          'Clearing this marker does not enable the channel. New upstream errors can create another event.'
        )}
        confirmText={t('Clear funding marker')}
        disabled={!reason.trim()}
        isLoading={mutation.isPending}
        handleConfirm={() => {
          if (dismiss) mutation.mutate({ event: dismiss, action: 'dismiss' })
        }}
      >
        <div className='space-y-2'>
          <Label htmlFor={`funding-reason-${channel.id}`}>{t('Reason')}</Label>
          <Textarea
            id={`funding-reason-${channel.id}`}
            value={reason}
            onChange={(event) => setReason(event.target.value)}
            maxLength={500}
          />
        </div>
      </ConfirmDialog>
    </>
  )
}
