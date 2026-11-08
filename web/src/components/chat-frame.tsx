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

import { SandboxedFrame } from '@/components/sandboxed-frame'
import { Button } from '@/components/ui/button'

type ChatFrameProps = {
  src: string
  title: string
}

export function ChatFrame(props: ChatFrameProps) {
  const { t } = useTranslation()
  return (
    <div className='flex h-full min-h-0 w-full flex-col'>
      <div className='flex flex-wrap items-center justify-end gap-2 border-b px-3 py-2'>
        <p className='text-muted-foreground min-w-0 flex-1 text-xs'>
          {t(
            'Open in a new tab to use sign-in, saved chats, camera, or microphone.'
          )}
        </p>
        <Button
          role='link'
          variant='outline'
          size='sm'
          render={
            <a
              href={props.src}
              target='_blank'
              rel='noopener noreferrer'
              referrerPolicy='no-referrer'
            />
          }
        >
          {t('Open in new tab')}
        </Button>
      </div>
      <SandboxedFrame
        key={props.src}
        src={props.src}
        className='min-h-0 w-full flex-1 border-0'
        title={props.title}
      />
    </div>
  )
}
