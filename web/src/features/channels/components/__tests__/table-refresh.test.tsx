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
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import type { Channel } from '../../types'
import { ChannelsProvider } from '../channels-provider'
import { ChannelsTable } from '../channels-table'

const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})
const clients: QueryClient[] = []

function channel(name: string): Channel {
  return {
    id: 3,
    type: 1,
    key: '',
    status: 1,
    name,
    created_time: 0,
    test_time: 0,
    response_time: 0,
    other: '',
    balance: 0,
    balance_updated_time: 0,
    models: 'gpt-4o',
    group: 'vip',
    used_quota: 0,
    other_info: '',
    remark: '',
    max_input_tokens: 0,
    channel_info: {
      is_multi_key: false,
      multi_key_size: 0,
      multi_key_polling_index: 0,
      multi_key_mode: 'random',
    },
    settings: '{}',
  }
}

beforeEach(() => {
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
  localStorage.clear()
})

afterEach(() => {
  cleanup()
  localStorage.clear()
  clients.splice(0).forEach((client) => client.clear())
  vi.restoreAllMocks()
})

async function renderChannelsPage(
  searchGate: () => Promise<void>,
  statsGate: () => Promise<void> = async () => {},
  currentChannels: () => Channel[] = () => [channel('prod')]
) {
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/channel/recent_stats') {
      await statsGate()
      return {
        data: {
          success: true,
          data: {
            ready: true,
            refreshed_at: 0,
            window_seconds: 600,
            scope: 'instance',
            items: [],
          },
        },
      }
    }
    if (url === '/api/channel/search') {
      await searchGate()
      return {
        data: {
          success: true,
          data: { items: currentChannels(), total: 1, type_counts: {} },
        },
      }
    }
    return { data: { success: true, data: ['vip'] } }
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const root = createRootRoute()
  const auth = createRoute({ getParentRoute: () => root, id: '_authenticated' })
  const channelsRoute = createRoute({
    getParentRoute: () => auth,
    path: 'channels/',
    component: () => (
      <ChannelsProvider>
        <ChannelsTable />
      </ChannelsProvider>
    ),
  })
  const initialEntry =
    '/channels/?filter=prod&group=%5B%22vip%22%5D&status=%5B%22enabled%22%5D&model=gpt-4o'
  const router = createRouter({
    routeTree: root.addChildren([auth.addChildren([channelsRoute])]),
    history: createMemoryHistory({ initialEntries: [initialEntry] }),
  })
  await router.load()
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  expect((await screen.findAllByText('prod')).length).toBeGreaterThan(0)
  return get
}

it('refetches the filtered channel list and current-page statistics together and stays busy until both finish', async () => {
  let gate = Promise.resolve()
  let statsGate = Promise.resolve()
  const get = await renderChannelsPage(
    () => gate,
    () => statsGate
  )
  const searchCalls = () =>
    get.mock.calls.filter(([url]) => url === '/api/channel/search')
  const statsCalls = () =>
    get.mock.calls.filter(([url]) => url === '/api/channel/recent_stats')
  const refresh = screen.getByRole('button', { name: 'Refresh' })
  expect(searchCalls()).toHaveLength(1)
  await waitFor(() => expect(statsCalls()).toHaveLength(1))
  expect(refresh).toHaveAttribute('aria-busy', 'false')

  let release!: () => void
  gate = new Promise((resolve) => {
    release = resolve
  })
  let releaseStats!: () => void
  statsGate = new Promise((resolve) => {
    releaseStats = resolve
  })
  await userEvent.click(refresh)

  await waitFor(() => expect(searchCalls()).toHaveLength(2))
  await waitFor(() => expect(statsCalls()).toHaveLength(2))
  expect(statsCalls()[1][1]?.params).toEqual({ channel_ids: '3' })
  expect(searchCalls()[1][1]?.params).toEqual(searchCalls()[0][1]?.params)
  expect(refresh).toHaveAttribute('aria-busy', 'true')
  release()
  await waitFor(() => expect(refresh).toHaveAttribute('aria-busy', 'true'))
  releaseStats()
  await waitFor(() => expect(refresh).toHaveAttribute('aria-busy', 'false'))
})

