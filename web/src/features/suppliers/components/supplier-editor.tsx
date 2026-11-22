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
import { PasswordInput } from '@/components/password-input'
import { PasswordStrength } from '@/components/password-strength'
import { Button } from '@/components/ui/button'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
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
import { SUPPLIER_PASSWORD_MIN_LENGTH } from '@/lib/password-policy'
import { useAuthStore } from '@/stores/auth-store'

import { getSupplierTemplates, saveSupplier, supplyQueryKey } from '../api'
import {
  supplierAccountCredentialsSchema,
  supplierAccountSchema as schema,
} from '../lib/forms'
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
  const provisioning = !supplier?.user_id
  const [advanced, setAdvanced] = useState(false)
  const userId = useAuthStore((state) => state.auth.user?.id)
  const catalog = useQuery({
    meta: { errorRedirect: false },
    queryKey: [...supplyQueryKey, userId, 'admin', 'templates'],
    queryFn: ({ signal }) => getSupplierTemplates(signal),
    enabled: advanced,
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
      username: '',
      password: '',
      allowed_types:
        supplier?.allowed_templates ??
        supplier?.allowed_types.map(String) ??
        [],
      allowed_models: supplier?.allowed_models ?? [],
      allow_all_templates: supplier?.allow_all_templates ?? !supplier,
      allow_all_models: supplier?.allow_all_models ?? !supplier,
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
  const allowAllTemplates = form.watch('allow_all_templates')
  const allowAllModels = form.watch('allow_all_models')
  const mutation = useMutation({
    mutationFn: async () => {
      const values = form.getValues()
      const context = {
        supplier_id: targetId ?? '',
        version: supplier?.revision ?? 0,
      }
      const title = t('Verify supplier settings')
      const proof = await verification.requestVerification(
        provisioning
          ? {
              scope: 'supplier.provision',
              context: { ...context, username: values.username.trim() },
              title,
            }
          : { scope: 'supplier.manage', context, title }
      )
      if (!proof) return false
      const templateIds = values.allow_all_templates ? [] : values.allowed_types
      await saveSupplier(
        targetId,
        {
          name: values.name.trim(),
          revision: supplier?.revision ?? 0,
          ...(provisioning
            ? { username: values.username.trim(), password: values.password }
            : {}),
          supply_enabled: values.supply_enabled,
          allow_all_templates: values.allow_all_templates,
          allow_all_models: values.allow_all_models,
          allowed_models: values.allow_all_models ? [] : values.allowed_models,
          allowed_templates: templateIds,
          allowed_types: templateIds
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
        void queryClient.invalidateQueries({ queryKey: ['users'] })
        props.onClose()
      }
    },
    onSettled: () => form.setValue('password', ''),
  })
  const fields = [
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
  let title = t('Add supplier')
  if (supplier) {
    title = provisioning ? t('Open supplier account') : t('Edit supplier')
  }
  return (
    <>
      <Dialog
        open={!verification.isActive}
        onOpenChange={(open) => !open && !mutation.isPending && props.onClose()}
        title={title}
      >
        <Form {...form}>
          <form
            className='space-y-4'
            onSubmit={form.handleSubmit((values) => {
              if (provisioning) {
                const result =
                  supplierAccountCredentialsSchema.safeParse(values)
                if (!result.success) {
                  for (const issue of result.error.issues) {
                    const field = issue.path[0]
                    if (field === 'username' || field === 'password') {
                      form.setError(field, { message: issue.message })
                    }
                  }
                  return
                }
              }
              mutation.mutate()
            })}
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
            <FormField
              control={form.control}
              name='name'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Supplier name')}</FormLabel>
                  <FormControl>
                    <Input {...field} disabled={mutation.isPending} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            {provisioning && (
              <>
                <FormField
                  control={form.control}
                  name='username'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Username')}</FormLabel>
                      <FormControl>
                        <Input
                          {...field}
                          autoComplete='off'
                          disabled={mutation.isPending}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name='password'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Password')}</FormLabel>
                      <FormControl>
                        <PasswordInput
                          {...field}
                          autoComplete='new-password'
                          disabled={mutation.isPending}
                        />
                      </FormControl>
                      <PasswordStrength
                        value={field.value}
                        minLength={SUPPLIER_PASSWORD_MIN_LENGTH}
                      />
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </>
            )}
            <Collapsible open={advanced} onOpenChange={setAdvanced}>
              <CollapsibleTrigger
                render={<Button type='button' variant='outline' />}
              >
                {t('Advanced Settings')}
              </CollapsibleTrigger>
              <CollapsibleContent className='mt-4 space-y-4'>
                {catalog.isPending && <LoadingState />}
                {catalog.isError && (
                  <ErrorState onRetry={() => void catalog.refetch()} />
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
                            <Input {...field} disabled={mutation.isPending} />
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
                {(
                  [
                    ['allow_all_templates', t('Allow all supported templates')],
                    ['allow_all_models', t('Allow all supported models')],
                  ] as const
                ).map(([name, label]) => (
                  <FormField
                    key={name}
                    control={form.control}
                    name={name}
                    render={({ field }) => (
                      <FormItem className='flex items-center justify-between'>
                        <FormLabel>{label}</FormLabel>
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
                ))}
                {!allowAllTemplates && (
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
                )}
                {!allowAllModels && (
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
                )}
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
              </CollapsibleContent>
            </Collapsible>
            <div className='flex justify-end gap-2'>
              <Button
                type='button'
                variant='outline'
                disabled={mutation.isPending}
                onClick={props.onClose}
              >
                {t('Cancel')}
              </Button>
              <Button type='submit' disabled={mutation.isPending}>
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
