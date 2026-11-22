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
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'

import { closeSupplierChannelDraft, supplyQueryKey } from '../api'
import type { SupplyChannel } from '../types'

export function CloseSupplierChannelDraftDialog(props: {
  channel: SupplyChannel
  onClose: () => void
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const close = useMutation({
    mutationFn: () => closeSupplierChannelDraft(props.channel),
    onSuccess: () => {
      toast.success(t('Draft closed'))
      void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
      void queryClient.invalidateQueries({ queryKey: ['channels'] })
      props.onClose()
    },
  })

  return (
    <ConfirmDialog
      open
      onOpenChange={(open) => !open && !close.isPending && props.onClose()}
      title={t('Close draft')}
      desc={
        props.channel.has_validated_config && props.channel.status === 'available'
          ? t(
              'Closing the draft keeps its test history. The current published configuration stays active.'
            )
          : t('The draft will be closed and its test history will be kept.')
      }
      confirmText={t('Close draft')}
      isLoading={close.isPending}
      handleConfirm={() => close.mutate()}
    />
  )
}
