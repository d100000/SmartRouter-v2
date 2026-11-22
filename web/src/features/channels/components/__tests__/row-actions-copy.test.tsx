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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { Row } from '@tanstack/react-table'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { TooltipProvider } from '@/components/ui/tooltip'
import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { channelSchema, type Channel } from '../../types'
import { ChannelsProvider } from '../channels-provider'
import { DataTableRowActions } from '../data-table-row-actions'

const originalAuth = useAuthStore.getState().auth

beforeEach(() => {
  useAuthStore.setState({
    auth: {
      ...originalAuth,
      user: { id: 1, username: 'root', role: ROLE.SUPER_ADMIN },
    },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    throw new Error(`Unexpected GET ${url}`)
  })
})

afterEach(() => {
  cleanup()
  useAuthStore.setState({ auth: originalAuth })
  vi.restoreAllMocks()
})

test.each([
  { name: 'a platform channel', disabled: false },
  {
    name: 'a supplier channel',
    owner_supplier_id: 'supplier-a',
    disabled: true,
  },
  { name: 'a write-only channel', secrets_write_only: true, disabled: true },
])(
  '"Copy Channel" is disabled=$disabled for $name',
  async (item: {
    disabled: boolean
    owner_supplier_id?: string
    secrets_write_only?: boolean
  }) => {
    const channel: Channel = channelSchema.parse({
      id: 3,
      name: 'Channel',
      type: 1,
      key: '',
      status: 1,
      created_time: 1,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      owner_supplier_id: item.owner_supplier_id,
      secrets_write_only: item.secrets_write_only,
    })
    const user = userEvent.setup()
    render(
      <QueryClientProvider client={new QueryClient()}>
        <ChannelsProvider>
          <TooltipProvider>
            <DataTableRowActions row={{ original: channel } as Row<Channel>} />
          </TooltipProvider>
        </ChannelsProvider>
      </QueryClientProvider>
    )

    await user.click(screen.getByRole('button', { name: 'Open menu' }))
    const copy = await screen.findByRole('menuitem', { name: 'Copy Channel' })
    if (item.disabled) {
      expect(copy).toHaveAttribute('aria-disabled', 'true')
    } else {
      expect(copy).not.toHaveAttribute('aria-disabled', 'true')
    }
  }
)
