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
import { RefreshCw } from 'lucide-react'
import { lazy, Suspense, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { getChannelOverview } from './api'
import { OverviewCoverage } from './components/overview-coverage'
import {
  OverviewFiltersBar,
  type OverviewFilters,
} from './components/overview-filters'
import { OverviewSummary } from './components/overview-summary'
import { OverviewTable } from './components/overview-table'
import { getOverviewObjectLabel } from './lib/object-label'

const OverviewCharts = lazy(() => import('./components/overview-charts'))

export function ChannelOverview() {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canRead =
    (user?.role ?? ROLE.GUEST) >= ROLE.ADMIN &&
    hasPermission(
      user,
      ADMIN_PERMISSION_RESOURCES.CHANNEL,
      ADMIN_PERMISSION_ACTIONS.READ
    )
  const [filters, setFilters] = useState<OverviewFilters>({
    dimension: 'channel',
    days: 7,
    object: '',
  })
  const query = useQuery({
    queryKey: ['channel-overview', filters],
    queryFn: ({ signal }) => getChannelOverview(filters, signal),
    enabled: canRead,
    staleTime: 30_000,
    retry: false,
    meta: { errorToast: false },
  })
  const data = query.data
  const objectOptions = (data?.options ?? [])
    .filter((object) => object.id !== '')
    .map((object) => ({
      value: object.id,
      label: getOverviewObjectLabel(
        filters.dimension,
        object.name,
        object.id,
        t
      ),
    }))
  if (
    filters.object &&
    !objectOptions.some((option) => option.value === filters.object)
  ) {
    const selectedRow = data?.ranking.find((row) => row.id === filters.object)
    objectOptions.push({
      value: filters.object,
      label: getOverviewObjectLabel(
        filters.dimension,
        selectedRow?.name ?? '',
        filters.object,
        t
      ),
    })
  }
  const rankingTitles = {
    channel: t('Channel Ranking'),
    group: t('Group Ranking'),
    key: t('Upstream Key Ranking'),
    supplier: t('Upstream Supplier Ranking'),
  }
  return (
    <SectionPageLayout stackActionsOnMobile>
      <SectionPageLayout.Title>{t('Channel Overview')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button
          size='sm'
          variant='outline'
          onClick={() => void query.refetch()}
          disabled={!canRead || query.isFetching}
        >
          <RefreshCw
            className={query.isFetching ? 'animate-spin' : ''}
            aria-hidden
          />
          {t('Refresh')}
        </Button>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='min-w-0 space-y-4'>
          <OverviewFiltersBar
            filters={filters}
            objects={objectOptions}
            onChange={setFilters}
          />
          {!canRead && (
            <ErrorState
              title={t('Access Forbidden')}
              description={t(
                'You do not have permission to perform this action.'
              )}
            />
          )}
          {canRead && query.isPending && <LoadingState />}
          {query.isError && (
            <ErrorState
              description={t('Unable to load channel analytics.')}
              onRetry={() => void query.refetch()}
            />
          )}
          {canRead && data && !query.isError && (
            <>
              <OverviewSummary metrics={data.summary} />
              <OverviewCoverage snapshot={data} />
              {data.summary.requests === 0 && data.summary.attempts === 0 ? (
                <EmptyState
                  title={t('No data in this time range')}
                  description={t(
                    'New relay calls will appear after analytics collection starts.'
                  )}
                />
              ) : (
                <>
                  <Suspense fallback={<LoadingState />}>
                    <OverviewCharts snapshot={data} />
                  </Suspense>
                  <OverviewTable
                    title={rankingTitles[filters.dimension]}
                    rows={data.ranking.map((row) => ({
                      ...row,
                      name: getOverviewObjectLabel(
                        filters.dimension,
                        row.name,
                        row.id,
                        t
                      ),
                    }))}
                    onSelect={(object) => setFilters({ ...filters, object })}
                    maxRows={data.limits.max_rows}
                  />
                </>
              )}
            </>
          )}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
