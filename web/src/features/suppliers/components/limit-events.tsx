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
import { formatTimestampToDate } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { getSupplierLimitEvents, supplyQueryKey } from '../api'
import type { SupplierLimitEvent } from '../types'

export function SupplierLimitEvents(props: {
  channelId: number
  audience: 'supplier' | 'admin'
}) {
  const { t } = useTranslation()
  const userId = useAuthStore((s) => s.auth.user?.id)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 10 })
  const query = useQuery({
    queryKey: [
      ...supplyQueryKey,
      userId,
      props.audience,
      'limit-events',
      props.channelId,
      pagination,
    ],
    queryFn: ({ signal }) =>
      getSupplierLimitEvents(
        props.channelId,
        props.audience,
        { p: pagination.pageIndex + 1, page_size: pagination.pageSize },
        signal
      ),
  })
  const columns = useMemo<ColumnDef<SupplierLimitEvent>[]>(
    () => [
      {
        accessorKey: 'occurred_at',
        header: t('Time'),
        cell: ({ row }) => formatTimestampToDate(row.original.occurred_at),
      },
      {
        accessorKey: 'kind',
        header: t('Event'),
        cell: ({ row }) =>
          ({
            config_changed: t('Limit configuration changed'),
            daily_exhausted: t('Daily budget exhausted'),
            daily_restored: t('Daily budget restored'),
            daily_reset: t('Daily budget reset'),
            tpm_estimated: t('TPM estimated'),
            usage_corrected: t('Usage corrected'),
            cost_corrected: t('Procurement cost corrected'),
          })[row.original.kind] ?? t('Unknown'),
      },
      { accessorKey: 'budget_day', header: t('Budget date (Beijing time)') },
    ],
    [t]
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
    <DataTablePage
      table={table}
      columns={columns}
      fixedHeight={false}
      isLoading={query.isPending}
      isFetching={query.isFetching}
      emptyTitle={t('No events')}
      toolbarProps={null}
    />
  )
}
