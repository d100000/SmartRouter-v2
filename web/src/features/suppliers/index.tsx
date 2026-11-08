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
import { lazy, Suspense } from 'react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useAuthStore } from '@/stores/auth-store'

import { getSupplierSelf, supplyQueryKey } from './api'
import { SupplyChannelList } from './components/channel-list'
import { SupplyLogs } from './components/logs'
import { SupplyOverviewPanel } from './components/overview'

const AuditLogViewer = lazy(() =>
  import('@/features/usage-logs/audit/components/audit-log-viewer').then(
    (module) => ({ default: module.AuditLogViewer })
  )
)

export function SupplierWorkspace() {
  const { t } = useTranslation()
  const userId = useAuthStore((s) => s.auth.user?.id)
  const self = useQuery({
    queryKey: [...supplyQueryKey, userId, 'self'],
    queryFn: ({ signal }) => getSupplierSelf(signal),
  })
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>
        {self.data?.supplier.name ?? t('Supplier Workspace')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Content>
        {self.isPending && <LoadingState />}
        {self.isError && <ErrorState onRetry={() => void self.refetch()} />}
        {self.data && (
          <div className='space-y-4'>
            {!self.data.supplier.supply_enabled && (
              <Alert>
                <AlertDescription>
                  {t(
                    'Supply is paused by the platform. You can still manage your channels.'
                  )}
                </AlertDescription>
              </Alert>
            )}
            <Tabs defaultValue='channels'>
              <TabsList className='h-auto flex-wrap'>
                <TabsTrigger value='channels'>{t('My Channels')}</TabsTrigger>
                <TabsTrigger value='overview'>
                  {t('Overview and stability')}
                </TabsTrigger>
                <TabsTrigger value='logs'>{t('Supply logs')}</TabsTrigger>
                <TabsTrigger value='operations'>
                  {t('Operation history')}
                </TabsTrigger>
              </TabsList>
              <TabsContent value='channels'>
                <SupplyChannelList
                  mode='supplier'
                  templates={self.data.templates}
                />
              </TabsContent>
              <TabsContent value='overview'>
                <SupplyOverviewPanel mode='supplier' />
              </TabsContent>
              <TabsContent value='operations'>
                <Suspense fallback={<LoadingState />}>
                  <AuditLogViewer scope='self' />
                </Suspense>
              </TabsContent>
              <TabsContent value='logs'>
                <SupplyLogs />
              </TabsContent>
            </Tabs>
          </div>
        )}
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
export function ChannelViewerWorkspace() {
  const { t } = useTranslation()
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Channels')}</SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <div className='space-y-4'>
          <Alert>
            <AlertDescription>
              {t(
                'Channel information is read-only. Your personal API keys and account features remain available.'
              )}
            </AlertDescription>
          </Alert>
          <Tabs defaultValue='channels'>
            <TabsList>
              <TabsTrigger value='channels'>{t('Channels')}</TabsTrigger>
              <TabsTrigger value='overview'>
                {t('Overview and stability')}
              </TabsTrigger>
            </TabsList>
            <TabsContent value='channels'>
              <SupplyChannelList mode='viewer' />
            </TabsContent>
            <TabsContent value='overview'>
              <SupplyOverviewPanel mode='viewer' />
            </TabsContent>
          </Tabs>
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
