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
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { type ReactNode, useState } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { getChannels } from '../api'
import { ChannelCostDialog } from '../components/dialogs/channel-cost-dialog'
import { UpstreamCredentialEditor } from '../components/dialogs/upstream-credential-editor'
import { UpstreamMetadataDialog } from '../components/dialogs/upstream-metadata-dialog'
import { channelSchema } from '../types'
import type { UpstreamCredential } from '../upstream-metadata-api'

let client: QueryClient
let credential: UpstreamCredential
const channel = channelSchema.parse({
  id: 42,
  type: 1,
  name: 'Primary',
  key: '',
  status: 1,
  created_time: 0,
  test_time: 0,
  response_time: 0,
  balance_updated_time: 0,
  cost_ratio: null,
})

function Wrapper(props: { children: ReactNode }) {
  return (
    <QueryClientProvider client={client}>{props.children}</QueryClientProvider>
  )
}

function CostEditor() {
  const [open, setOpen] = useState(true)
  return open ? (
    <ChannelCostDialog channel={channel} onClose={() => setOpen(false)} />
  ) : null
}

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'root', role: ROLE.SUPER_ADMIN })
  credential = {
    binding_id: 'binding-a',
    channel_id: 42,
    key_index: 0,
    credential_id: 'credential-a',
    credential_version_id: 'v1',
    alias: 'Primary upstream',
    supplier_id: '',
    tags: [],
    cost_ratio: null,
    effective_cost_ratio: 0.0015,
    cost_source: 'channel',
    cost_version_id: 'cost-v1',
    ownership_version_id: 'owner-v1',
    active: true,
  }
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data:
        url === '/api/upstream-supplier'
          ? [{ id: 's1', name: 'Supplier A' }]
          : [credential],
    },
  }))
  vi.spyOn(api, 'put').mockResolvedValue({ data: { success: true } })
  vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { id: 's2', name: 'Supplier B' } },
  })
})

afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
})

describe('upstream procurement metadata', () => {
  test('the channel list uses the registered trailing-slash API route', async () => {
    const params = { p: 1, page_size: 20 }
    const response = { success: true, data: { items: [channel], total: 1 } }
    vi.mocked(api.get).mockResolvedValueOnce({ data: response })

    expect(await getChannels(params)).toEqual(response)
    expect(api.get).toHaveBeenCalledWith('/api/channel/', { params })
  })

  test.each([
    ['0', 0],
    ['', null],
    ['0.0015', 0.0015],
  ] as const)(
    'channel cost %s preserves the explicit number or unknown value',
    async (input, expected) => {
      const user = userEvent.setup()
      render(<CostEditor />, { wrapper: Wrapper })
      if (input) {
        await user.type(
          screen.getByRole('spinbutton', { name: 'Cost Ratio' }),
          input
        )
      }
      await user.click(screen.getByRole('button', { name: 'Save' }))
      await waitFor(() =>
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
      )
      expect(api.put).toHaveBeenCalledWith(
        '/api/channel/',
        { id: 42, cost_ratio: expected },
        expect.anything()
      )
    }
  )

  test('invalid channel cost sends no request and business failure preserves the editor', async () => {
    const user = userEvent.setup()
    render(<CostEditor />, { wrapper: Wrapper })
    const input = screen.getByRole('spinbutton', { name: 'Cost Ratio' })
    await user.type(input, '-1')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(
      await screen.findByText(
        'Enter a cost ratio between 0 and 100, or leave empty.'
      )
    ).toBeVisible()
    expect(api.put).not.toHaveBeenCalled()
    await user.clear(input)
    vi.mocked(api.put).mockResolvedValueOnce({
      data: { success: false, message: 'Version conflict' },
    })
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Version conflict'
    )
    expect(screen.getByRole('dialog')).toBeVisible()
  })

  test('key metadata saves tags, supplier and explicit zero together without key material', async () => {
    const user = userEvent.setup()
    render(
      <UpstreamCredentialEditor
        credential={credential}
        suppliers={[{ id: 's1', name: 'Supplier A' }]}
        onClose={vi.fn()}
      />,
      { wrapper: Wrapper }
    )
    await user.click(
      screen.getByRole('combobox', { name: 'Upstream Supplier' })
    )
    await user.click(screen.getByRole('option', { name: 'Supplier A' }))
    await user.type(
      screen.getByRole('combobox', { name: 'Key Tags' }),
      '  region-us, primary, region-us,'
    )
    await user.keyboard('{Escape}')
    await user.type(
      screen.getByRole('spinbutton', { name: 'Key Cost Ratio' }),
      '0'
    )
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledWith(
        '/api/upstream-credential/credential-a',
        {
          alias: 'Primary upstream',
          supplier_id: 's1',
          tags: ['region-us', 'primary'],
          cost_ratio: 0,
        }
      )
    )
    expect(
      screen.getByText(
        'Metadata changes affect every channel using this upstream key.'
      )
    ).toBeVisible()
  })

  test('tag length validation is accessible and prevents metadata submission', async () => {
    const user = userEvent.setup()
    render(
      <UpstreamCredentialEditor
        credential={credential}
        suppliers={[]}
        onClose={vi.fn()}
      />,
      { wrapper: Wrapper }
    )
    const input = screen.getByRole('combobox', { name: 'Key Tags' })
    await user.type(input, `${'x'.repeat(41)},`)
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(
      await screen.findByText(
        'Use up to 20 tags, with no more than 40 characters each.'
      )
    ).toBeVisible()
    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(input).toHaveAccessibleDescription(
      /Use up to 20 tags, with no more than 40 characters each./
    )
    expect(api.put).not.toHaveBeenCalled()
  })

  test('new supplier is selected for the metadata save and blank key cost inherits', async () => {
    const user = userEvent.setup()
    render(
      <UpstreamCredentialEditor
        credential={credential}
        suppliers={[]}
        onClose={vi.fn()}
      />,
      { wrapper: Wrapper }
    )
    await user.type(
      screen.getByRole('textbox', { name: 'New Supplier Name' }),
      ' Supplier B '
    )
    await user.click(screen.getByRole('button', { name: 'Add Supplier' }))
    await waitFor(() =>
      expect(api.post).toHaveBeenCalledWith('/api/upstream-supplier', {
        name: 'Supplier B',
      })
    )
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()
    )
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledWith(
        '/api/upstream-credential/credential-a',
        {
          alias: 'Primary upstream',
          supplier_id: 's2',
          tags: [],
          cost_ratio: null,
        }
      )
    )
  })

  test('read-only metadata uses two bulk reads, shows full decimal costs and hides edits', async () => {
    useAuthStore.getState().auth.setUser({
      id: 2,
      username: 'reader',
      role: ROLE.ADMIN,
      permissions: {
        admin_permissions: { channel: { read: true, write: false } },
      },
    })
    render(<UpstreamMetadataDialog channel={channel} onClose={vi.fn()} />, {
      wrapper: Wrapper,
    })
    const table = await screen.findByRole('table')
    expect(within(table).getByText('0.0015')).toBeVisible()
    expect(within(table).getByText('Inherited from Channel')).toBeVisible()
    expect(
      within(table).queryByRole('button', { name: 'Edit' })
    ).not.toBeInTheDocument()
    expect(api.get).toHaveBeenCalledTimes(2)
    expect(api.get).toHaveBeenCalledWith(
      '/api/channel/42/upstream-credentials',
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    expect(api.put).not.toHaveBeenCalled()
  })
})
