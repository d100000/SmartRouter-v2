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

import {
  StaticDataTable,
  staticDataTableClassNames,
} from '@/components/data-table'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import { getOverviewObjectLabel } from '../lib/object-label'
import type { OverviewModel } from '../types'

export function OverviewCacheTable(props: {
  dimension: 'model' | 'channel'
  rows: OverviewModel[]
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const title =
    props.dimension === 'model'
      ? t('Cache Statistics by Model')
      : t('Cache Statistics by Channel')

  return (
    <div className='overflow-x-auto'>
      <StaticDataTable
        className={staticDataTableClassNames.embeddedContainer}
        tableClassName='min-w-[880px]'
        tableProps={{ 'aria-label': title }}
        headerRowClassName={staticDataTableClassNames.mutedHeaderRow}
        data={props.rows}
        getRowKey={(row) => row.id}
        emptyContent={t('No cache usage samples yet.')}
        columns={[
          {
            id: 'name',
            header: props.dimension === 'model' ? t('Model') : t('Channel'),
            className: 'min-w-48',
            cell: (row) => (
              <span className='block max-w-72 break-all whitespace-normal'>
                {props.dimension === 'channel'
                  ? getOverviewObjectLabel('channel', row.name, row.id, t)
                  : row.name || row.id || t('Unknown')}
              </span>
            ),
          },
          {
            id: 'cache-hit-rate',
            header: t('Cache Hit Rate'),
            cellClassName: staticDataTableClassNames.compactNumericCell,
            cell: (row) =>
              row.cache_usage_attempts > 0 && row.cache_hit_rate != null
                ? `${formatNumber(row.cache_hit_rate, locale)}%`
                : '—',
          },
          {
            id: 'cache-read',
            header: t('Cache Read Tokens'),
            cellClassName: staticDataTableClassNames.compactNumericCell,
            cell: (row) =>
              row.cache_usage_attempts > 0
                ? formatNumber(row.cache_read_tokens, locale)
                : '—',
          },
          {
            id: 'cache-write',
            header: t('Cache Write Tokens'),
            cellClassName: staticDataTableClassNames.compactNumericCell,
            cell: (row) =>
              row.cache_usage_attempts > 0
                ? formatNumber(row.cache_write_tokens, locale)
                : '—',
          },
          {
            id: 'input-tokens',
            header: t('Input Tokens'),
            cellClassName: staticDataTableClassNames.compactNumericCell,
            cell: (row) =>
              row.cache_usage_attempts > 0
                ? formatNumber(row.cache_input_tokens, locale)
                : '—',
          },
          {
            id: 'cache-coverage',
            header: t('Cache Usage Coverage'),
            cellClassName: staticDataTableClassNames.compactNumericCell,
            cell: (row) => (
              <div className='space-y-0.5'>
                <div>
                  {row.cache_usage_coverage == null
                    ? '—'
                    : `${formatNumber(row.cache_usage_coverage, locale)}%`}
                </div>
                {row.cache_usage_attempts != null && (
                  <div className='text-muted-foreground text-xs'>
                    {t('{{known}} / {{total}} attempts', {
                      known: formatNumber(row.cache_usage_attempts, locale),
                      total: formatNumber(row.attempts, locale),
                    })}
                  </div>
                )}
              </div>
            ),
          },
        ]}
      />
    </div>
  )
}
