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
import type { NavGroup } from '@/components/layout/types'
import type { AuthUser } from '@/stores/auth-store'

import { ROLE } from './roles'

export function isSupplier(user: AuthUser | null | undefined): boolean {
  return user?.role === ROLE.SUPPLIER
}

export function isChannelViewer(user: AuthUser | null | undefined): boolean {
  return user?.role === ROLE.USER && user.channel_viewer === true
}

export function canReadChannels(user: AuthUser | null | undefined): boolean {
  return Boolean(user && (user.role >= ROLE.ADMIN || isChannelViewer(user)))
}

export function isSupplierRoute(path: string): boolean {
  return ['/supplier', '/profile', '/security', '/errors'].some(
    (prefix) => path === prefix || path.startsWith(`${prefix}/`)
  )
}

export function filterRoleNavigation(
  groups: NavGroup[],
  user: AuthUser | null | undefined
): NavGroup[] {
  const role = user?.role ?? ROLE.GUEST
  return groups
    .filter((group) => {
      if (isSupplier(user)) {
        return group.id === 'supply' || group.id === 'personal'
      }
      if (group.id === 'supply') return false
      if (group.id === 'channel-viewer') return isChannelViewer(user)
      return group.id !== 'admin' || role >= ROLE.ADMIN
    })
    .map((group) => ({
      ...group,
      items: group.items.filter((item) => {
        if (isSupplier(user) && 'url' in item && item.url === '/wallet') {
          return false
        }
        return item.requiredRole === undefined || role >= item.requiredRole
      }),
    }))
    .filter((group) => group.items.length > 0)
}
