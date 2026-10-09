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
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
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
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'

import { updateChannel } from '../../api'
import { channelsQueryKeys } from '../../lib'
import { channelCostFormSchema } from '../../lib/upstream-cost-form'
import type { Channel } from '../../types'

export function ChannelCostDialog(props: {
  channel: Channel
  onClose: () => void
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const form = useForm<z.infer<typeof channelCostFormSchema>>({
    resolver: zodResolver(channelCostFormSchema),
    defaultValues: {
      cost_ratio:
        props.channel.cost_ratio == null
          ? ''
          : String(props.channel.cost_ratio),
    },
  })
  const mutation = useMutation({
    mutationFn: async (values: z.infer<typeof channelCostFormSchema>) => {
      requireServerSuccess(
        await updateChannel(props.channel.id, {
          cost_ratio:
            values.cost_ratio === '' ? null : Number(values.cost_ratio),
        })
      )
    },
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: channelsQueryKeys.all }),
        queryClient.invalidateQueries({ queryKey: ['channel-overview'] }),
        queryClient.invalidateQueries({ queryKey: ['upstream-credentials'] }),
      ])
      toast.success(t('Cost ratio saved'))
      props.onClose()
    },
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => !open && props.onClose()}
      title={t('Channel Cost Ratio')}
      description={props.channel.name}
      footer={
        <>
          <Button variant='outline' onClick={props.onClose}>
            {t('Cancel')}
          </Button>
          <Button
            disabled={mutation.isPending}
            onClick={form.handleSubmit((values) => mutation.mutate(values))}
          >
            {mutation.isPending ? t('Saving...') : t('Save')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <form onSubmit={form.handleSubmit((values) => mutation.mutate(values))}>
          <FormField
            control={form.control}
            name='cost_ratio'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Cost Ratio')}</FormLabel>
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
                    'Leave empty for unknown cost. Zero means explicitly free upstream usage.'
                  )}
                </FormDescription>
                <FormDescription>
                  {t('Upstream cost = base price × cost ratio.')}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
          <p className='text-muted-foreground mt-4 text-xs'>
            {t(
              'Changes apply to future calls. Historical cost records remain unchanged.'
            )}
          </p>
          {mutation.isError && (
            <Alert variant='destructive' className='mt-3'>
              <AlertDescription>
                {getServerErrorMessage(mutation.error, t('Save failed'))}
              </AlertDescription>
            </Alert>
          )}
        </form>
      </Form>
    </Dialog>
  )
}
