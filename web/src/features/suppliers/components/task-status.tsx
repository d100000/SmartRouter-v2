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

import { StatusBadge } from '@/components/status-badge'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber, formatTimestampToDate } from '@/lib/format'

import type { SupplyLog } from '../types'

export function SupplyTaskStatus(props: { log: SupplyLog }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  if (props.log.stage !== 'task_submit' && props.log.protocol !== 'async_task') {
    return '—'
  }
  const status = props.log.task_status || 'unknown'
  const label =
    {
      accepted: t('Accepted, awaiting task status'),
      queued: t('Queued'),
      running: t('Running'),
      succeeded: t('Confirmed success'),
      failed: t('Confirmed failure'),
      unknown: t('Unknown task result'),
    }[status] ?? t('Unknown task result')
  return (
    <div className='max-w-72 space-y-1 text-xs'>
      <StatusBadge label={label} copyable={false} />
      <dl className='space-y-1'>
        <div>
          <dt className='text-muted-foreground'>
            {t('Task status observed at')}
          </dt>
          <dd>
            {props.log.task_observed_at
              ? formatTimestampToDate(props.log.task_observed_at, 'seconds')
              : t('Unknown')}
          </dd>
        </div>
        {props.log.task_completed_at != null &&
          props.log.task_completed_at > 0 && (
            <div>
              <dt className='text-muted-foreground'>
                {t('Task completed at')}
              </dt>
              <dd>
                {formatTimestampToDate(props.log.task_completed_at, 'seconds')}
              </dd>
            </div>
          )}
        <div>
          <dt className='text-muted-foreground'>
            {t('Task completion time (ms)')}
          </dt>
          <dd>
            {props.log.task_duration_ms == null
              ? t('Unknown')
              : formatNumber(props.log.task_duration_ms, locale)}
          </dd>
        </div>
      </dl>
    </div>
  )
}
