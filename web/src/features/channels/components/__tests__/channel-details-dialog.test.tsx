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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { TooltipProvider } from '@/components/ui/tooltip'
import type { UsageLog } from '@/features/usage-logs/data/schema'
import { api } from '@/lib/api'

import type { Channel } from '../../types'
import { ChannelDetailsButton } from '../channel-details-button'
import { ChannelRecentStatsProvider } from '../channel-recent-stats'
import { ChannelsProvider, useChannels } from '../channels-provider'
import { ChannelDetailsDialog } from '../dialogs/channel-details-dialog'

const now = Math.floor(Date.now() / 1000)
const clients: QueryClient[] = []

const channel: Channel = {
  id: 3,
  type: 1,
  key: '',
  status: 1,
  name: 'prod',
  created_time: 0,
  test_time: 0,
  response_time: 0,
  other: '',
  balance: 0,
  balance_updated_time: 0,
  models: 'gpt-4o',
  group: 'default',
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

function usageLog(fields: Partial<UsageLog>): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: now - 60,
    type: 2,
    content: '',
    username: 'alice',
    token_name: 'prod-web',
    model_name: 'gpt-4o',
    quota: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    use_time: 0,
    is_stream: true,
    channel: 3,
    channel_name: 'prod',
    token_id: 5,
    group: 'default',
    ip: '',
    other: '',
    request_id: '',
    upstream_request_id: '',
    ...fields,
  }
}

const succeeded = usageLog({
  id: 11,
  type: 2,
  created_at: now - 60,
  use_time: 4,
  prompt_tokens: 1204,
  completion_tokens: 356,
  quota: 2950,
  request_id: 'req-succeeded',
  other: JSON.stringify({ frt: 700, admin_info: { use_channel: ['3'] } }),
})
const failed = usageLog({
  id: 12,
  type: 5,
  created_at: now - 30,
  content: 'Rate limit reached',
  request_id: 'req-failed',
  other: JSON.stringify({
    status_code: 429,
    error_code: 'rate_limit_exceeded',
    admin_info: { use_channel: ['5', '3'] },
  }),
})

// What the gateway answers: the channel's statistics and its two kinds of log.
function mockGateway(answer: {
  succeeded: UsageLog[]
  failed: UsageLog[]
  logged: { successes: boolean; failures: boolean }
}) {
  return vi.spyOn(api, 'get').mockImplementation(async (url) => {
    const [path, query] = String(url).split('?')
    if (path === '/api/channel/recent_stats') {
      return {
        data: {
          success: true,
          data: {
            ready: true,
            refreshed_at: now,
            window_seconds: 600,
            scope: 'instance',
            collected_since: now - 3600,
            history_window_seconds: 3600,
            bucket_seconds: 600,
            latency_window_seconds: 300,
            log_recording_enabled: answer.logged.successes,
            error_recording_enabled: answer.logged.failures,
            items: [
              {
                channel_id: 3,
                requests: 50,
                successes: 49,
                success_rate: 0.98,
                requests_1h: 50,
                successes_1h: 49,
                success_rate_1h: 0.98,
                history: [],
                ttft_samples_5m: 2,
                ttft_sum_ms_5m: 1640,
                avg_ttft_ms_5m: 820,
                response_samples_5m: 1,
                response_sum_ms_5m: 4100,
                avg_response_ms_5m: 4100,
              },
            ],
          },
        },
      }
    }
    if (path === '/api/log') {
      const items =
        new URLSearchParams(query).get('type') === '5'
          ? answer.failed
          : answer.succeeded
      return {
        data: {
          success: true,
          data: { items, total: items.length, page: 1, page_size: 20 },
        },
      }
    }
    return { data: { success: true, data: [] } }
  })
}

function logSearches(get: ReturnType<typeof mockGateway>) {
  return get.mock.calls
    .map(([url]) => String(url))
    .filter((url) => url.startsWith('/api/log?'))
    .map((url) => new URLSearchParams(url.split('?')[1]))
}

// The list renders the dialog inside its statistics, as this page does.
function ChannelPage(props: { row: Channel }) {
  const { open, setOpen, currentRow } = useChannels()
  return (
    <ChannelRecentStatsProvider channels={[channel]}>
      <h1>Channel list</h1>
      <ChannelDetailsButton channel={props.row} />
      {open === 'channel-details' && currentRow && (
        <ChannelDetailsDialog
          key={currentRow.id}
          channel={currentRow}
          onClose={() => setOpen(null)}
        />
      )}
    </ChannelRecentStatsProvider>
  )
}

async function renderChannelPage(row: Channel = channel) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const root = createRootRoute()
  const channelsRoute = createRoute({
    getParentRoute: () => root,
    path: 'channels',
    component: () => (
      <ChannelsProvider>
        <ChannelPage row={row} />
      </ChannelsProvider>
    ),
  })
  const usageLogsRoute = createRoute({
    getParentRoute: () => root,
    path: 'usage-logs/$section',
    component: () => <p>Usage logs page</p>,
  })
  const router = createRouter({
    routeTree: root.addChildren([channelsRoute, usageLogsRoute]),
    history: createMemoryHistory({ initialEntries: ['/channels'] }),
  })
  await router.load()
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>
    </QueryClientProvider>
  )
  expect(await screen.findByText('Channel list')).toBeVisible()
  return router
}

beforeEach(() => {
  localStorage.clear()
})

afterEach(() => {
  clients.splice(0).forEach((client) => client.clear())
  localStorage.clear()
})

