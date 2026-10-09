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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import type { Channel } from '../../types'
import {
  getUpstreamCredentials,
  getUpstreamSuppliers,
  type UpstreamCredential,
} from '../../upstream-metadata-api'
import { UpstreamCredentialEditor } from './upstream-credential-editor'

export function UpstreamMetadataDialog(props: {
  channel: Channel
  onClose: () => void
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const user = useAuthStore((state) => state.auth.user)
  const canRead = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.READ
  )
  const canWrite = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.WRITE
  )
  const [selected, setSelected] = useState<UpstreamCredential | null>(null)
  const credentials = useQuery({
    queryKey: ['upstream-credentials', props.channel.id],
    queryFn: ({ signal }) => getUpstreamCredentials(props.channel.id, signal),
    enabled: canRead,
    staleTime: 30_000,
    meta: { errorToast: false },
  })
  const suppliers = useQuery({
    queryKey: ['upstream-suppliers'],
    queryFn: ({ signal }) => getUpstreamSuppliers(signal),
    enabled: canRead,
    staleTime: 30_000,
    meta: { errorToast: false },
  })
  const pending = credentials.isPending || suppliers.isPending
  const failed = credentials.isError || suppliers.isError
  const sources = {
    credential: t('Key Override'),
    channel: t('Inherited from Channel'),
    unknown: t('Unknown Cost'),
  }
  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => !open && props.onClose()}
        title={t('Upstream Key Metadata')}
        description={props.channel.name}
        contentClassName='sm:max-w-4xl'
        footer={
          <Button variant='outline' onClick={props.onClose}>
            {t('Close')}
          </Button>
        }
      >
        {!canRead && <ErrorState title={t('Access Forbidden')} />}
        {canRead && pending && <LoadingState />}
        {failed && (
          <ErrorState
            description={t('Unable to load upstream key metadata.')}
            onRetry={() => {
              void credentials.refetch()
              void suppliers.refetch()
            }}
          />
        )}
        {canRead &&
          !pending &&
          !failed &&
          (credentials.data?.length ? (
            <div className='overflow-x-auto'>
              <StaticDataTable
                data={credentials.data}
                getRowKey={(row) => row.binding_id}
                tableClassName='min-w-[680px]'
                columns={[
                  {
                    id: 'name',
                    header: t('Key Display Name'),
                    cell: (row) =>
                      row.alias ||
                      t('Upstream Key #{{index}}', {
                        index: formatNumber(row.key_index + 1, locale),
                      }),
                  },
                  {
                    id: 'supplier',
                    header: t('Upstream Supplier'),
                    cell: (row) =>
                      suppliers.data?.find(
                        (supplier) => supplier.id === row.supplier_id
                      )?.name ?? t('Unassigned'),
                  },
                  {
                    id: 'tags',
                    header: t('Key Tags'),
                    cell: (row) => (
                      <div className='flex max-w-64 flex-wrap gap-1'>
                        {row.tags.map((tag) => (
                          <Badge key={tag} variant='outline'>
                            {tag}
                          </Badge>
                        ))}
                      </div>
                    ),
                  },
                  {
                    id: 'cost',
                    header: t('Effective Cost Ratio'),
                    cell: (row) => (
                      <div>
                        <span className='font-mono'>
                          {row.effective_cost_ratio == null
                            ? '—'
                            : formatNumber(row.effective_cost_ratio, locale, {
                                maximumFractionDigits: 20,
                              })}
                        </span>
                        <p className='text-muted-foreground text-xs'>
                          {sources[row.cost_source]}
                        </p>
                      </div>
                    ),
                  },
                  {
                    id: 'actions',
                    header: t('Actions'),
                    cell: (row) =>
                      canWrite && (
                        <Button
                          size='sm'
                          variant='outline'
                          onClick={() => setSelected(row)}
                        >
                          {t('Edit')}
                        </Button>
                      ),
                  },
                ]}
              />
            </div>
          ) : (
            <EmptyState title={t('No upstream keys available')} />
          ))}
      </Dialog>
      {selected && (
        <UpstreamCredentialEditor
          key={selected.credential_id}
          credential={selected}
          suppliers={suppliers.data ?? []}
          onClose={() => setSelected(null)}
        />
      )}
    </>
  )
}
