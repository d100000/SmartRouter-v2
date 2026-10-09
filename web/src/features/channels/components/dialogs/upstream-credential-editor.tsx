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
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import type { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { MultiSelect } from '@/components/multi-select'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { upstreamCredentialFormSchema } from '../../lib/upstream-cost-form'
import {
  createUpstreamSupplier,
  saveUpstreamCredential,
  type UpstreamCredential,
  type UpstreamSupplier,
} from '../../upstream-metadata-api'

export function UpstreamCredentialEditor(props: {
  credential: UpstreamCredential
  suppliers: UpstreamSupplier[]
  onClose: () => void
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [supplierName, setSupplierName] = useState('')
  const form = useForm<z.infer<typeof upstreamCredentialFormSchema>>({
    resolver: zodResolver(upstreamCredentialFormSchema),
    defaultValues: {
      alias: props.credential.alias,
      supplier_id: props.credential.supplier_id,
      tags: props.credential.tags,
      cost_ratio:
        props.credential.cost_ratio == null
          ? ''
          : String(props.credential.cost_ratio),
    },
  })
  const supplierMutation = useMutation({
    mutationFn: () => createUpstreamSupplier(supplierName.trim()),
    onSuccess: async (supplier) => {
      await queryClient.invalidateQueries({ queryKey: ['upstream-suppliers'] })
      form.setValue('supplier_id', supplier.id)
      setSupplierName('')
    },
  })
  const mutation = useMutation({
    mutationFn: (values: z.infer<typeof upstreamCredentialFormSchema>) =>
      saveUpstreamCredential(props.credential.credential_id, {
        alias: values.alias,
        supplier_id: values.supplier_id,
        tags: [...new Set(values.tags)],
        cost_ratio: values.cost_ratio === '' ? null : Number(values.cost_ratio),
      }),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['upstream-credentials'] }),
        queryClient.invalidateQueries({ queryKey: ['channel-overview'] }),
      ])
      toast.success(t('Upstream key metadata saved'))
      props.onClose()
    },
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => !open && props.onClose()}
      title={t('Upstream Key Metadata')}
      description={t(
        'Metadata changes affect every channel using this upstream key.'
      )}
      contentClassName='sm:max-w-xl'
      footer={
        <>
          <Button variant='outline' onClick={props.onClose}>
            {t('Cancel')}
          </Button>
          <Button
            disabled={mutation.isPending || supplierMutation.isPending}
            onClick={form.handleSubmit((values) => mutation.mutate(values))}
          >
            {mutation.isPending ? t('Saving...') : t('Save')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <form
          className='space-y-4'
          onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
        >
          <FormField
            control={form.control}
            name='alias'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Key Display Name')}</FormLabel>
                <FormControl>
                  <Input maxLength={100} {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='supplier_id'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Upstream Supplier')}</FormLabel>
                <FormControl>
                  <Combobox
                    options={[
                      { value: '', label: t('Unassigned') },
                      ...props.suppliers.map((supplier) => ({
                        value: supplier.id,
                        label: supplier.name,
                      })),
                    ]}
                    value={field.value}
                    onValueChange={(value) => field.onChange(value ?? '')}
                    aria-label={t('Upstream Supplier')}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <div className='flex flex-wrap items-end gap-2'>
            <div className='min-w-0 flex-1 space-y-1.5'>
              <Label htmlFor='new-upstream-supplier'>
                {t('New Supplier Name')}
              </Label>
              <Input
                id='new-upstream-supplier'
                value={supplierName}
                onChange={(event) => setSupplierName(event.target.value)}
                maxLength={100}
              />
            </div>
            <Button
              type='button'
              variant='outline'
              disabled={!supplierName.trim() || supplierMutation.isPending}
              onClick={() => supplierMutation.mutate()}
            >
              {t('Add Supplier')}
            </Button>
          </div>
          <FormField
            control={form.control}
            name='tags'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Key Tags')}</FormLabel>
                <FormControl>
                  <MultiSelect
                    options={[]}
                    selected={field.value}
                    onChange={field.onChange}
                    allowCreate
                    placeholder={t('Add tags...')}
                    aria-label={t('Key Tags')}
                    maxVisibleChips={4}
                  />
                </FormControl>
                <FormDescription>
                  {t('Press Enter or comma to add tags')}{' '}
                  {t(
                    'Separate tags with commas. Up to 20 tags, 40 characters each.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='cost_ratio'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Key Cost Ratio')}</FormLabel>
                <FormControl>
                  <Input
                    type='number'
                    min={0}
                    max={100}
                    step='any'
                    {...field}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Leave empty to inherit each bound channel’s cost ratio. Zero is explicit free usage.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
          <p className='text-muted-foreground text-xs'>
            {t(
              'Changes apply to future calls. Historical cost records remain unchanged.'
            )}
          </p>
          {(mutation.isError || supplierMutation.isError) && (
            <Alert variant='destructive'>
              <AlertDescription>
                {getServerErrorMessage(
                  mutation.error ?? supplierMutation.error,
                  t('Save failed')
                )}
              </AlertDescription>
            </Alert>
          )}
        </form>
      </Form>
    </Dialog>
  )
}
