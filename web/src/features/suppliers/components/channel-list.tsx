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
import type { ColumnDef } from '@tanstack/react-table'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { DataTablePage, useDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useAuthStore } from '@/stores/auth-store'

import {
  getSupplyChannels,
  discardSupplyChannel,
  setSupplyChannelPaused,
  testSupplyChannel,
  supplyQueryKey,
} from '../api'
import type { SupplierMode, SupplierTemplate, SupplyChannel } from '../types'
import { ChannelEditor } from './channel-editor'
import { ChannelLimitsDialog } from './channel-limits-dialog'
import { CopySupplyChannelDialog } from './copy-channel-dialog'

interface Props {
  mode: SupplierMode
  templates?: SupplierTemplate[]
}
export function SupplyChannelList(props: Props) {
  const { t } = useTranslation()
  const userId = useAuthStore((s) => s.auth.user?.id)
  const queryClient = useQueryClient()
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const [draftSearch, setDraftSearch] = useState('')
  const [keyword, setKeyword] = useState('')
  const [editing, setEditing] = useState<SupplyChannel | 'new' | null>(null)
  const [copying, setCopying] = useState<SupplyChannel | null>(null)
  const [detailSnapshot, setDetail] = useState<SupplyChannel | null>(null)
  const [limits, setLimits] = useState<SupplyChannel | null>(null)
  const [pausing, setPausing] = useState<SupplyChannel | null>(null)
  const [discarding, setDiscarding] = useState<SupplyChannel | null>(null)
  const query = useQuery<Awaited<ReturnType<typeof getSupplyChannels>>>({
    queryKey: [
      ...supplyQueryKey,
      userId,
      props.mode,
      'channels',
      pagination,
      keyword,
    ],
    refetchInterval: (query) =>
      query.state.data?.items.some((channel) =>
        ['configuring', 'pending', 'testing', 'waiting'].includes(
          channel.validation_status
        )
      )
        ? 5000
        : 15000,
    queryFn: ({ signal }) =>
      getSupplyChannels(
        props.mode,
        {
          p: pagination.pageIndex + 1,
          page_size: pagination.pageSize,
          keyword,
        },
        signal
      ),
  })
  const detail =
    query.data?.items.find((channel) => channel.id === detailSnapshot?.id) ??
    detailSnapshot
  const statusMutation = useMutation({
    mutationFn: (channel: SupplyChannel) =>
      setSupplyChannelPaused(channel, !channel.paused),
    onSuccess: () => {
      setPausing(null)
      toast.success(t('Saved successfully'))
      void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
    },
  })
  const discardMutation = useMutation({
    mutationFn: discardSupplyChannel,
    onSuccess: () => {
      setDiscarding(null)
      toast.success(t('Draft discarded'))
      void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
    },
  })
  const testMutation = useMutation({
    mutationFn: testSupplyChannel,
    onSuccess: () => {
      toast.success(t('Channel test requested'))
      void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
    },
  })
  const columns = useMemo<ColumnDef<SupplyChannel>[]>(
    () => [
      {
        accessorKey: 'name',
        header: t('Name'),
        cell: ({ row }) => (
          <Button
            variant='link'
            className='max-w-full truncate px-0'
            onClick={() => setDetail(row.original)}
          >
            {row.original.name}
          </Button>
        ),
      },
      ...(props.mode === 'viewer'
        ? [
            {
              accessorKey: 'supplier_name',
              header: t('Channel Supplier'),
              cell: ({ row }: { row: { original: SupplyChannel } }) =>
                row.original.supplier_name || t('Unassigned'),
            },
          ]
        : []),
      {
        accessorKey: 'models',
        header: t('Models'),
        cell: ({ row }) => (
          <span className='line-clamp-2 max-w-64 break-all'>
            {row.original.models.join(', ')}
          </span>
        ),
      },
      {
        accessorKey: 'status',
        header: t('Status'),
        cell: ({ row }) => (
          <div className='flex flex-wrap gap-1'>
            {channelStates(row.original, t).map((label) => (
              <Badge key={label} variant='outline'>
                {label}
              </Badge>
            ))}
          </div>
        ),
      },
      {
        accessorKey: 'validation_status',
        header: t('Configuration validation'),
        cell: ({ row }) => validationState(row.original.validation_status, t),
      },
      { accessorKey: 'priority', header: t('Supplier internal priority') },
      {
        id: 'actions',
        header: t('Actions'),
        cell: ({ row }) => (
          <div className='flex flex-wrap gap-1'>
            <Button
              size='sm'
              variant='outline'
              onClick={() => setLimits(row.original)}
            >
              {t('Limits')}
            </Button>
            {props.mode === 'supplier' && (
              <>
                <Button
                  size='sm'
                  variant='ghost'
                  disabled={
                    !(
                      row.original.has_validated_config ??
                      row.original.validation_status === 'validated'
                    )
                  }
                  onClick={() => setCopying(row.original)}
                >
                  {t('Copy Channel')}
                </Button>
                <Button
                  size='sm'
                  variant='ghost'
                  onClick={() => setEditing(row.original)}
                >
                  {t('Edit')}
                </Button>
                <Button
                  size='sm'
                  variant='ghost'
                  disabled={statusMutation.isPending}
                  onClick={() => setPausing(row.original)}
                >
                  {row.original.paused ? t('Resume') : t('Pause')}
                </Button>
                <Button
                  size='sm'
                  variant='ghost'
                  disabled={
                    testMutation.isPending ||
                    ['testing', 'waiting'].includes(
                      row.original.validation_status
                    )
                  }
                  onClick={() => testMutation.mutate(row.original)}
                >
                  {t('Test')}
                </Button>
                {row.original.has_validated_config === false &&
                  !['testing', 'waiting', 'result_unknown'].includes(
                    row.original.validation_status
                  ) && (
                    <Button
                      size='sm'
                      variant='ghost'
                      disabled={discardMutation.isPending}
                      onClick={() => setDiscarding(row.original)}
                    >
                      {t('Discard draft')}
                    </Button>
                  )}
              </>
            )}
          </div>
        ),
      },
    ],
    [
      props.mode,
      statusMutation.isPending,
      discardMutation.isPending,
      t,
      testMutation,
    ]
  )
  const { table } = useDataTable({
    data: query.data?.items ?? [],
    columns,
    pagination,
    onPaginationChange: setPagination,
    totalCount: query.data?.total ?? 0,
    manualPagination: true,
    enableRowSelection: false,
    columnVisibilityStorageKey: false,
    columnSizingStorageKey: false,
  })
  if (query.isError) return <ErrorState onRetry={() => void query.refetch()} />
  return (
    <>
      <DataTablePage
        table={table}
        columns={columns}
        isLoading={query.isPending}
        isFetching={query.isFetching}
        fixedHeight={false}
        emptyTitle={t('No channels')}
        emptyDescription={
          props.mode === 'supplier'
            ? t('Add a channel to start supplying models.')
            : undefined
        }
        toolbar={
          <div className='flex flex-wrap items-center justify-between gap-2'>
            <form
              className='flex min-w-0 items-center gap-2'
              onSubmit={(event) => {
                event.preventDefault()
                setKeyword(draftSearch)
                setPagination((p) => ({ ...p, pageIndex: 0 }))
              }}
            >
              <Input
                aria-label={t('Search channels')}
                value={draftSearch}
                onChange={(event) => setDraftSearch(event.target.value)}
                placeholder={t('Search channels')}
              />
              <Button type='submit' variant='outline'>
                {t('Search')}
              </Button>
            </form>
            <div className='flex gap-2'>
              <Button
                variant='outline'
                onClick={() => void query.refetch()}
                disabled={query.isFetching}
              >
                {t('Refresh')}
              </Button>
              {props.mode === 'supplier' && (
                <Button
                  onClick={() => setEditing('new')}
                  disabled={!props.templates?.length}
                >
                  {t('Add Channel')}
                </Button>
              )}
            </div>
          </div>
        }
      />
      {copying && (
        <CopySupplyChannelDialog
          channel={copying}
          onClose={() => setCopying(null)}
        />
      )}
      {editing && (
        <ChannelEditor
          key={editing === 'new' ? 'new' : editing.id}
          channel={editing === 'new' ? undefined : editing}
          templates={props.templates ?? []}
          onClose={() => setEditing(null)}
        />
      )}
      {limits && (
        <ChannelLimitsDialog
          channelId={limits.id}
          channelName={limits.name}
          audience={props.mode}
          onClose={() => setLimits(null)}
        />
      )}
      {detail && (
        <Dialog
          open
          onOpenChange={(open) => !open && setDetail(null)}
          title={detail.name}
        >
          <dl className='grid gap-3 text-sm'>
            <div>
              <dt className='text-muted-foreground'>{t('Channel Supplier')}</dt>
              <dd>{detail.supplier_name || t('Unassigned')}</dd>
            </div>
            <div>
              <dt className='text-muted-foreground'>{t('Base URL')}</dt>
              <dd className='break-all'>{detail.base_url}</dd>
            </div>
            <div>
              <dt className='text-muted-foreground'>{t('Models')}</dt>
              <dd className='break-all'>{detail.models.join(', ')}</dd>
            </div>
            <div>
              <dt className='text-muted-foreground'>{t('Status')}</dt>
              <dd>{channelStates(detail, t).join(' · ')}</dd>
            </div>
            <div>
              <dt className='text-muted-foreground'>
                {t('Configuration validation')}
              </dt>
              <dd>{validationState(detail.validation_status, t)}</dd>
            </div>
            {props.mode === 'viewer' && (
              <>
                <div>
                  <dt className='text-muted-foreground'>{t('Group')}</dt>
                  <dd>{detail.group || '—'}</dd>
                </div>
                <div>
                  <dt className='text-muted-foreground'>
                    {t('Platform pool priority')}
                  </dt>
                  <dd>{detail.platform_priority ?? 0}</dd>
                </div>
                <div>
                  <dt className='text-muted-foreground'>
                    {t('Platform pool weight')}
                  </dt>
                  <dd>{detail.weight ?? 0}</dd>
                </div>
              </>
            )}
            <div>
              <dt className='text-muted-foreground'>{t('Credentials')}</dt>
              <dd>
                {detail.has_key
                  ? t('Configured (write-only)')
                  : t('Not configured')}
              </dd>
            </div>
            <div>
              <dt className='text-muted-foreground'>{t('Remark')}</dt>
              <dd className='break-words whitespace-pre-wrap'>
                {detail.remark || '—'}
              </dd>
            </div>
          </dl>
        </Dialog>
      )}
      <ConfirmDialog
        open={discarding !== null}
        onOpenChange={(open) => !open && setDiscarding(null)}
        title={t('Discard draft')}
        desc={t(
          'Only an unused draft can be discarded. Channels that have sent upstream requests are retained.'
        )}
        confirmText={t('Discard')}
        destructive
        isLoading={discardMutation.isPending}
        handleConfirm={() => discarding && discardMutation.mutate(discarding)}
      />
      <ConfirmDialog
        open={pausing !== null}
        onOpenChange={(open) => !open && setPausing(null)}
        title={pausing?.paused ? t('Resume channel') : t('Pause channel')}
        desc={
          <>
            <p>
              {t(
                'Only your manual pause is changed. Other supply restrictions remain in effect.'
              )}
            </p>
            {!pausing?.paused && (
              <p>
                {t(
                  'Pausing closes active realtime connections. Submitted tasks continue to be checked for results.'
                )}
              </p>
            )}
          </>
        }
        isLoading={statusMutation.isPending}
        handleConfirm={() => pausing && statusMutation.mutate(pausing)}
      />
    </>
  )
}
function channelStates(
  channel: SupplyChannel,
  t: (key: string) => string
): string[] {
  const reasons = channel.blocking_reasons?.length
    ? channel.blocking_reasons
    : [channel.status]
  return reasons.map((reason) => channelState(reason, t))
}
function channelState(status: string, t: (key: string) => string): string {
  switch (status) {
    case 'available':
    case 'enabled':
      return t('Available')
    case 'pending':
      return t('Pending validation')
    case 'paused':
      return t('Paused')
    case 'manual_paused':
      return t('Paused by an administrator')
    case 'health_disabled':
      return t('Disabled by health checks')
    case 'template_unavailable':
      return t('Channel template unavailable')
    case 'authorization_removed':
      return t('Channel authorization withdrawn')
    case 'supply_paused':
      return t('Supply paused')
    case 'outside_schedule':
      return t('Outside availability schedule')
    case 'rpm_limited':
      return t('RPM limit reached')
    case 'tpm_limited':
      return t('TPM limit reached')
    case 'daily_exhausted':
      return t('Daily budget exhausted')
    case 'budget_reserved':
      return t('Budget is reserved by pending requests')
    case 'cost_config_missing':
      return t('Waiting for cost configuration')
    case 'accounting_unavailable':
      return t('Usage accounting is temporarily unavailable')
    default:
      return t('Unavailable')
  }
}
function validationState(status: string, t: (key: string) => string): string {
  switch (status) {
    case 'validated':
      return t('Validated')
    case 'failed':
      return t('Validation failed')
    case 'testing':
      return t('Testing')
    case 'waiting':
      return t('Waiting for task completion')
    case 'result_unknown':
      return t('Validation result unknown')
    case 'cost_config_missing':
      return t('Waiting for cost configuration')
    case 'pending':
    case 'configuring':
      return t('Pending validation')
    default:
      return t('Unknown')
  }
}
