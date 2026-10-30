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

import { IconGithub } from '@/assets/brand-icons/icon-github'
import { formatTimestampToDate } from '@/lib/format'
import { cn } from '@/lib/utils'

import { PROJECT_REPOSITORY_URL, projectVersionQueryOptions } from './query'

function VersionDate(props: { value?: string }) {
  if (!props.value || !Number.isFinite(Date.parse(props.value))) return null
  const formatted = formatTimestampToDate(
    Date.parse(props.value),
    'milliseconds'
  )
  return (
    <time
      dateTime={props.value}
      title={formatted}
      className='shrink-0 tabular-nums'
    >
      {formatted.slice(0, 10)}
    </time>
  )
}

export function ProjectVersion(props: { className?: string }) {
  const { t } = useTranslation()
  const query = useQuery(projectVersionQueryOptions)
  const latest = query.data
  const currentVersion =
    import.meta.env.VITE_PROJECT_VERSION || t('Local build')
  let latestLabel = t('Checking updates...')
  if (latest) {
    latestLabel =
      latest.kind === 'release' ? t('Latest release') : t('Latest commit')
    if (query.isError) latestLabel = t('Last known version')
  } else if (query.isError || query.isPaused) {
    latestLabel = t('GitHub temporarily unavailable')
  } else if (query.isSuccess) {
    latestLabel = t('No published versions')
  }

  return (
    <div
      role='group'
      aria-label={t('Project version')}
      className={cn(
        'text-muted-foreground flex min-w-0 flex-wrap items-center justify-end gap-x-4 gap-y-1 text-xs',
        props.className
      )}
    >
      <span className='flex min-w-0 flex-wrap items-center justify-end gap-x-2 gap-y-1'>
        <span>{t('Custom build')}</span>{' '}
        <span className='min-w-0 font-mono break-all'>{currentVersion}</span>{' '}
        <VersionDate value={import.meta.env.VITE_PROJECT_VERSION_DATE} />
      </span>
      <a
        href={latest?.url ?? PROJECT_REPOSITORY_URL}
        target='_blank'
        rel='noopener noreferrer'
        title={t('View project on GitHub')}
        className='hover:text-foreground focus-visible:ring-ring flex min-h-6 min-w-0 flex-wrap items-center justify-end gap-x-2 gap-y-1 rounded-sm focus-visible:ring-2 focus-visible:outline-none'
      >
        <IconGithub aria-hidden='true' className='size-3.5 shrink-0' />
        <span>{latestLabel}</span>{' '}
        {latest && (
          <>
            <span className='min-w-0 font-mono break-all'>
              {latest.version}
            </span>{' '}
            <VersionDate value={latest.date} />
          </>
        )}
      </a>
    </div>
  )
}
