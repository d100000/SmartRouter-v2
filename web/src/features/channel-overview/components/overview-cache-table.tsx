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
  type StaticDataTableColumn,
} from '@/components/data-table'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import { getOverviewObjectLabel } from '../lib/object-label'
import type { OverviewChannelModel, OverviewModel } from '../types'

type CacheRow = OverviewModel | OverviewChannelModel

export function OverviewCacheTable(props: {
  dimension: 'model' | 'channel' | 'channel_model'
  rows: CacheRow[]
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const titles = {
    model: t('Cache Statistics by Model'),
    channel: t('Cache Statistics by Channel'),
    channel_model: t('Cache Statistics by Channel and Model'),
  }
  const channelColumns: StaticDataTableColumn<CacheRow>[] = []
  if (props.dimension === 'channel_model') {
    channelColumns.push({
      id: 'channel',
      header: t('Channel'),
      className: 'min-w-48',
      cell: (row) => {
        if (!('channel_id' in row)) return '—'
        return (
          <div className='max-w-72 break-all whitespace-normal'>
            <div>
              {getOverviewObjectLabel(
                'channel',
                row.channel_name,
                String(row.channel_id),
                t
              )}
            </div>
            <div
              data-table-text='secondary'
              className='text-muted-foreground text-xs'
            >
              #{row.channel_id}
            </div>
          </div>
        )
      },
    })
  }

  return (
    <div className='overflow-x-auto'>
      <StaticDataTable
        className={staticDataTableClassNames.embeddedContainer}
        tableClassName={
          props.dimension === 'channel_model'
            ? 'min-w-[1080px]'
            : 'min-w-[960px]'
        }
        tableProps={{ 'aria-label': titles[props.dimension] }}
        headerRowClassName={staticDataTableClassNames.mutedHeaderRow}
        data={props.rows}
        getRowKey={(row) => row.id}
        emptyContent={t('No cache usage samples yet.')}
        columns={[
          ...channelColumns,
          {
            id: 'name',
            header: props.dimension === 'channel' ? t('Channel') : t('Model'),
            className: 'min-w-48',
            cell: (row) => {
              let label = row.name || t('Unknown')
              if (props.dimension === 'model') {
                label = row.name || row.id || t('Unknown')
              } else if (props.dimension === 'channel') {
                label = getOverviewObjectLabel('channel', row.name, row.id, t)
              }
              return (
                <span className='block max-w-72 break-all whitespace-normal'>
                  {label}
                </span>
              )
            },
          },
          {
            id: 'cache-hit-rate',
            header: t('Cache Token Hit Rate'),
            cellClassName: staticDataTableClassNames.compactNumericCell,
            cell: (row) =>
              row.cache_usage_attempts > 0 && row.cache_hit_rate != null
                ? `${formatNumber(row.cache_hit_rate, locale)}%`
                : '—',
          },
          {
            id: 'cache-request-hit-rate',
            header: t('Cache Request Hit Rate'),
            cellClassName: staticDataTableClassNames.compactNumericCell,
            cell: (row) => {
              if (
                !(row.cache_usage_attempts > 0) ||
                row.cache_attempt_hit_rate == null
              ) {
                return '—'
              }
              return (
                <div className='space-y-0.5'>
                  <div>{formatNumber(row.cache_attempt_hit_rate, locale)}%</div>
                  <div
                    data-table-text='secondary'
                    className='text-muted-foreground text-xs'
                  >
                    {t('{{hits}} / {{total}} measured attempts', {
                      hits: formatNumber(row.cache_hit_attempts, locale),
                      total: formatNumber(row.cache_usage_attempts, locale),
                    })}
                  </div>
                </div>
              )
            },
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
                  <div
                    data-table-text='secondary'
                    className='text-muted-foreground text-xs'
                  >
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
