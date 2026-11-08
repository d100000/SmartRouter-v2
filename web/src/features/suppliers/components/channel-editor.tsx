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
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import type { z } from 'zod'

import { Dialog } from '@/components/dialog'
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import {
  SecureVerificationDialog,
  useSecureVerification,
} from '@/features/auth/secure-verification'

import { saveSupplyChannel, supplyQueryKey } from '../api'
import { supplierChannelSchema as schema } from '../lib/forms'
import { limitsPayload } from '../lib/limits'
import type { SupplierTemplate, SupplyChannel } from '../types'

type Values = z.infer<typeof schema>
interface Props {
  audience?: 'supplier' | 'admin'
  supplierId?: string
  channel?: SupplyChannel
  templates: SupplierTemplate[]
  onClose: () => void
}
export function ChannelEditor(props: Props) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const verification = useSecureVerification()
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: props.channel?.name ?? '',
      template_id:
        props.channel?.template_id ??
        String(props.channel?.type ?? props.templates[0]?.id ?? ''),
      base_url: props.channel?.base_url ?? '',
      key: '',
      models: props.channel?.models ?? [],
      remark: props.channel?.remark ?? '',
      priority: String(props.channel?.priority ?? 0),
      rpm: '',
      tpm: '',
      daily_usd: '',
    },
  })
  const selectedTemplate = props.templates.find(
    (template) => template.id === form.watch('template_id')
  )
  const tpmApplicable =
    selectedTemplate?.tpm_applicable ?? props.channel?.tpm_applicable ?? true
  const templateOptions = props.templates.map((template) => ({
    value: template.id,
    label: template.name,
  }))
  if (props.channel && !selectedTemplate) {
    const id = props.channel.template_id ?? String(props.channel.type)
    templateOptions.push({ value: id, label: id })
  }
  const mutation = useMutation({
    mutationFn: async () => {
      const values = form.getValues()
      const credentialChange =
        !props.channel ||
        values.key !== '' ||
        values.base_url !== props.channel.base_url
      if (credentialChange && !values.key.trim()) {
        form.setError('key', { message: t('Enter a new upstream key') })
        return false
      }
      let proofToken: string | undefined
      if (credentialChange) {
        const proof = await verification.requestVerification({
          scope: 'supplier.channel.credentials',
          context: {
            channel_id: props.channel?.id ?? 0,
            version: props.channel?.version ?? 0,
          },
          title: t('Verify channel credentials'),
        })
        if (!proof) return false
        proofToken = proof.proof_token
      }
      const { rpm, tpm, daily_usd, ...channelValues } = values
      const limits = limitsPayload(
        { rpm, tpm: tpmApplicable ? tpm : '', daily_usd },
        0
      )
      await saveSupplyChannel(
        props.channel?.id,
        {
          ...channelValues,
          key: values.key || undefined,
          priority: Number(values.priority),
          version: props.channel?.version,
          ...(!props.channel
            ? {
                limits: {
                  rpm: limits.rpm,
                  tpm: limits.tpm,
                  daily_usd: limits.daily_usd,
                },
              }
            : {}),
        },
        proofToken,
        props.audience,
        props.supplierId
      )
      return true
    },
    onSuccess: (saved) => {
      if (saved) {
        toast.success(t('Channel configuration saved'))
        void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
        if (props.audience === 'admin') {
          void queryClient.invalidateQueries({ queryKey: ['channels'] })
        }
        props.onClose()
      }
    },
    onSettled: () => form.setValue('key', ''),
  })
  return (
    <>
      <Dialog
        open={!verification.isActive}
        onOpenChange={(open) => !open && !mutation.isPending && props.onClose()}
        title={props.channel ? t('Edit Channel') : t('Add Channel')}
        description={t(
          'Credentials are write-only. New connection settings take effect after validation.'
        )}
      >
        <Form {...form}>
          <form
            className='space-y-4'
            onSubmit={form.handleSubmit(() => mutation.mutate())}
          >
            <FormField
              control={form.control}
              name='name'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Name')}</FormLabel>
                  <FormControl>
                    <Input {...field} disabled={mutation.isPending} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='template_id'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Channel template')}</FormLabel>
                  <Select
                    value={field.value}
                    items={templateOptions}
                    onValueChange={(value) => {
                      field.onChange(value ?? '')
                      form.setValue('models', [])
                      form.setValue('tpm', '')
                    }}
                    disabled={Boolean(props.channel) || mutation.isPending}
                  >
                    <FormControl>
                      <SelectTrigger>
                        <SelectValue
                          placeholder={t('Select a channel template')}
                        />
                      </SelectTrigger>
                    </FormControl>
                    <SelectContent>
                      {templateOptions.map((template) => (
                        <SelectItem key={template.value} value={template.value}>
                          {template.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FormMessage />
                  {selectedTemplate?.protocols?.includes('async_task') && (
                    <p className='text-muted-foreground text-xs'>
                      {t(
                        'Asynchronous models become available only after a test task is confirmed successful.'
                      )}
                    </p>
                  )}
                  {selectedTemplate?.protocols?.includes('realtime') && (
                    <p className='text-muted-foreground text-xs'>
                      {t(
                        'Realtime limits apply to each generation, not each connection.'
                      )}
                    </p>
                  )}
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='base_url'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Base URL')}</FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      disabled={mutation.isPending}
                      autoComplete='off'
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='key'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>
                    {props.channel
                      ? t('Replacement upstream key')
                      : t('Upstream key')}
                  </FormLabel>
                  <FormControl>
                    <Textarea
                      {...field}
                      disabled={mutation.isPending}
                      autoComplete='off'
                      spellCheck={false}
                      placeholder={
                        props.channel
                          ? t('Leave blank to keep the current key')
                          : undefined
                      }
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='models'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Models')}</FormLabel>
                  <FormControl>
                    <MultiSelect
                      options={(selectedTemplate?.models ?? []).map(
                        (model) => ({ value: model, label: model })
                      )}
                      selected={field.value}
                      onChange={field.onChange}
                      disabled={mutation.isPending}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='priority'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Supplier internal priority')}</FormLabel>
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
            {!props.channel && (
              <fieldset className='space-y-3'>
                <legend className='text-sm font-medium'>
                  {t('Channel limits')}
                </legend>
                <p className='text-muted-foreground text-xs'>
                  {t(
                    'Leave RPM or TPM blank for no limit. Daily limits use procurement cost in USD and reset at 00:00 Beijing time.'
                  )}
                </p>
                <div className='grid gap-3 sm:grid-cols-3'>
                  {(
                    [
                      { name: 'rpm', label: t('RPM') },
                      { name: 'tpm', label: t('TPM') },
                      { name: 'daily_usd', label: t('Daily limit (USD)') },
                    ] as const
                  ).map(({ name, label }) => (
                    <FormField
                      key={name}
                      control={form.control}
                      name={name}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{label}</FormLabel>
                          <FormControl>
                            <Input
                              {...field}
                              inputMode={
                                name === 'daily_usd' ? 'decimal' : 'numeric'
                              }
                              placeholder={
                                name === 'tpm' && !tpmApplicable
                                  ? t('Not applicable')
                                  : t('Unlimited')
                              }
                              disabled={
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
              </fieldset>
            )}
            <FormField
              control={form.control}
              name='remark'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Remark')}</FormLabel>
                  <FormControl>
                    <Textarea {...field} disabled={mutation.isPending} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <div className='flex justify-end gap-2'>
              <Button
                type='button'
                variant='outline'
                onClick={props.onClose}
                disabled={mutation.isPending}
              >
                {t('Cancel')}
              </Button>
              <Button
                type='submit'
                disabled={mutation.isPending || props.templates.length === 0}
              >
                {mutation.isPending ? t('Saving...') : t('Save')}
              </Button>
            </div>
          </form>
        </Form>
      </Dialog>
      <SecureVerificationDialog {...verification.dialogProps} />
    </>
  )
}
