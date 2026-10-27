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
  act,
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { StrictMode, type ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { ChannelOverview } from '../index'
import type { OverviewMetrics, OverviewSnapshot } from '../types'

let client: QueryClient
let snapshot: OverviewSnapshot

function Wrapper(props: { children: ReactNode }) {
  return (
    <QueryClientProvider client={client}>{props.children}</QueryClientProvider>
  )
}

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'root', role: ROLE.SUPER_ADMIN })
  const metrics: OverviewMetrics = {
    revenue: 12,
    cost: 5,
    complete_revenue: 10,
    complete_cost: 5,
    margin: 5,
    margin_rate: 50,
    requests: 4,
    attempts: 5,
    successes: 4,
    failures: 1,
    unknown_cost_attempts: 1,
    complete_requests: 3,
    complete_attempts: 4,
    unconfirmed_revenue_requests: 0,
    subscription_revenue: 0,
    cost_coverage: 80,
    health: 80,
    latency_ms: 120,
    cache_input_tokens: 10_000,
    cache_read_tokens: 990,
    cache_write_tokens: 400,
    cache_usage_attempts: 4,
    cache_hit_attempts: 2,
    cache_hit_rate: 9.9,
    cache_usage_coverage: 80,
  }
  snapshot = {
    dimension: 'channel',
    days: 7,
    currency: 'USD',
    scope: 'synchronous_relay',
    refreshed_at: 1791432000,
    collected_since: 1791345600,
    partial: false,
    summary: metrics,
    daily: [{ day: '2026-10-08', ...metrics }],
    ranking: [{ id: '42', name: 'Primary', ...metrics }],
    distribution: [],
    models: [],
    channels: [{ id: '42', name: 'Primary', ...metrics }],
    options: [{ id: '42', name: 'Primary' }],
    limits: { max_days: 90, max_rows: 100 },
    warnings: [],
  }
  vi.spyOn(api, 'get').mockImplementation(async (_url, config) => {
    const params = config?.params as
      | { dimension?: OverviewSnapshot['dimension']; days?: number }
      | undefined
    return {
      data: {
        success: true,
        data: {
          ...snapshot,
          dimension: params?.dimension,
          days: params?.days,
        },
      },
    }
  })
})

afterEach(async () => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
  await i18next.changeLanguage('en')
  i18next.removeResourceBundle('fr', 'translation')
})

