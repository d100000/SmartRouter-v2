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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  SecureVerificationDialog,
  useSecureVerification,
} from '@/features/auth/secure-verification'

import { copySupplyChannel, supplyQueryKey } from '../api'
import type { SupplyChannel } from '../types'

export function CopySupplyChannelDialog(props: {
  channel: SupplyChannel
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [name, setName] = useState('')
  const queryClient = useQueryClient()
  const verification = useSecureVerification()
  const mutation = useMutation({
    mutationFn: async () => {
      const proof = await verification.requestVerification({
        scope: 'supplier.channel.credentials',
        context: {
          channel_id: props.channel.id,
          version: props.channel.version,
        },
        title: t('Verify channel credentials'),
      })
      if (!proof) return false
      await copySupplyChannel(props.channel, name.trim(), proof.proof_token)
      return true
    },
    onSuccess: (saved) => {
      if (saved) {
        toast.success(t('Channel configuration saved'))
        void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
        props.onClose()
      }
    },
  })
  return (
    <>
      <ConfirmDialog
        open={!verification.isActive}
        onOpenChange={(open) => !open && !mutation.isPending && props.onClose()}
        title={t('Copy Channel')}
        desc={t(
          'The copy belongs to your supplier and keeps credentials write-only. It has independent usage counters.'
        )}
        confirmText={t('Copy Channel')}
        disabled={!name.trim() || [...name.trim()].length > 100}
        isLoading={mutation.isPending}
        handleConfirm={() => mutation.mutate()}
      >
        <div className='space-y-2'>
          <Label htmlFor='supply-copy-name'>{t('Name')}</Label>
          <Input
            id='supply-copy-name'
            value={name}
            onChange={(event) => setName(event.target.value)}
            disabled={mutation.isPending}
          />
        </div>
      </ConfirmDialog>
      <SecureVerificationDialog {...verification.dialogProps} />
    </>
  )
}
