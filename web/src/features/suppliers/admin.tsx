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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { ColumnDef } from '@tanstack/react-table'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { DataTablePage, useDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  SecureVerificationDialog,
  useSecureVerification,
} from '@/features/auth/secure-verification'
import { manageUser } from '@/features/users/api'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import {
  getSuppliers,
  getSupplierTemplates,
  saveSupplier,
  supplyQueryKey,
} from './api'
import { ChannelEditor } from './components/channel-editor'
import { SupplierEditor } from './components/supplier-editor'
import { supplierChannelTemplates } from './lib/templates'
import type { SupplierAccount } from './types'

export function SuppliersAdmin() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const user = useAuthStore((s) => s.auth.user)
  const userId = user?.id
  const canManage = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.SENSITIVE_WRITE
  )
  const verification = useSecureVerification()
  const [search, setSearch] = useState('')
  const [channelSupplier, setChannelSupplier] =
    useState<SupplierAccount | null>(null)
  const [editing, setEditing] = useState<SupplierAccount | 'new' | null>(null)
  const query = useQuery({
    queryKey: [...supplyQueryKey, userId, 'admin', 'accounts'],
    queryFn: ({ signal }) => getSuppliers(signal),
  })
  const templates = useQuery({
    queryKey: [...supplyQueryKey, userId, 'admin', 'templates'],
    queryFn: ({ signal }) => getSupplierTemplates(signal),
    enabled: channelSupplier !== null,
  })
  const mutation = useMutation({
    mutationFn: async (operation: {
      supplier: SupplierAccount
      action: 'login' | 'supply'
    }) => {
      const supplier = operation.supplier
      if (operation.action === 'login') {
        const action = supplier.login_enabled ? 'disable' : 'enable'
        const proof = await verification.requestVerification({
          scope: 'admin.user.manage',
          context: { user_id: supplier.user_id, action },
          title: t('Verify supplier login access'),
        })
        if (!proof) return false
        requireServerSuccess(
          await manageUser(supplier.user_id, action, proof.proof_token)
        )
      } else {
        const proof = await verification.requestVerification({
          scope: 'supplier.manage',
          context: { supplier_id: supplier.id, version: supplier.revision },
          title: t('Verify supplier settings'),
        })
        if (!proof) return false
        await saveSupplier(
          supplier.id,
          { ...supplier, supply_enabled: !supplier.supply_enabled },
          proof.proof_token
        )
      }
      return true
    },
    onSuccess: (saved) => {
      if (saved) {
        toast.success(t('Saved successfully'))
        void queryClient.invalidateQueries({ queryKey: supplyQueryKey })
      }
    },
  })
  const columns = useMemo<ColumnDef<SupplierAccount>[]>(
    () => [
      { accessorKey: 'name', header: t('Supplier name') },
      {
        accessorKey: 'username',
        header: t('Dedicated account'),
        cell: ({ row }) => row.original.username || t('Not bound'),
      },
      {
        accessorKey: 'supply_enabled',
        header: t('Supply'),
        cell: ({ row }) => (
          <Badge variant='outline'>
            {row.original.supply_enabled ? t('Enabled') : t('Paused')}
          </Badge>
        ),
      },
      {
        accessorKey: 'login_enabled',
        header: t('Login'),
        cell: ({ row }) => (
          <Badge variant='outline'>
            {row.original.login_enabled ? t('Enabled') : t('Disabled')}
          </Badge>
        ),
      },
      { accessorKey: 'max_channels', header: t('Maximum channels') },
      { accessorKey: 'group', header: t('Group') },
      {
        id: 'actions',
        header: t('Actions'),
        cell: ({ row }) => (
          <div className='flex flex-wrap gap-1'>
            <Button
              variant='outline'
              size='sm'
              disabled={!canManage}
              onClick={() => setChannelSupplier(row.original)}
            >
              {t('Add Channel')}
            </Button>
            <Button
              variant='outline'
              size='sm'
              disabled={!canManage || mutation.isPending}
              onClick={() => setEditing(row.original)}
            >
              {t('Edit')}
            </Button>
            <Button
              variant='ghost'
              size='sm'
              disabled={!canManage || mutation.isPending}
              onClick={() =>
                mutation.mutate({ supplier: row.original, action: 'supply' })
              }
            >
              {row.original.supply_enabled
                ? t('Pause supply')
                : t('Resume supply')}
            </Button>
            <Button
              variant='ghost'
              size='sm'
              disabled={mutation.isPending || !row.original.user_id}
              onClick={() =>
                mutation.mutate({ supplier: row.original, action: 'login' })
              }
            >
              {row.original.login_enabled
                ? t('Disable login')
                : t('Enable login')}
            </Button>
          </div>
        ),
      },
    ],
    [t, mutation, canManage]
  )
  const { table } = useDataTable({
    data: (query.data?.items ?? []).filter((supplier) =>
      `${supplier.name} ${supplier.username ?? ''}`
        .toLocaleLowerCase()
        .includes(search.trim().toLocaleLowerCase())
    ),
    columns,
    enableRowSelection: false,
    withPaginationRowModel: true,
    columnVisibilityStorageKey: false,
    columnSizingStorageKey: false,
  })
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>
        {t('Channel Suppliers')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button variant='outline' render={<Link to='/users' />}>
          {t('Users')}
        </Button>
        <Button disabled={!canManage} onClick={() => setEditing('new')}>
          {t('Add supplier')}
        </Button>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <p className='text-muted-foreground mb-4 text-sm'>
          {t(
            'Login access and supply permission are independent. Midnight budget recovery changes neither.'
          )}
        </p>
        {query.isError ? (
          <ErrorState onRetry={() => void query.refetch()} />
        ) : (
          <DataTablePage
            table={table}
            columns={columns}
            isLoading={query.isPending}
            isFetching={query.isFetching}
            fixedHeight={false}
            toolbar={
              <Input
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                placeholder={t('Filter suppliers by name or account')}
                aria-label={t('Filter suppliers by name or account')}
                className='w-full sm:max-w-sm'
              />
            }
            emptyTitle={t('No suppliers')}
          />
        )}
        {editing && (
          <SupplierEditor
            key={editing === 'new' ? 'new' : editing.id}
            supplier={editing === 'new' ? undefined : editing}
            configuredSupplierIds={query.data?.items.map((item) => item.id)}
            onClose={() => setEditing(null)}
          />
        )}
        {channelSupplier && templates.data && (
          <ChannelEditor
            audience='admin'
            supplierId={channelSupplier.id}
            templates={supplierChannelTemplates(
              channelSupplier,
              templates.data ?? []
            )}
            onClose={() => setChannelSupplier(null)}
          />
        )}
        {channelSupplier && !templates.data && (
          <Dialog
            open
            title={t('Add Channel')}
            onOpenChange={(open) => !open && setChannelSupplier(null)}
          >
            {templates.isError ? (
              <ErrorState onRetry={() => void templates.refetch()} />
            ) : (
              <LoadingState />
            )}
          </Dialog>
        )}
        <SecureVerificationDialog {...verification.dialogProps} />
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