describe('channel overview contracts', () => {
  test('cache summary uses the server token-weighted rate and model measurements', async () => {
    snapshot.models = [
      {
        ...snapshot.ranking[0],
        id: 'small-cached-model',
        name: 'small-cached-model',
        cache_input_tokens: 1_000,
        cache_read_tokens: 990,
        cache_write_tokens: 400,
        cache_hit_rate: 99,
      },
      {
        ...snapshot.ranking[0],
        id: 'large-uncached-model',
        name: 'large-uncached-model',
        cache_input_tokens: 9_000,
        cache_read_tokens: 0,
        cache_write_tokens: 0,
        cache_hit_rate: 0,
      },
    ]
    render(<ChannelOverview />, { wrapper: Wrapper })
    const section = await screen.findByRole('region', {
      name: 'Cache Statistics',
    })
    const summary = within(section).getByRole('region', {
      name: 'Cache Usage Samples',
    })
    expect(within(summary).getByText('9.9%')).toBeVisible()
    expect(within(summary).queryByText('49.5%')).not.toBeInTheDocument()
    expect(within(summary).getByText('990')).toBeVisible()
    expect(within(summary).getByText('400')).toBeVisible()
    const models = within(section).getByRole('table', {
      name: 'Cache Statistics by Model',
    })
    expect(within(models).getByText('99%')).toBeVisible()
    expect(within(models).getByText('0%')).toBeVisible()
    expect(within(models).getByText('9,000')).toBeVisible()
  })

  test('cache views switch between models and channels without another analytics request', async () => {
    const user = userEvent.setup()
    snapshot.models = [
      { ...snapshot.ranking[0], id: 'model-a', name: 'model-a' },
    ]
    render(<ChannelOverview />, { wrapper: Wrapper })
    await screen.findByRole('table', { name: 'Cache Statistics by Model' })
    await user.click(screen.getByRole('tab', { name: 'By Channel' }))
    expect(screen.getByRole('tab', { name: 'By Channel' })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    const channels = screen.getByRole('table', {
      name: 'Cache Statistics by Channel',
    })
    expect(within(channels).getByText('Primary')).toBeVisible()
    expect(within(channels).getByText('9.9%')).toBeVisible()
    expect(within(channels).getByText('4 / 5 attempts')).toBeVisible()
    await user.keyboard('{ArrowLeft}')
    await user.keyboard('{Enter}')
    expect(screen.getByRole('tab', { name: 'By Model' })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    expect(
      screen.getByRole('table', { name: 'Cache Statistics by Model' })
    ).toBeVisible()
    expect(api.get).toHaveBeenCalledTimes(1)
  })

  test('missing cache measurements stay unavailable while measured cache misses display zero', async () => {
    snapshot.summary = {
      ...snapshot.summary,
      cache_input_tokens: 0,
      cache_read_tokens: 0,
      cache_write_tokens: 0,
      cache_usage_attempts: 0,
      cache_hit_attempts: 0,
      cache_hit_rate: null,
      cache_usage_coverage: 0,
    }
    snapshot.models = [
      { ...snapshot.summary, id: 'unknown-model', name: 'unknown-model' },
      {
        ...snapshot.summary,
        id: 'measured-miss',
        name: 'measured-miss',
        attempts: 1,
        cache_input_tokens: 100,
        cache_usage_attempts: 1,
        cache_hit_rate: 0,
        cache_usage_coverage: 100,
      },
    ]
    render(<ChannelOverview />, { wrapper: Wrapper })
    const models = await screen.findByRole('table', {
      name: 'Cache Statistics by Model',
    })
    const unknownCells = within(
      within(models).getByRole('row', { name: /unknown-model/ })
    ).getAllByRole('cell')
    expect(unknownCells[1]).toHaveTextContent('—')
    expect(unknownCells[2]).toHaveTextContent('—')
    expect(unknownCells[3]).toHaveTextContent('—')
    expect(unknownCells[4]).toHaveTextContent('—')
    const missCells = within(
      within(models).getByRole('row', { name: /measured-miss/ })
    ).getAllByRole('cell')
    expect(missCells[1]).toHaveTextContent('0%')
    expect(missCells[2]).toHaveTextContent('0')
    expect(missCells[3]).toHaveTextContent('0')
    expect(missCells[4]).toHaveTextContent('100')
    expect(screen.getByText('No cache usage samples yet.')).toBeVisible()
  })

  test('older analytics responses without cache fields keep financial data and unknown cache values', async () => {
    const user = userEvent.setup()
    for (const key of Object.keys(snapshot.summary)) {
      if (key.startsWith('cache_')) {
        Reflect.deleteProperty(snapshot.summary, key)
      }
    }
    Reflect.deleteProperty(snapshot, 'channels')
    render(<ChannelOverview />, { wrapper: Wrapper })
    const summary = await screen.findByRole('region', {
      name: 'Cache Usage Samples',
    })
    expect(within(summary).getAllByText('—')).toHaveLength(4)
    const ranking = screen.getByRole('table', { name: 'Channel Ranking' })
    expect(within(ranking).getAllByRole('cell')[3]).toHaveTextContent('$12')
    await user.click(screen.getByRole('tab', { name: 'By Channel' }))
    expect(
      screen.getByRole('table', { name: 'Cache Statistics by Channel' })
    ).toHaveTextContent('No cache usage samples yet.')
    expect(screen.queryByText('NaN')).not.toBeInTheDocument()
  })

  test('cache statistics refresh with the current period and selected analysis object', async () => {
    const user = userEvent.setup()
    vi.mocked(api.get).mockImplementation(async (_url, config) => {
      const params = config?.params as
        | { days?: number; object_id?: string }
        | undefined
      const selected = params?.days === 30 && params?.object_id === '42'
      const cache = {
        ...snapshot.summary,
        cache_hit_rate: selected ? 37.5 : 9.9,
      }
      return {
        data: {
          success: true,
          data: {
            ...snapshot,
            summary: cache,
            models: [{ ...cache, id: 'model-a', name: 'model-a' }],
            channels: [{ ...cache, id: '42', name: 'Primary' }],
          },
        },
      }
    })
    render(<ChannelOverview />, { wrapper: Wrapper })
    await screen.findByRole('table', { name: 'Cache Statistics by Model' })
    await user.click(screen.getByRole('button', { name: '30 Days' }))
    await user.click(await screen.findByRole('button', { name: 'Primary' }))
    await waitFor(() =>
      expect(
        within(
          screen.getByRole('table', { name: 'Cache Statistics by Model' })
        ).getByText('37.5%')
      ).toBeVisible()
    )
    await user.click(screen.getByRole('tab', { name: 'By Channel' }))
    expect(
      within(
        screen.getByRole('table', { name: 'Cache Statistics by Channel' })
      ).getByText('37.5%')
    ).toBeVisible()
    expect(api.get).toHaveBeenLastCalledWith(
      '/api/channel-overview',
      expect.objectContaining({
        params: { dimension: 'channel', days: 30, object_id: '42' },
      })
    )
  })

  test('typing an object ID sends no analytics queries until Enter commits it', async () => {
    const user = userEvent.setup()
    render(<ChannelOverview />, { wrapper: Wrapper })
    await screen.findByRole('table', { name: 'Channel Ranking' })
    const input = screen.getByRole('combobox', { name: 'Analysis Object' })
    await user.click(input)
    await user.clear(input)
    await user.type(input, '101')
    expect(api.get).toHaveBeenCalledTimes(1)
    await user.keyboard('{Enter}')
    await waitFor(() => expect(api.get).toHaveBeenCalledTimes(2))
    expect(api.get).toHaveBeenLastCalledWith(
      '/api/channel-overview',
      expect.objectContaining({
        params: { dimension: 'channel', days: 7, object_id: '101' },
      })
    )
  })

  test('metric explanations stay collapsed until their info button is hovered or focused', async () => {
    const user = userEvent.setup()
    render(<ChannelOverview />, { wrapper: Wrapper })
    await screen.findByRole('table', { name: 'Channel Ranking' })
    expect(
      screen.queryByText('Settled usage value, not cash receipts.')
    ).not.toBeInTheDocument()
    const help = screen.getByRole('button', {
      name: 'Details for Billing Revenue',
    })
    await user.hover(help)
    expect(await screen.findByRole('tooltip')).toHaveTextContent(
      'Settled usage value, not cash receipts.'
    )
    expect(help).toHaveAccessibleDescription(
      'Settled usage value, not cash receipts.'
    )
    await user.unhover(help)
    await waitFor(() =>
      expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
    )
    await act(async () => {
      help.focus()
    })
    expect(await screen.findByRole('tooltip')).toHaveTextContent(
      'Settled usage value, not cash receipts.'
    )
    await user.keyboard('{Escape}')
    await waitFor(() =>
      expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
    )
  })

  test('coverage explanations are collapsed while unknown cost and funding counts stay visible', async () => {
    const user = userEvent.setup()
    snapshot.summary.unconfirmed_revenue_requests = 2
    render(<ChannelOverview />, { wrapper: Wrapper })
    await screen.findByRole('table', { name: 'Channel Ranking' })
    expect(screen.getByText('Unknown-cost attempts: 1')).toBeVisible()
    expect(screen.getByText('Funding-unconfirmed requests: 2')).toBeVisible()
    expect(
      screen.queryByText(
        'Data before collection started is unavailable. Historical calls are not recalculated.'
      )
    ).not.toBeInTheDocument()
    await user.click(
      screen.getByRole('button', { name: 'Details for Data Coverage' })
    )
    expect(await screen.findByRole('tooltip')).toHaveTextContent(
      'Data before collection started is unavailable.'
    )
  })

  test('language switches reformat the loaded monetary values without another API request', async () => {
    snapshot.summary.revenue = 12.5
    snapshot.ranking[0].revenue = 12.5
    snapshot.models = [
      { ...snapshot.ranking[0], id: 'model-a', name: 'model-a' },
    ]
    i18next.addResourceBundle('fr', 'translation', {
      'Channel Overview': 'Vue des canaux',
    })
    render(<ChannelOverview />, { wrapper: Wrapper })
    const table = await screen.findByRole('table', { name: 'Channel Ranking' })
    expect(within(table).getAllByRole('cell')[3]).toHaveTextContent('$12.5')
    const cache = screen.getByRole('table', {
      name: 'Cache Statistics by Model',
    })
    expect(within(cache).getByText('10,000')).toBeVisible()
    await act(async () => {
      await i18next.changeLanguage('fr')
    })
    expect(
      screen.getByRole('heading', { name: 'Vue des canaux' })
    ).toBeVisible()
    expect(within(table).getAllByRole('cell')[3]).toHaveTextContent('12,5')
    expect(within(cache).getByText(/10\s000/)).toBeVisible()
    expect(api.get).toHaveBeenCalledTimes(1)
  })

  test('loads one batch per active tab and drills into an object with the current range', async () => {
    const user = userEvent.setup()
    snapshot.options = []
    render(<ChannelOverview />, { wrapper: Wrapper })
    await screen.findByRole('table', { name: 'Channel Ranking' })
    expect(api.get).toHaveBeenCalledTimes(1)
    await user.click(screen.getByRole('button', { name: '30 Days' }))
    await screen.findByRole('table', { name: 'Channel Ranking' })
    await user.click(screen.getByRole('button', { name: 'Primary' }))
    await waitFor(() =>
      expect(api.get).toHaveBeenLastCalledWith(
        '/api/channel-overview',
        expect.objectContaining({
          params: { dimension: 'channel', days: 30, object_id: '42' },
          signal: expect.any(AbortSignal),
          disableDuplicate: true,
        })
      )
    )
    await waitFor(() =>
      expect(
        screen.getByRole('combobox', { name: 'Analysis Object' })
      ).toHaveValue('Primary')
    )
    await user.click(
      screen.getByRole('tab', { name: 'Upstream Key Analytics' })
    )
    await screen.findByRole('table', { name: 'Upstream Key Ranking' })
    expect(api.get).toHaveBeenLastCalledWith(
      '/api/channel-overview',
      expect.objectContaining({
        params: { dimension: 'key', days: 30, object_id: undefined },
      })
    )
    expect(api.get).toHaveBeenCalledTimes(4)
  })

  test('unknown costs and missing health remain unavailable while explicit percentages keep their units', async () => {
    const user = userEvent.setup()
    snapshot.summary = {
      ...snapshot.summary,
      cost_coverage: null,
      complete_requests: 0,
      complete_attempts: 0,
      margin_rate: null,
      health: null,
      unconfirmed_revenue_requests: 2,
      subscription_revenue: 9,
    }
    snapshot.ranking[0] = { ...snapshot.ranking[0], ...snapshot.summary }
    snapshot.daily[0] = { ...snapshot.daily[0], ...snapshot.summary }
    render(<ChannelOverview />, { wrapper: Wrapper })
    const table = await screen.findByRole('table', { name: 'Channel Ranking' })
    const cells = within(table).getAllByRole('cell')
    expect(cells[4]).toHaveTextContent('—')
    expect(cells[5]).toHaveTextContent('—')
    expect(cells[6]).toHaveTextContent('—')
    expect(cells[7]).toHaveTextContent('—')
    expect(await screen.findByText('No samples yet')).toBeVisible()
    expect(screen.getByText('Unknown-cost attempts: 1')).toBeVisible()
    expect(screen.getByText('Funding-unconfirmed requests: 2')).toBeVisible()
    expect(screen.getByText('Subscription usage value: $9')).toBeVisible()
    await user.hover(
      screen.getByRole('button', { name: 'Details for Estimated Gross Profit' })
    )
    expect(await screen.findByRole('tooltip')).toHaveTextContent(
      'No requests with complete cost records yet.'
    )
    await user.keyboard('{Escape}')
    await user.click(
      screen.getByRole('button', { name: 'Details for Data Coverage' })
    )
    const tooltip = await screen.findByRole('tooltip')
    expect(tooltip).toHaveTextContent('Unknown cost is never treated as zero.')
    expect(tooltip).toHaveTextContent('They are excluded from profit.')
    expect(tooltip).toHaveTextContent(
      'This is a quota equivalent, not cash revenue.'
    )
    await user.keyboard('{Escape}')
    await act(async () => {
      snapshot.summary = {
        ...snapshot.summary,
        cost_coverage: 80,
        complete_requests: 3,
        complete_attempts: 4,
        margin_rate: 50,
        health: 80,
      }
      snapshot.ranking[0] = { ...snapshot.ranking[0], ...snapshot.summary }
      await client.refetchQueries({
        queryKey: ['channel-overview'],
        type: 'active',
      })
    })
    await waitFor(() =>
      expect(
        within(
          screen.getByRole('table', { name: 'Channel Ranking' })
        ).getAllByText('80%')
      ).toHaveLength(2)
    )
  })

  test.each([
    [2, '-$2'],
    [0.000002, '-$0.000002'],
  ])(
    'retry-only objects retain complete upstream loss %s even without a final billed request',
    async (cost, expectedMargin) => {
      snapshot.summary = {
        ...snapshot.summary,
        requests: 0,
        attempts: 1,
        successes: 0,
        failures: 1,
        health: 0,
        revenue: 0,
        cost: Number(cost),
        complete_revenue: 0,
        complete_cost: Number(cost),
        complete_requests: 0,
        complete_attempts: 1,
        margin: -Number(cost),
        margin_rate: null,
        cost_coverage: 100,
        unknown_cost_attempts: 0,
      }
      snapshot.ranking[0] = { ...snapshot.ranking[0], ...snapshot.summary }
      snapshot.daily[0] = { ...snapshot.daily[0], ...snapshot.summary }
      render(<ChannelOverview />, { wrapper: Wrapper })
      const table = await screen.findByRole('table', {
        name: 'Channel Ranking',
      })
      expect(within(table).getAllByRole('cell')[5]).toHaveTextContent(
        String(expectedMargin)
      )
      expect(within(table).getAllByRole('cell')[5]).toHaveClass(
        'text-destructive'
      )
      expect(
        screen.queryByText('No requests with complete cost records yet.')
      ).not.toBeInTheDocument()
    }
  )

  test('aborts stale tab requests and never displays their late response', async () => {
    const user = userEvent.setup()
    let resolveFirst: ((value: unknown) => void) | undefined
    vi.mocked(api.get).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveFirst = resolve
        })
    )
    render(<ChannelOverview />, { wrapper: Wrapper })
    await waitFor(() => expect(api.get).toHaveBeenCalledTimes(1))
    const signal = vi.mocked(api.get).mock.calls[0][1]?.signal
    await user.click(screen.getByRole('tab', { name: 'Group Analytics' }))
    await screen.findByRole('table', { name: 'Group Ranking' })
    expect(signal?.aborted).toBe(true)
    await act(async () =>
      resolveFirst?.({
        data: {
          success: true,
          data: {
            ...snapshot,
            ranking: [{ ...snapshot.ranking[0], name: 'Stale channel' }],
          },
        },
      })
    )
    expect(screen.queryByText('Stale channel')).not.toBeInTheDocument()
  })

  test('unnamed procurement objects have readable labels and drill through stable IDs', async () => {
    const user = userEvent.setup()
    snapshot.ranking[0] = {
      ...snapshot.ranking[0],
      id: '__unassigned__',
      name: '',
    }
    snapshot.options = [{ id: '__unassigned__', name: '' }]
    render(<ChannelOverview />, { wrapper: Wrapper })
    await user.click(
      screen.getByRole('tab', { name: 'Upstream Supplier Analytics' })
    )
    await user.click(await screen.findByRole('button', { name: 'Unassigned' }))
    await waitFor(() =>
      expect(api.get).toHaveBeenLastCalledWith(
        '/api/channel-overview',
        expect.objectContaining({
          params: {
            dimension: 'supplier',
            days: 7,
            object_id: '__unassigned__',
          },
        })
      )
    )
    const keyId = '68e1abcd-2081-453f-91b2-64036bfa835d'
    snapshot.ranking[0] = { ...snapshot.ranking[0], id: keyId, name: '' }
    snapshot.options = [{ id: keyId, name: '' }]
    await user.click(
      screen.getByRole('tab', { name: 'Upstream Key Analytics' })
    )
    await user.click(
      await screen.findByRole('button', { name: 'Upstream Key 68e1abcd' })
    )
    await waitFor(() =>
      expect(api.get).toHaveBeenLastCalledWith(
        '/api/channel-overview',
        expect.objectContaining({
          params: { dimension: 'key', days: 7, object_id: keyId },
        })
      )
    )
  })

  test('server failure offers retry and an empty response shows no fabricated trend', async () => {
    const user = userEvent.setup()
    vi.mocked(api.get).mockResolvedValueOnce({
      data: { success: false, message: 'Unavailable' },
    })
    render(<ChannelOverview />, { wrapper: Wrapper })
    expect(
      await screen.findByText('Unable to load channel analytics.')
    ).toBeVisible()
    snapshot.summary = {
      ...snapshot.summary,
      requests: 0,
      attempts: 0,
      complete_requests: 0,
      complete_attempts: 0,
      health: null,
      cost_coverage: null,
      margin_rate: null,
    }
    snapshot.daily = []
    snapshot.ranking = []
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('No data in this time range')).toBeVisible()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    expect(
      screen.queryByText('Daily Spending and Profit')
    ).not.toBeInTheDocument()
  })

  test.each([
    [ROLE.ADMIN, false],
    [ROLE.USER, true],
  ])(
    'admin role and read permission are required before any analytics request (%s, %s)',
    async (role, read) => {
      useAuthStore.getState().auth.setUser({
        id: 2,
        username: 'restricted',
        role,
        permissions: { admin_permissions: { channel: { read } } },
      })
      render(<ChannelOverview />, { wrapper: Wrapper })
      expect(screen.getByText('Access Forbidden')).toBeVisible()
      expect(api.get).not.toHaveBeenCalled()
    }
  )

  test('StrictMode can restart a cancelled request without duplicate-request interference', async () => {
    render(
      <StrictMode>
        <ChannelOverview />
      </StrictMode>,
      { wrapper: Wrapper }
    )
    expect(
      await screen.findByRole('table', { name: 'Channel Ranking' })
    ).toBeVisible()
    expect(api.get).toHaveBeenCalledWith(
      '/api/channel-overview',
      expect.objectContaining({ disableDuplicate: true })
    )
  })
})
