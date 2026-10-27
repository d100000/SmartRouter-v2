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
import { AlertTriangle } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StatusBadge, type StatusVariant } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

import type { UsageLog } from '../data/schema'
import { isUnchargedStreamFailure, parseLogOther } from '../lib/format'
import { getLogTypeConfig } from '../lib/utils'
import { DetailsDialog } from './dialogs/details-dialog'

export function LogRequestStatus(props: {
  log: UsageLog
  isAdmin: boolean
  isRoot: boolean
  className?: string
  showDot?: boolean
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const other = parseLogOther(props.log.other)
  const config = getLogTypeConfig(props.log.type)

  if (!other?.stream_failure) {
    return (
      <StatusBadge
        label={t(config.label)}
        variant={config.color as StatusVariant}
        size='sm'
        copyable={false}
        showDot={props.showDot}
        className={props.className}
      />
    )
  }

  const label = isUnchargedStreamFailure(props.log, other)
    ? `${t('Error')} · ${t('Not charged')}`
    : t('Error')

  return (
    <>
      <Button
        variant='ghost'
        size='xs'
        className={cn(
          'h-auto min-h-6 max-w-full justify-start py-0 text-left whitespace-normal max-sm:min-h-11',
          props.className
        )}
        aria-haspopup='dialog'
        aria-expanded={open}
        onClick={() => setOpen(true)}
      >
        <StatusBadge
          label={label}
          variant='danger'
          icon={AlertTriangle}
          copyable={false}
          className='h-auto min-h-5 px-0 text-xs [&_span]:whitespace-normal'
        />
      </Button>
      <DetailsDialog
        log={props.log}
        isAdmin={props.isAdmin}
        isRoot={props.isRoot}
        open={open}
        onOpenChange={setOpen}
      />
    </>
  )
}
