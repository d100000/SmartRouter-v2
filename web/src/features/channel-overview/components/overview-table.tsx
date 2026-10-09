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
import { Button } from '@/components/ui/button'
import { PanelWrapper } from '@/features/dashboard/components/ui/panel-wrapper'
import { toIntlLocale } from '@/i18n/languages'
import { formatBillingCurrencyFromUSD } from '@/lib/currency'
import { formatNumber } from '@/lib/format'

import type { OverviewRow } from '../types'
import { OverviewHelp } from './overview-help'

export function OverviewTable(props: {
  title: string
  rows: OverviewRow[]
  onSelect?: (id: string) => void
  maxRows?: number
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const money = (value: number | null) =>
    value == null
      ? '—'
      : formatBillingCurrencyFromUSD(value, {
          locale,
          abbreviate: false,
          digitsSmall: 8,
        })
  return (
    <PanelWrapper
      title={
        <span className='inline-flex items-center gap-1'>
          {props.title}
          <OverviewHelp title={props.title}>
            {t(
              'Ranking is limited to {{count}} rows. Select an object for its complete view.',
              { count: formatNumber(props.maxRows ?? 100, locale) }
            )}
          </OverviewHelp>
        </span>
      }
      contentClassName='p-0 sm:p-0'
    >
      <div className='overflow-x-auto'>
        <StaticDataTable
          className='rounded-none border-0'
          tableClassName='min-w-[880px]'
          tableProps={{ 'aria-label': props.title }}
          headerRowClassName={staticDataTableClassNames.mutedHeaderRow}
          data={props.rows}
          getRowKey={(row) => row.id}
          emptyContent={t('No data in this time range')}
          columns={[
            {
              id: 'name',
              header: t('Name'),
              className: 'min-w-48',
              cell: (row) =>
                props.onSelect ? (
                  <Button
                    disabled={row.id === ''}
                    variant='link'
                    className='h-auto max-w-72 justify-start px-0 py-1 text-left break-all whitespace-normal'
                    onClick={() => props.onSelect?.(row.id)}
                  >
                    {row.name}
                  </Button>
                ) : (
                  row.name
                ),
            },
            {
              id: 'requests',
              header: t('Requests'),
              cellClassName: staticDataTableClassNames.compactNumericCell,
              cell: (row) => formatNumber(row.requests, locale),
            },
            {
              id: 'attempts',
              header: t('Attempts'),
              cellClassName: staticDataTableClassNames.compactNumericCell,
              cell: (row) => formatNumber(row.attempts, locale),
            },
            {
              id: 'revenue',
              header: t('Billing Revenue'),
              cellClassName: staticDataTableClassNames.compactNumericCell,
              cell: (row) => money(row.revenue),
            },
            {
              id: 'cost',
              header: t('Known Upstream Cost'),
              cellClassName: staticDataTableClassNames.compactNumericCell,
              cell: (row) =>
                row.cost_coverage == null ? '—' : money(row.cost),
            },
            {
              id: 'profit',
              header: t('Estimated Gross Profit'),
              cellClassName: (row) =>
                row.margin != null && row.margin < 0
                  ? 'text-destructive py-2.5 text-right font-mono'
                  : staticDataTableClassNames.compactNumericCell,
              cell: (row) =>
                row.complete_attempts === 0 ? '—' : money(row.margin),
            },
            {
              id: 'success',
              header: t('Attempt Success Rate'),
              cellClassName: staticDataTableClassNames.compactNumericCell,
              cell: (row) =>
                row.health == null
                  ? '—'
                  : `${formatNumber(row.health, locale)}%`,
            },
            {
              id: 'coverage',
              header: t('Cost Coverage'),
              cellClassName: staticDataTableClassNames.compactNumericCell,
              cell: (row) =>
                row.cost_coverage == null
                  ? '—'
                  : `${formatNumber(row.cost_coverage, locale)}%`,
            },
          ]}
        />
      </div>
    </PanelWrapper>
  )
}
