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
import type { ColumnDef } from '@tanstack/react-table'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePage, useDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { toIntlLocale } from '@/i18n/languages'
import { formatFixedUSD } from '@/lib/currency'
import { formatTimestampToDate } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { getSupplierBudgetEvents, supplyQueryKey } from '../api'
import type { SupplierBudgetEvent } from '../types'

export function SupplierBudgetEvents(props: {
  channelId: number
  day: string
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const userId = useAuthStore((s) => s.auth.user?.id)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 10 })
  const [draftDay, setDraftDay] = useState(props.day)
  const [day, setDay] = useState(props.day)
  const query = useQuery({
    meta: { errorRedirect: false },
    queryKey: [
      ...supplyQueryKey,
      userId,
      'budget-events',
      props.channelId,
      day,
      pagination,
    ],
    queryFn: ({ signal }) =>
      getSupplierBudgetEvents(
        props.channelId,
        {
          p: pagination.pageIndex + 1,
          page_size: pagination.pageSize,
          day: day || undefined,
        },
        signal
      ),
  })
  const columns = useMemo<ColumnDef<SupplierBudgetEvent>[]>(
    () => [
      {
        accessorKey: 'sent_at',
        header: t('Time'),
        cell: ({ row }) =>
          row.original.sent_at > 0
            ? formatTimestampToDate(row.original.sent_at)
            : t('Unknown'),
      },
      { accessorKey: 'budget_day', header: t('Budget date (Beijing time)') },
      {
        accessorKey: 'status',
        header: t('Accounting status'),
        cell: ({ row }) =>
          ({
            pending: t('Pending'),
            settled: t('Settled'),
            reconciling: t('Reconciling'),
            estimated: t('TPM estimated'),
          })[row.original.status] ?? t('Unknown'),
      },
      {
        accessorKey: 'cost_usd',
        header: t('Procurement cost (USD)'),
        cell: ({ row }) =>
          row.original.cost_usd == null
            ? t('Unknown')
            : formatFixedUSD(row.original.cost_usd, {
                locale,
                digitsSmall: 9,
                digitsLarge: 9,
                abbreviate: false,
              }),
      },
      {
        accessorKey: 'reserved_usd',
        header: t('Reserved cost'),
        cell: ({ row }) =>
          row.original.reserved_usd == null
            ? t('Unknown')
            : formatFixedUSD(row.original.reserved_usd, {
                locale,
                digitsSmall: 9,
                digitsLarge: 9,
                abbreviate: false,
              }),
      },
      { accessorKey: 'id', header: t('Reference') },
    ],
    [locale, t]
  )
  const { table } = useDataTable({
    data: query.data?.items ?? [],
    columns,
    pagination,
    onPaginationChange: setPagination,
    totalCount: query.data?.total ?? 0,
    manualPagination: true,
    enableRowSelection: false,
    columnVisibilityStorageKey: false,
    columnSizingStorageKey: false,
  })
  if (query.isError) return <ErrorState onRetry={() => void query.refetch()} />
  return (
    <section className='space-y-3' aria-label={t('Budget details')}>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Estimated TPM does not confirm the request result. Unknown procurement costs remain pending.'
        )}
      </p>
      <DataTablePage
        table={table}
        columns={columns}
        fixedHeight={false}
        isLoading={query.isPending}
        isFetching={query.isFetching}
        emptyTitle={t('No budget events')}
        toolbar={
          <form
            className='flex gap-2'
            onSubmit={(event) => {
              event.preventDefault()
              setDay(draftDay)
              setPagination((value) => ({ ...value, pageIndex: 0 }))
            }}
          >
            <Input
              type='date'
              aria-label={t('Budget date (Beijing time)')}
              value={draftDay}
              onChange={(event) => setDraftDay(event.target.value)}
            />
            <Button type='submit' variant='outline'>
              {t('Filter')}
            </Button>
          </form>
        }
      />
    </section>
  )
}
