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

import { isTagAggregateRow } from '../lib/channel-utils'
import type { Channel } from '../types'
import { useChannels } from './channels-provider'

/**
 * Opens a channel's figures and latest requests. A tag row stands for several
 * channels and the usage log is searched by one, so it has no button.
 */
export function ChannelDetailsButton(props: { channel: Channel }) {
  const { t } = useTranslation()
  const { setCurrentRow, setOpen } = useChannels()
  if (isTagAggregateRow(props.channel)) return null
  return (
    <Button
      variant='outline'
      size='xs'
      className='shrink-0'
      aria-haspopup='dialog'
      aria-label={t('Details of {{name}}', { name: props.channel.name })}
      onClick={() => {
        setCurrentRow(props.channel)
        setOpen('channel-details')
      }}
    >
      {t('Details')}
    </Button>
  )
}
