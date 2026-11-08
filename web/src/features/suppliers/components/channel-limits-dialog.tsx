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

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
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
import { toIntlLocale } from '@/i18n/languages'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { formatFixedUSD } from '@/lib/currency'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { getChannelLimits, saveChannelLimits, supplyQueryKey } from '../api'
import {
  channelLimitsEditorSchema,
  limitsFormValues,
  limitsPayload,
  type ChannelLimitsEditorForm,
} from '../lib/limits'
import type { ChannelLimits, SupplierMode } from '../types'
import { SupplierBudgetEvents } from './budget-events'
import { SupplierLimitEvents } from './limit-events'

interface Props {
  channelId: number
  channelName: string
  audience: SupplierMode | 'admin'
  onClose: () => void
}
export function ChannelLimitsDialog(props: Props) {
  const { t } = useTranslation()
  const userId = useAuthStore((s) => s.auth.user?.id)
  const query = useQuery({
    meta: { errorRedirect: false },
    refetchOnWindowFocus: false,
    queryKey: [
      ...supplyQueryKey,
      userId,
      props.audience,
      'limits',
      props.channelId,
    ],
    queryFn: ({ signal }) =>
      getChannelLimits(props.channelId, props.audience, signal),
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => !open && props.onClose()}
      title={`${t('Limits and daily budget')} · ${props.channelName}`}
      description={t(
        'Daily procurement costs use USD and reset at 00:00 Beijing time.'
      )}
    >
      {query.isPending && <LoadingState />}
      {query.isError && <ErrorState onRetry={() => void query.refetch()} />}
      {query.data && (
        <LimitsEditor
          key={query.data.config.revision}
          {...props}
          data={query.data}
        />
      )}
    </Dialog>
  )
}
function LimitsEditor(props: Props & { data: ChannelLimits }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const queryClient = useQueryClient()
  const user = useAuthStore((state) => state.auth.user)
  const readonly =
    props.audience === 'viewer' ||
    (props.audience === 'admin' &&
      !hasPermission(
        user,
        ADMIN_PERMISSION_RESOURCES.CHANNEL,
        ADMIN_PERMISSION_ACTIONS.WRITE
      ))
  const [budgetVisible, setBudgetVisible] = useState(false)
  const [historyVisible, setHistoryVisible] = useState(false)
  const form = useForm<ChannelLimitsEditorForm>({
    resolver: zodResolver(channelLimitsEditorSchema),
    defaultValues: {
      ...limitsFormValues(props.data.config),
      reconcile_seconds: String(props.data.config.reconcile_seconds),
    },
  })
  const mutation = useMutation({
    mutationFn: (values: ChannelLimitsEditorForm) => {
      if (readonly || props.audience === 'viewer') {
        throw new Error('Permission denied')
      }
      return saveChannelLimits(props.channelId, props.audience, {
        ...limitsPayload(values, props.data.config.revision),
        ...(props.audience === 'admin'
          ? { reconcile_seconds: Number(values.reconcile_seconds) }
          : {}),
      })
    },
    onSuccess: () => {
      toast.success(t('Saved successfully'))
      void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
      props.onClose()
    },
  })
  const usage = props.data.usage
  const tpmApplicable = props.data.config.tpm_applicable !== false
  const reason = usage.reason
  const money = (value: string | null) =>
    value == null
      ? t('Unknown')
      : `${formatFixedUSD(value, { locale, digitsSmall: 9, digitsLarge: 6, abbreviate: false })} USD`
  const status = limitReason(reason, t)
  return (
    <div className='space-y-4'>
      {status && (
        <Alert>
          <AlertDescription>{status}</AlertDescription>
        </Alert>
      )}
      <dl className='grid grid-cols-2 gap-3 text-sm sm:grid-cols-3'>
        <div>
          <dt className='text-muted-foreground'>{t('RPM usage')}</dt>
          <dd className='tabular-nums'>
            {formatNumber(usage.rpm_used, locale)}
          </dd>
        </div>
        <div>
          <dt className='text-muted-foreground'>{t('TPM usage')}</dt>
          <dd className='tabular-nums'>
            {tpmApplicable
              ? formatNumber(usage.tpm_used, locale)
              : t('Not applicable')}
          </dd>
        </div>
        <div>
          <dt className='text-muted-foreground'>{t('Reserved tokens')}</dt>
          <dd className='tabular-nums'>
            {tpmApplicable
              ? formatNumber(usage.tpm_reserved, locale)
              : t('Not applicable')}
          </dd>
        </div>
        <div>
          <dt className='text-muted-foreground'>
            {t('Daily procurement cost')}
          </dt>
          <dd className='tabular-nums'>{money(usage.daily_used_usd)}</dd>
        </div>
        <div>
          <dt className='text-muted-foreground'>{t('Reserved cost')}</dt>
          <dd className='tabular-nums'>{money(usage.daily_reserved_usd)}</dd>
        </div>
        <div>
          <dt className='text-muted-foreground'>{t('Remaining budget')}</dt>
          <dd className='tabular-nums'>
            {props.data.config.daily_usd == null
              ? t('Unlimited')
              : money(usage.daily_remaining_usd)}
          </dd>
        </div>
      </dl>
      <p className='text-muted-foreground text-xs'>
        {t('Budget date (Beijing time)')}: {usage.budget_day || '—'}
        {usage.reset_at > 0 && (
          <>
            {' '}
            · {t('Next budget reset')}:{' '}
            {new Intl.DateTimeFormat(locale, {
              timeZone: 'Asia/Shanghai',
              dateStyle: 'medium',
              timeStyle: 'short',
            }).format(new Date(usage.reset_at * 1000))}{' '}
            ({t('Beijing time')})
          </>
        )}
      </p>
      <Form {...form}>
        <form
          onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
          className='space-y-4'
        >
          <div className='grid gap-4 sm:grid-cols-3'>
            {(['rpm', 'tpm', 'daily_usd'] as const).map((name) => (
              <FormField
                key={name}
                control={form.control}
                name={name}
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>
                      {name === 'daily_usd'
                        ? t('Daily limit (USD)')
                        : name.toUpperCase()}
                    </FormLabel>
                    <FormControl>
                      <Input
                        {...field}
                        inputMode={name === 'daily_usd' ? 'decimal' : 'numeric'}
                        placeholder={
                          name === 'tpm' && !tpmApplicable
                            ? t('Not applicable')
                            : t('Unlimited')
                        }
                        disabled={
                          readonly ||
                          mutation.isPending ||
                          (name === 'tpm' && !tpmApplicable)
                        }
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            ))}
          </div>
          {!tpmApplicable && (
            <p className='text-muted-foreground text-xs'>
              {t(
                'TPM does not apply to this template. Images, video seconds and credits are counted separately.'
              )}
            </p>
          )}
          {props.audience === 'admin' && tpmApplicable && (
            <FormField
              control={form.control}
              name='reconcile_seconds'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>
                    {t('TPM reconciliation window (seconds)')}
                  </FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      inputMode='numeric'
                      disabled={readonly || mutation.isPending}
                    />
                  </FormControl>
                  <FormMessage />
                  <p className='text-muted-foreground text-xs'>
                    {t(
                      'New reconciliation cases use this duration. Active cases keep their original deadline.'
                    )}
                  </p>
                </FormItem>
              )}
            />
          )}
          <p className='text-muted-foreground text-sm'>
            {t(
              'Leave a limit blank for unlimited usage. Changing limits does not reset usage.'
            )}
          </p>
          {!readonly && !props.data.config.budget_ready && (
            <p className='text-muted-foreground text-sm'>
              {t(
                'A daily budget requires an administrator to configure procurement costs.'
              )}
            </p>
          )}
          <div className='flex justify-end gap-2'>
            <Button type='button' variant='outline' onClick={props.onClose}>
              {t('Close')}
            </Button>
            {!readonly && (
              <Button type='submit' disabled={mutation.isPending}>
                {mutation.isPending ? t('Saving...') : t('Save')}
              </Button>
            )}
          </div>
        </form>
      </Form>
      {props.audience !== 'viewer' && (
        <>
          <Button
            type='button'
            variant='outline'
            aria-expanded={historyVisible}
            onClick={() => setHistoryVisible(!historyVisible)}
          >
            {t('Limit history')}
          </Button>
          {historyVisible && (
            <SupplierLimitEvents
              channelId={props.channelId}
              audience={props.audience}
            />
          )}
        </>
      )}
      {props.audience === 'supplier' && (
        <>
          <Button
            type='button'
            variant='outline'
            aria-expanded={budgetVisible}
            onClick={() => setBudgetVisible(!budgetVisible)}
          >
            {t('Budget details')}
          </Button>
          {budgetVisible && (
            <SupplierBudgetEvents
              channelId={props.channelId}
              day={usage.budget_day}
            />
          )}
        </>
      )}
    </div>
  )
}
function limitReason(reason: string, t: (key: string) => string): string {
  switch (reason) {
    case 'rpm_limited':
      return t('RPM limit reached')
    case 'tpm_limited':
      return t('TPM limit reached')
    case 'daily_exhausted':
      return t(
        'Daily budget exhausted. The budget resets at 00:00 Beijing time.'
      )
    case 'budget_reserved':
      return t('Budget is reserved by pending requests')
    case 'cost_config_missing':
      return t(
        'Waiting for an administrator to configure procurement costs. The daily limit is not active yet.'
      )
    case 'accounting_unavailable':
      return t('Usage accounting is temporarily unavailable')
    default:
      return ''
  }
}
