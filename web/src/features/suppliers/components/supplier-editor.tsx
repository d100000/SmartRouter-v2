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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import {
  SecureVerificationDialog,
  useSecureVerification,
} from '@/features/auth/secure-verification'
import { getUpstreamSuppliers } from '@/features/channels/upstream-metadata-api'
import { useAuthStore } from '@/stores/auth-store'

import { getSupplierTemplates, saveSupplier, supplyQueryKey } from '../api'
import { supplierAccountSchema as schema } from '../lib/forms'
import type { SupplierAccount } from '../types'

type Values = z.infer<typeof schema>
export function SupplierEditor(props: {
  supplier?: SupplierAccount
  configuredSupplierIds?: string[]
  onClose: () => void
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const verification = useSecureVerification()
  const supplier = props.supplier
  const userId = useAuthStore((state) => state.auth.user?.id)
  const catalog = useQuery({
    meta: { errorRedirect: false },
    queryKey: [...supplyQueryKey, userId, 'admin', 'templates'],
    queryFn: ({ signal }) => getSupplierTemplates(signal),
  })
  const templates = (catalog.data ?? []).map((template) => ({
    value: template.id,
    label: template.name,
  }))
  const [existingId, setExistingId] = useState('new')
  const upstream = useQuery({
    meta: { errorRedirect: false },
    queryKey: [...supplyQueryKey, userId, 'procurement-suppliers'],
    queryFn: ({ signal }) => getUpstreamSuppliers(signal),
    enabled: !supplier,
  })
  const sourceOptions = [
    { value: 'new', label: t('Create new supplier') },
    ...(upstream.data ?? [])
      .filter((item) => !props.configuredSupplierIds?.includes(item.id))
      .map((item) => ({ value: item.id, label: item.name })),
  ]
  const targetId =
    supplier?.id ?? (existingId === 'new' ? undefined : existingId)
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: supplier?.name ?? '',
      user_id: supplier?.user_id ? String(supplier.user_id) : '',
      allowed_types:
        supplier?.allowed_templates ??
        supplier?.allowed_types.map(String) ??
        [],
      allowed_models: supplier?.allowed_models ?? [],
      group: supplier?.group ?? 'default',
      max_channels: String(supplier?.max_channels ?? 20),
      cost_ratio:
        supplier?.cost_ratio == null ? '' : String(supplier.cost_ratio),
      pool_priority: String(supplier?.pool_priority ?? 0),
      pool_capacity: String(supplier?.pool_capacity ?? 0),
      pool_weight: String(supplier?.pool_weight ?? 1),
      max_output_tokens: supplier?.max_output_tokens
        ? String(supplier.max_output_tokens)
        : '',
      max_input_tokens: supplier?.max_input_tokens
        ? String(supplier.max_input_tokens)
        : '',
      supply_enabled: supplier?.supply_enabled ?? true,
    },
  })
  const mutation = useMutation({
    mutationFn: async (values: Values) => {
      const proof = await verification.requestVerification({
        scope: 'supplier.manage',
        context: {
          supplier_id: targetId ?? '',
          version: supplier?.revision ?? 0,
        },
        title: t('Verify supplier settings'),
      })
      if (!proof) return false
      await saveSupplier(
        targetId,
        {
          ...values,
          revision: supplier?.revision ?? 0,
          user_id: Number(values.user_id) || 0,
          allowed_templates: values.allowed_types,
          allowed_types: values.allowed_types
            .filter((id) => /^\d+$/.test(id))
            .map(Number),
          max_channels: Number(values.max_channels),
          cost_ratio:
            values.cost_ratio === '' ? null : Number(values.cost_ratio),
          pool_priority: Number(values.pool_priority),
          pool_capacity: Number(values.pool_capacity),
          pool_weight: Number(values.pool_weight),
          max_output_tokens: Number(values.max_output_tokens),
          max_input_tokens: Number(values.max_input_tokens) || 0,
        },
        proof.proof_token
      )
      return true
    },
    onSuccess: (saved) => {
      if (saved) {
        toast.success(t('Saved successfully'))
        void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
        props.onClose()
      }
    },
  })
  const fields = [
    { name: 'name', label: t('Supplier name') },
    { name: 'user_id', label: t('Dedicated user ID (optional)') },
    { name: 'group', label: t('Group') },
    { name: 'max_channels', label: t('Maximum channels') },
    { name: 'cost_ratio', label: t('Procurement cost ratio') },
    { name: 'pool_priority', label: t('Platform pool priority') },
    {
      name: 'pool_capacity',
      label: t('Supply pool capacity (0 uses platform default)'),
    },
    { name: 'pool_weight', label: t('Platform pool weight') },
    {
      name: 'max_output_tokens',
      label: `${t('Trusted output token ceiling')} · ${t('Optional')}`,
    },
    {
      name: 'max_input_tokens',
      label: `${t('Trusted input and context token ceiling')} · ${t('Optional')}`,
    },
  ] as const
  return (
    <>
      <Dialog
        open={!verification.isActive}
        onOpenChange={(open) => !open && !mutation.isPending && props.onClose()}
        title={supplier ? t('Edit supplier') : t('Add supplier')}
        description={t(
          'Create the supplier configuration, then create a dedicated supplier user in Users.'
        )}
      >
        {catalog.isPending && <LoadingState />}
        {catalog.isError && (
          <ErrorState onRetry={() => void catalog.refetch()} />
        )}
        <Form {...form}>
          <form
            className='space-y-4'
            onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
          >
            {!supplier && (
              <div className='space-y-2'>
                {upstream.isError && (
                  <ErrorState
                    className='min-h-0'
                    onRetry={() => void upstream.refetch()}
                  />
                )}
                <Select
                  value={existingId}
                  items={sourceOptions}
                  onValueChange={(value) => {
                    setExistingId(value ?? 'new')
                    const source = upstream.data?.find(
                      (item) => item.id === value
                    )
                    if (source) form.setValue('name', source.name)
                  }}
                >
                  <SelectTrigger
                    aria-label={t('Existing procurement supplier (optional)')}
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {sourceOptions.map((item) => (
                      <SelectItem key={item.value} value={item.value}>
                        {item.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className='text-muted-foreground text-xs'>
                  {t(
                    'Linking a procurement supplier does not grant access to historical channels.'
                  )}
                </p>
              </div>
            )}
            <div className='grid gap-4 sm:grid-cols-2'>
              {fields.map((item) => (
                <FormField
                  key={item.name}
                  control={form.control}
                  name={item.name}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{item.label}</FormLabel>
                      <FormControl>
                        <Input
                          {...field}
                          disabled={
                            mutation.isPending ||
                            (item.name === 'user_id' &&
                              Boolean(supplier?.user_id))
                          }
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              ))}
            </div>
            <p className='text-muted-foreground text-xs'>
              {t(
                'The output ceiling must match the actual model capability. It does not truncate requests.'
              )}
            </p>
            <FormField
              control={form.control}
              name='allowed_types'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Allowed channel templates')}</FormLabel>
                  <FormControl>
                    <MultiSelect
                      aria-label={t('Allowed channel templates')}
                      options={templates}
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
              name='allowed_models'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Allowed models')}</FormLabel>
                  <FormControl>
                    <MultiSelect
                      aria-label={t('Allowed models')}
                      options={[]}
                      selected={field.value}
                      onChange={field.onChange}
                      allowCreate
                      disabled={mutation.isPending}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='supply_enabled'
              render={({ field }) => (
                <FormItem className='flex items-center justify-between'>
                  <FormLabel>{t('Supply enabled')}</FormLabel>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                      disabled={mutation.isPending}
                    />
                  </FormControl>
                </FormItem>
              )}
            />
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
                disabled={mutation.isPending || !catalog.data?.length}
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
