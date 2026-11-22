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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import type { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { MultiSelect } from '@/components/multi-select'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  SecureVerificationDialog,
  useSecureVerification,
} from '@/features/auth/secure-verification'
import { getGroups } from '@/features/users/api'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import {
  getAdminSupplyChannel,
  getSuppliers,
  getSupplierTemplates,
  publishSupplierChannel,
  supplyQueryKey,
  testSupplyChannel,
  updateSupplierChannelRouting,
} from '../api'
import { supplierRoutingSchema as schema } from '../lib/forms'
import { supplierChannelTemplates } from '../lib/templates'
import type { SupplyChannel } from '../types'
import { ChannelEditor } from './channel-editor'
import { SupplierChannelTestStatus } from './channel-test-status'
import { CloseSupplierChannelDraftDialog } from './close-channel-draft-dialog'

export function AdminSupplierChannelEditor(props: {
  channelId: number
  onClose: () => void
}) {
  const { t } = useTranslation()
  const userId = useAuthStore((state) => state.auth.user?.id)
  const channel = useQuery({
    meta: { errorRedirect: false, errorToast: false },
    queryKey: [...supplyQueryKey, userId, 'admin', 'channel', props.channelId],
    queryFn: ({ signal }) => getAdminSupplyChannel(props.channelId, signal),
    refetchInterval: (query) =>
      ['pending', 'testing', 'waiting'].includes(
        query.state.data?.validation_status ?? ''
      )
        ? 3000
        : false,
  })
  if (channel.data) {
    return (
      <AdminSupplierChannelDetails
        key={channel.data.id}
        channel={channel.data}
        onClose={props.onClose}
      />
    )
  }
  return (
    <Dialog
      open
      title={t('Review supplier channel')}
      onOpenChange={(open) => !open && props.onClose()}
    >
      {channel.isPending ? (
        <LoadingState />
      ) : (
        <ErrorState onRetry={() => void channel.refetch()} />
      )}
    </Dialog>
  )
}

function AdminSupplierChannelDetails(props: {
  channel: SupplyChannel
  onClose: () => void
}) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const queryClient = useQueryClient()
  const [action, setAction] = useState<'publish' | 'routing' | 'edit' | null>(
    null
  )
  const [closeDraft, setCloseDraft] = useState(false)
  const channel = props.channel
  const activeTest = [
    'pending',
    'testing',
    'waiting',
    'result_unknown',
  ].includes(channel.validation_status)
  const canWrite = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.WRITE
  )
  const canOperate = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.OPERATE
  )
  const canEditSensitive = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.SENSITIVE_WRITE
  )
  const canClose =
    !channel.test_only &&
    !activeTest &&
    !['closed', 'validated'].includes(channel.validation_status)
  const canPublish =
    channel.validation_status === 'ready_for_review' &&
    !channel.test_only &&
    canWrite &&
    canOperate &&
    canEditSensitive
  const suppliers = useQuery({
    meta: { errorRedirect: false, errorToast: false },
    queryKey: [...supplyQueryKey, user?.id, 'admin', 'accounts'],
    queryFn: ({ signal }) => getSuppliers(signal),
    enabled: action === 'edit',
  })
  const templates = useQuery({
    meta: { errorRedirect: false, errorToast: false },
    queryKey: [...supplyQueryKey, user?.id, 'admin', 'templates'],
    queryFn: ({ signal }) => getSupplierTemplates(signal),
    enabled: action === 'edit',
  })
  const supplier = suppliers.data?.items.find(
    (item) => item.id === channel.supplier_id
  )
  const test = useMutation({
    mutationFn: () => testSupplyChannel(channel, 'admin'),
    onSuccess: () => {
      toast.success(t('Channel test requested'))
      void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
      void queryClient.invalidateQueries({ queryKey: ['channels'] })
    },
  })

  if (action === 'edit') {
    if (supplier && templates.data) {
      return (
        <ChannelEditor
          audience='admin'
          channel={channel}
          templates={supplierChannelTemplates(supplier, templates.data)}
          onClose={() => setAction(null)}
        />
      )
    }
    return (
      <Dialog
        open
        title={t('Submit update')}
        onOpenChange={(open) => !open && setAction(null)}
      >
        {suppliers.isPending || templates.isPending ? (
          <LoadingState />
        ) : (
          <ErrorState
            onRetry={() =>
              void Promise.all([suppliers.refetch(), templates.refetch()])
            }
          />
        )}
      </Dialog>
    )
  }
  if (action === 'publish' || action === 'routing') {
    return (
      <SupplierRoutingEditor
        key={action}
        channel={channel}
        mode={action}
        onClose={() => setAction(null)}
      />
    )
  }
  return (
    <>
      <Dialog
        open
        title={t('Review supplier channel')}
        description={channel.name}
        onOpenChange={(open) => !open && props.onClose()}
      >
        <dl className='grid gap-3 text-sm sm:grid-cols-2'>
          <div>
            <dt className='text-muted-foreground'>{t('Test result')}</dt>
            <dd>
              <SupplierChannelTestStatus status={channel.validation_status} />
            </dd>
          </div>
          <div>
            <dt className='text-muted-foreground'>{t('Channel Supplier')}</dt>
            <dd>{channel.supplier_name}</dd>
          </div>

          <div>
            <dt className='text-muted-foreground'>{t('Base URL')}</dt>
            <dd className='break-all'>{channel.base_url}</dd>
          </div>
          <div>
            <dt className='text-muted-foreground'>{t('Group')}</dt>
            <dd>{channel.group || '—'}</dd>
          </div>
          <div>
            <dt className='text-muted-foreground'>{t('Published models')}</dt>
            <dd className='break-all'>
              {channel.live_models?.join(', ') || '—'}
            </dd>
          </div>
          <div>
            <dt className='text-muted-foreground'>{t('Tested models')}</dt>
            <dd className='break-all'>
              {channel.tested_models?.join(', ') || '—'}
            </dd>
          </div>
        </dl>
        <div className='flex flex-wrap justify-end gap-2'>
          {canClose && canWrite && (
            <Button variant='outline' onClick={() => setCloseDraft(true)}>
              {t('Close draft')}
            </Button>
          )}
          {canEditSensitive &&
            (channel.validation_status !== 'closed' ||
              channel.has_validated_config) && (
              <Button variant='outline' onClick={() => setAction('edit')}>
                {t('Submit update')}
              </Button>
            )}
          {canOperate &&
            !activeTest &&
            channel.validation_status !== 'closed' && (
              <Button
                variant='outline'
                disabled={test.isPending}
                onClick={() => test.mutate()}
              >
                {t('Test')}
              </Button>
            )}
          {canWrite && channel.has_validated_config && (
            <Button variant='outline' onClick={() => setAction('routing')}>
              {t('Channel routing details')}
            </Button>
          )}
          {canPublish && (
            <Button onClick={() => setAction('publish')}>
              {channel.has_validated_config
                ? t('Publish update')
                : t('Review and publish')}
            </Button>
          )}
        </div>
      </Dialog>
      {closeDraft && (
        <CloseSupplierChannelDraftDialog
          channel={channel}
          onClose={() => setCloseDraft(false)}
        />
      )}
    </>
  )
}

