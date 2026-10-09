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
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { isTagAggregateRow } from '../lib'
import type { Channel } from '../types'
import { useChannels } from './channels-provider'

export function ChannelCostCell(props: { channel: Channel }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const { sensitiveVisible, setCurrentRow, setOpen } = useChannels()
  const user = useAuthStore((state) => state.auth.user)
  const canWrite = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.WRITE
  )
  if (isTagAggregateRow(props.channel)) {
    return <span className='text-muted-foreground'>—</span>
  }
  let label = t('Not configured')
  if (props.channel.cost_ratio != null) {
    label = formatNumber(props.channel.cost_ratio, locale, {
      maximumFractionDigits: 20,
    })
  }
  if (!sensitiveVisible) label = '••••'
  if (!canWrite) return <span className='text-xs tabular-nums'>{label}</span>
  return (
    <Button
      variant='ghost'
      size='sm'
      className='font-mono'
      aria-label={t('Edit cost ratio for {{name}}', {
        name: props.channel.name,
      })}
      onClick={() => {
        setCurrentRow(props.channel)
        setOpen('cost-settings')
      }}
    >
      {label}
    </Button>
  )
}