test('a channel shows its latest requests and opens the usage log filtered by it', async () => {
  const get = mockGateway({
    succeeded: [succeeded],
    failed: [failed],
    logged: { successes: true, failures: true },
  })
  const router = await renderChannelPage()
  const user = userEvent.setup()

  await user.click(screen.getByRole('button', { name: 'Details of prod' }))
  const dialog = await screen.findByRole('dialog', { name: 'Channel details' })
  expect(within(dialog).getByText('prod')).toBeVisible()

  // The row's own figures.
  expect(await within(dialog).findByText('98%')).toBeVisible()
  expect(within(dialog).getByText('820ms')).toBeVisible()
  expect(within(dialog).getByText('4.1s')).toBeVisible()

  // Both kinds of request of this channel in the last hour, newest first.
  await within(dialog).findByText('Failed 429')
  const searches = logSearches(get)
  expect(searches.map((search) => search.get('type')).sort()).toEqual([
    '2',
    '5',
  ])
  for (const search of searches) {
    expect(search.get('channel')).toBe('3')
    expect(search.get('page_size')).toBe('20')
    const since = Number(search.get('start_timestamp'))
    expect(
      Math.abs(since - (Math.floor(Date.now() / 1000) - 3600))
    ).toBeLessThan(120)
  }
  const rows = within(dialog).getAllByRole('row').slice(1)
  expect(rows).toHaveLength(2)
  expect(rows[0]).toHaveTextContent('Failed 429')
  expect(rows[0]).toHaveTextContent('<1s / —')
  expect(rows[1]).toHaveTextContent('Success')
  expect(rows[1]).toHaveTextContent('4.0s / 0.7s')
  expect(rows[1]).toHaveTextContent('1,204 / 356')
  expect(rows[1]).toHaveTextContent('req-succeeded')
  expect(
    within(dialog).queryByText(/logging is disabled/)
  ).not.toBeInTheDocument()

  // A failed request explains itself without leaving the page.
  await user.click(
    within(rows[0]).getByRole('button', { name: 'Show details' })
  )
  expect(
    within(dialog).getByText('rate_limit_exceeded: Rate limit reached')
  ).toBeVisible()
  expect(within(dialog).getByText('#5 → #3')).toBeVisible()
  expect(within(dialog).getByText('alice / prod-web')).toBeVisible()
  expect(within(dialog).getByText('Not charged')).toBeVisible()
  await user.click(
    within(rows[0]).getByRole('button', { name: 'Hide details' })
  )
  expect(within(dialog).queryByText('#5 → #3')).not.toBeInTheDocument()

  // Narrowing to failures reads nothing new.
  await user.click(within(dialog).getByRole('button', { name: /^Failed only/ }))
  expect(within(dialog).getAllByRole('row').slice(1)).toHaveLength(1)
  expect(within(dialog).queryByText('req-succeeded')).not.toBeInTheDocument()
  expect(logSearches(get)).toHaveLength(searches.length)

  // The usage log opens with this channel, period and result already set.
  await user.click(
    within(dialog).getByRole('link', { name: 'View in usage logs' })
  )
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/usage-logs/common')
  )
  const search = router.state.location.search as Record<string, unknown>
  expect(search.channel).toBe('3')
  expect(search.type).toEqual(['5'])
  const since = Number(searches[0].get('start_timestamp'))
  expect(search.startTime).toBe(since * 1000)
  expect(search.endTime).toBe((since + 3600 + 3600) * 1000)
})

test('one request opens in the usage log by its request ID', async () => {
  mockGateway({
    succeeded: [succeeded],
    failed: [],
    logged: { successes: true, failures: true },
  })
  const router = await renderChannelPage()
  const user = userEvent.setup()

  await user.click(screen.getByRole('button', { name: 'Details of prod' }))
  const dialog = await screen.findByRole('dialog', { name: 'Channel details' })
  await within(dialog).findByText('req-succeeded')
  await user.click(within(dialog).getByRole('button', { name: 'Show details' }))
  expect(within(dialog).getByText('First attempt')).toBeVisible()
  await user.click(
    within(dialog).getByRole('link', {
      name: 'Open this request in usage logs',
    })
  )

  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/usage-logs/common')
  )
  expect(router.state.location.search).toEqual({
    requestId: 'req-succeeded',
    startTime: (succeeded.created_at - 3600) * 1000,
    endTime: (succeeded.created_at + 3600) * 1000,
  })
})

test('an empty list says which requests the gateway does not log', async () => {
  mockGateway({
    succeeded: [],
    failed: [],
    logged: { successes: false, failures: false },
  })
  await renderChannelPage()

  await userEvent.click(screen.getByRole('button', { name: 'Details of prod' }))
  const dialog = await screen.findByRole('dialog', { name: 'Channel details' })

  expect(
    await within(dialog).findByText('No requests in this time range')
  ).toBeVisible()
  expect(
    await within(dialog).findByText(
      'Consumption logging is disabled, so successful requests are not listed here.'
    )
  ).toBeVisible()
  expect(
    within(dialog).getByText(
      'Request error logging is disabled, so ordinary failed requests are not listed here. Enable ERROR_LOG_ENABLED on gateway nodes to record them.'
    )
  ).toBeVisible()
  expect(within(dialog).queryByRole('table')).not.toBeInTheDocument()
})

test('a tag row, which stands for several channels, has no details button', async () => {
  mockGateway({
    succeeded: [],
    failed: [],
    logged: { successes: true, failures: true },
  })
  await renderChannelPage({
    ...channel,
    id: -1,
    children: [channel],
  } as Channel)

  expect(
    screen.queryByRole('button', { name: /^Details/ })
  ).not.toBeInTheDocument()
})