function SupplierRoutingEditor(props: {
  channel: SupplyChannel
  mode: 'publish' | 'routing'
  onClose: () => void
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const verification = useSecureVerification()
  const channel = props.channel
  const publishing = props.mode === 'publish'
  const title = publishing
    ? t('Review and publish')
    : t('Channel routing details')
  const testedModels = channel.tested_models ?? []
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: {
      models: testedModels,
      groups: channel.group?.split(',').filter(Boolean) ?? [],
      priority: String(channel.priority ?? 0),
      weight: String(channel.channel_weight ?? 0),
    },
  })
  const groups = useQuery({
    queryKey: ['groups'],
    queryFn: async () => requireServerSuccess(await getGroups()),
  })
  const mutation = useMutation({
    mutationFn: async (values: z.infer<typeof schema>) => {
      const routing = {
        revision: channel.publication_revision ?? '',
        groups: values.groups,
        priority: Number(values.priority),
        weight: Number(values.weight),
      }
      if (publishing) {
        if (values.models.length === 0) {
          form.setError('models', { message: t('Select at least one model') })
          return false
        }
        const payload = {
          ...routing,
          version: channel.version,
          models: values.models,
        }
        const proof = await verification.requestVerification({
          scope: 'supplier.channel.publish',
          context: { channel_id: channel.id, ...payload },
          title,
        })
        if (!proof) return false
        await publishSupplierChannel(channel.id, payload, proof.proof_token)
      } else {
        await updateSupplierChannelRouting(channel.id, routing)
      }
      return true
    },
    onSuccess: async (saved) => {
      if (!saved) return
      toast.success(t('Saved successfully'))
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: supplyQueryKey }),
        queryClient.invalidateQueries({ queryKey: ['channels'] }),
      ])
      props.onClose()
    },
  })

  return (
    <>
      <Dialog
        open={!verification.isActive}
        title={title}
        description={channel.name}
        onOpenChange={(open) => !open && !mutation.isPending && props.onClose()}
      >
        {groups.isError && <ErrorState onRetry={() => void groups.refetch()} />}
        <Form {...form}>
          <form
            className='space-y-4'
            onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
          >
            <FormField
              control={form.control}
              name='groups'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Groups')}</FormLabel>
                  <FormControl>
                    <MultiSelect
                      aria-label={t('Groups')}
                      options={(groups.data?.data ?? []).map((group) => ({
                        value: group,
                        label: group,
                      }))}
                      selected={field.value}
                      onChange={field.onChange}
                      disabled={
                        groups.isPending || groups.isError || mutation.isPending
                      }
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            {publishing && (
              <FormField
                control={form.control}
                name='models'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Tested models')}</FormLabel>
                    <FormControl>
                      <MultiSelect
                        aria-label={t('Tested models')}
                        options={testedModels.map((model) => ({
                          value: model,
                          label: model,
                        }))}
                        selected={field.value}
                        onChange={field.onChange}
                        disabled={mutation.isPending}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            )}
            <div className='grid gap-3 sm:grid-cols-2'>
              {(
                [
                  ['priority', t('Priority')],
                  ['weight', t('Weight')],
                ] as const
              ).map(([name, label]) => (
                <FormField
                  name={name}
                  control={form.control}
                  key={name}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{label}</FormLabel>
                      <FormControl>
                        <Input
                          {...field}
                          inputMode='numeric'
                          disabled={mutation.isPending}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              ))}
            </div>
            <div className='flex justify-end gap-2'>
              <Button
                type='button'
                variant='outline'
                disabled={mutation.isPending}
                onClick={props.onClose}
              >
                {t('Cancel')}
              </Button>
              <Button
                type='submit'
                disabled={
                  mutation.isPending || groups.isPending || groups.isError
                }
              >
                {publishing ? t('Publish') : t('Save')}
              </Button>
            </div>
          </form>
        </Form>
      </Dialog>
      <SecureVerificationDialog {...verification.dialogProps} />
    </>
  )
}