it('fetches fresh statistics when refresh changes the page back to a recently cached channel scope', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(1_900_000_000_000)
  let rows = [channel('prod')]
  const get = await renderChannelsPage(
    async () => {},
    async () => {},
    () => rows
  )
  const refresh = screen.getByRole('button', { name: 'Refresh' })
  const originalScopeRequests = () =>
    get.mock.calls.filter(
      ([url, config]) =>
        url === '/api/channel/recent_stats' &&
        config?.params?.channel_ids === '3'
    )
  await waitFor(() => expect(refresh).toHaveAttribute('aria-busy', 'false'))
  expect(originalScopeRequests()).toHaveLength(1)

  rows = [{ ...channel('prod-next'), id: 4 }]
  await userEvent.click(refresh)
  expect(await screen.findByText('prod-next')).toBeVisible()
  await waitFor(() => expect(refresh).toHaveAttribute('aria-busy', 'false'))
  expect(originalScopeRequests()).toHaveLength(2)

  rows = [channel('prod')]
  await userEvent.click(refresh)
  expect(await screen.findByText('prod')).toBeVisible()
  await waitFor(() => expect(refresh).toHaveAttribute('aria-busy', 'false'))
  // The clock has not advanced: the original scope is still inside staleTime.
  // Refresh must invalidate that inactive cache before it becomes visible.
  expect(originalScopeRequests()).toHaveLength(3)
})

it('shows health in a separate column after the name and allows hiding it', async () => {
  localStorage.setItem('channels:view-mode', 'table')
  await renderChannelsPage(async () => {})

  const headers = screen.getAllByRole('columnheader')
  const nameHeader = screen.getByRole('columnheader', { name: /^Name\b/ })
  const healthHeader = screen.getByRole('columnheader', {
    name: /^Channel health \(1h\)/,
  })
  expect(headers.indexOf(healthHeader)).toBe(headers.indexOf(nameHeader) + 1)
  const nameCell = screen.getByRole('cell', { name: 'prod' })
  expect(within(nameCell).queryByRole('group')).not.toBeInTheDocument()
  const health = screen.getByRole('group', { name: 'Channel health (1h)' })
  expect(within(health).getAllByRole('img')).toHaveLength(6)

  await userEvent.click(screen.getByRole('button', { name: 'View' }))
  await userEvent.click(
    screen.getByRole('menuitemcheckbox', { name: 'Channel health (1h)' })
  )
  expect(
    screen.queryByRole('columnheader', { name: /^Channel health \(1h\)/ })
  ).not.toBeInTheDocument()
  expect(screen.getByRole('cell', { name: 'prod' })).toBeVisible()
})

it('restores the health column width and saves keyboard width changes independently of the name', async () => {
  localStorage.setItem('channels:view-mode', 'table')
  localStorage.setItem(
    'channels:column-sizing',
    JSON.stringify({ channel_health: 300, name: 280 })
  )
  await renderChannelsPage(async () => {})

  const healthHeader = screen.getByRole('columnheader', {
    name: /^Channel health \(1h\)/,
  })
  expect(healthHeader).toHaveStyle({ width: '300px' })
  const resizer = within(healthHeader).getByRole('separator', {
    name: 'Resize column',
  })
  resizer.focus()
  await userEvent.keyboard('{ArrowLeft}')
  expect(healthHeader).toHaveStyle({ width: '290px' })
  expect(screen.getByRole('columnheader', { name: /^Name\b/ })).toHaveStyle({
    width: '280px',
  })
  await waitFor(() => {
    expect(
      JSON.parse(localStorage.getItem('channels:column-sizing') ?? 'null')
    ).toEqual(expect.objectContaining({ channel_health: 290, name: 280 }))
  })
})
