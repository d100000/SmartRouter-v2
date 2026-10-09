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
import { Info } from 'lucide-react'
import { type ReactNode, useId, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'

export function OverviewHelp(props: { title: string; children: ReactNode }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const descriptionId = useId()
  return (
    <TooltipProvider>
      <Tooltip open={open} onOpenChange={setOpen}>
        <TooltipTrigger
          closeOnClick={false}
          render={
            <Button
              variant='ghost'
              size='icon-xs'
              className='text-muted-foreground shrink-0'
              aria-label={t('Details for {{title}}', { title: props.title })}
              aria-expanded={open}
              aria-describedby={open ? descriptionId : undefined}
              onClick={() => setOpen(true)}
            >
              <Info aria-hidden />
            </Button>
          }
        />
        <TooltipContent
          id={descriptionId}
          role='tooltip'
          className='block max-h-80 max-w-xs space-y-2 overflow-y-auto leading-relaxed'
        >
          {props.children}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}
