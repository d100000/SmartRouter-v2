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
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { useAuthStore } from '@/stores/auth-store'

import {
  getReadOnlyChannel,
  getSuppliers,
  getSupplierTemplates,
  supplyQueryKey,
} from '../api'
import { supplierChannelTemplates } from '../lib/templates'
import { ChannelEditor } from './channel-editor'

export function AdminSupplierChannelEditor(props: {
  channelId: number
  onClose: () => void
}) {
  const { t } = useTranslation()
  const userId = useAuthStore((state) => state.auth.user?.id)
  const channel = useQuery({
    queryKey: [...supplyQueryKey, userId, 'admin', 'channel', props.channelId],
    queryFn: ({ signal }) => getReadOnlyChannel(props.channelId, signal),
  })
  const suppliers = useQuery({
    queryKey: [...supplyQueryKey, userId, 'admin', 'accounts'],
    queryFn: ({ signal }) => getSuppliers(signal),
  })
  const templates = useQuery({
    queryKey: [...supplyQueryKey, userId, 'admin', 'templates'],
    queryFn: ({ signal }) => getSupplierTemplates(signal),
  })
  const supplier = suppliers.data?.items.find(
    (item) => item.id === channel.data?.supplier_id
  )
  if (channel.data && supplier && templates.data) {
    return (
      <ChannelEditor
        audience='admin'
        channel={channel.data}
        templates={supplierChannelTemplates(supplier, templates.data)}
        onClose={props.onClose}
      />
    )
  }
  return (
    <Dialog
      open
      title={t('Edit Channel')}
      onOpenChange={(open) => !open && props.onClose()}
    >
      {channel.isPending || suppliers.isPending || templates.isPending ? (
        <LoadingState />
      ) : (
        <ErrorState
          onRetry={() => {
            void channel.refetch()
            void suppliers.refetch()
            void templates.refetch()
          }}
        />
      )}
    </Dialog>
  )
}
