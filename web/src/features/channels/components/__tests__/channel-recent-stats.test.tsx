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
import { QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { TooltipProvider } from '@/components/ui/tooltip'
import { createAppQueryClient } from '@/lib/query-client'

import { getChannelRecentStats } from '../../api'
import type {
  Channel,
  ChannelRecentStat,
  ChannelRecentStats,
} from '../../types'
import {
  ChannelRecentStatsCell,
  ChannelRecentStatsProvider,
} from '../channel-recent-stats'
import { ChannelRowActionsLayoutContext } from '../channel-row-actions-context'

vi.mock('../../api', () => ({ getChannelRecentStats: vi.fn() }))

const onInternalServerError = vi.fn()
const clients: ReturnType<typeof createAppQueryClient>[] = []
const refreshedAt = Math.floor(Date.now() / 1000)

function channelStat(
  channelId: number,
  requests: number,
  successes: number
): ChannelRecentStat {
  return {
    channel_id: channelId,
    requests,
    successes,
    success_rate: requests > 0 ? successes / requests : null,
    requests_1h: requests,
    successes_1h: successes,
    success_rate_1h: requests > 0 ? successes / requests : null,
    history: Array.from({ length: 6 }, (_, index) => ({
      start_time: refreshedAt - 3600 + index * 600,
      end_time: refreshedAt - 3000 + index * 600,
      requests: index === 5 ? requests : 0,
      successes: index === 5 ? successes : 0,
      success_rate: index === 5 && requests > 0 ? successes / requests : null,
    })),
    ttft_samples_5m: successes > 0 ? 1 : 0,
    ttft_sum_ms_5m: successes > 0 ? 250 : 0,
    avg_ttft_ms_5m: successes > 0 ? 250 : null,
    response_samples_5m: 0,
    response_sum_ms_5m: 0,
    avg_response_ms_5m: null,
  }
}

const snapshot: ChannelRecentStats = {
  ready: true,
  refreshed_at: refreshedAt,
  window_seconds: 600,
  scope: 'instance',
  collected_since: refreshedAt - 3600,
  history_window_seconds: 3600,
  bucket_seconds: 600,
  latency_window_seconds: 300,
  items: [channelStat(1, 10, 9), channelStat(2, 90, 0)],
}

function mount(channels: Channel[], cached?: ChannelRecentStats) {
  const client = createAppQueryClient(onInternalServerError)
  clients.push(client)
  client.setDefaultOptions({ queries: { gcTime: 0 } })
  if (cached) client.setQueryData(['channel-recent-stats'], cached)
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <ChannelRecentStatsProvider>
          <span>Channel list is ready</span>
          {channels.map((channel) => (
            <ChannelRecentStatsCell key={channel.id} channel={channel} />
          ))}
        </ChannelRecentStatsProvider>
      </TooltipProvider>
    </QueryClientProvider>
  )
  return client
}

beforeEach(() => {
  vi.mocked(getChannelRecentStats).mockResolvedValue(snapshot)
})

afterEach(async () => {
  expect(onInternalServerError).not.toHaveBeenCalled()
  vi.resetAllMocks()
  clients.splice(0).forEach((client) => client.clear())
  await i18next.changeLanguage('en')
})

describe('channel recent statistics', () => {
  test('keeps health above a muted first-token line within the existing table row height', async () => {
    const refreshedAt = Math.floor(Date.now() / 1000)
    const health = {
      ...snapshot,
      refreshed_at: refreshedAt,
      collected_since: refreshedAt - 3600,
      history_window_seconds: 3600,
      bucket_seconds: 600,
      latency_window_seconds: 300,
      items: [
        {
          ...snapshot.items[0],
          requests_1h: 10,
          successes_1h: 9,
          success_rate_1h: 0.9,
          history: Array.from({ length: 6 }, (_, index) => ({
            start_time: refreshedAt - 3600 + index * 600,
            end_time: refreshedAt - 3000 + index * 600,
            requests: index === 5 ? 10 : 0,
            successes: index === 5 ? 9 : 0,
            success_rate: index === 5 ? 0.9 : null,
          })),
          ttft_samples_5m: 1,
          ttft_sum_ms_5m: 250,
          avg_ttft_ms_5m: 250,
          response_samples_5m: 0,
          response_sum_ms_5m: 0,
          avg_response_ms_5m: null,
        },
      ],
    }
    mount([{ id: 1 }] as Channel[], health)
    const summary = screen.getByRole('group', { name: 'Channel health (1h)' })
    expect(summary).toHaveClass(
      'h-10',
      'w-50',
      'shrink-0',
      'whitespace-nowrap',
      'grid-cols-[46px_minmax(0,1fr)]',
      'grid-rows-[20px_16px]',
      'gap-y-1'
    )
    const latency = within(summary).getByLabelText(
      'Average streaming first-token time (5m): 250ms'
    )
    expect(latency).toHaveClass(
      'col-span-2',
      'row-start-2',
      'text-muted-foreground'
    )
    expect(latency).toHaveAttribute('data-table-text', 'secondary')
    expect(within(summary).getByText('90%')).toBeVisible()
    expect(within(summary).getByText('250ms')).toBeVisible()
    const intervals = within(summary).getAllByRole('img')
    expect(intervals).toHaveLength(6)
    await userEvent.hover(intervals[0])
    expect(
      await screen.findByText('No requests in this interval')
    ).toBeVisible()
    await userEvent.unhover(intervals[0])
    await userEvent.hover(intervals[5])
    expect(await screen.findByText('Successful attempts: 9 / 10')).toBeVisible()
  })

  test('loads once without blocking the list, then distinguishes failures from no samples', async () => {
    let resolve!: (value: ChannelRecentStats) => void
    vi.mocked(getChannelRecentStats).mockImplementation(
      () =>
        new Promise((done) => {
          resolve = done
        })
    )
    mount([{ id: 1 }, { id: 2 }, { id: 3 }] as Channel[])
    expect(screen.getByText('Channel list is ready')).toBeVisible()
    expect(
      screen.getAllByRole('group', { name: 'Channel statistics are loading' })
    ).toHaveLength(3)
    expect(getChannelRecentStats).toHaveBeenCalledTimes(1)
    await act(async () => resolve(snapshot))
    expect(await screen.findByText('90%')).toBeVisible()
    expect(screen.getByText('0%')).toBeVisible()
    expect(screen.getByLabelText('Success rate (1h): —')).toBeVisible()
  })

  test('weights tag success by request counts instead of averaging percentages', () => {
    const tag = {
      id: -1,
      children: [{ id: 1 }, { id: 2 }],
    } as unknown as Channel
    mount([tag], snapshot)
    expect(screen.getByText('9%')).toBeVisible()
  })

  test('weights tag streaming latency by sample counts and keeps non-streaming latency separate', async () => {
    const tag = {
      id: -1,
      children: [{ id: 1 }, { id: 2 }],
    } as unknown as Channel
    mount([tag], {
      ...snapshot,
      items: [
        {
          ...channelStat(1, 2, 2),
          ttft_samples_5m: 1,
          ttft_sum_ms_5m: 100,
          avg_ttft_ms_5m: 100,
          response_samples_5m: 1,
          response_sum_ms_5m: 1000,
          avg_response_ms_5m: 1000,
        },
        {
          ...channelStat(2, 12, 12),
          ttft_samples_5m: 9,
          ttft_sum_ms_5m: 2700,
          avg_ttft_ms_5m: 300,
          response_samples_5m: 3,
          response_sum_ms_5m: 15000,
          avg_response_ms_5m: 5000,
        },
      ],
    })
    const latency = screen.getByLabelText(
      'Average streaming first-token time (5m): 280ms'
    )
    await userEvent.hover(latency)
    expect(
      await screen.findByText(
        'Based on 10 completed successful streaming attempts.'
      )
    ).toBeVisible()
    expect(
      screen.getByText(
        'Average non-streaming first-response time (5m): 4,000ms'
      )
    ).toBeVisible()
    expect(
      screen.getByText(
        'Based on 4 completed successful non-streaming attempts.'
      )
    ).toBeVisible()
  })

  test('shows no streaming latency when only non-streaming measurements are available', async () => {
    mount([{ id: 1 }] as Channel[], {
      ...snapshot,
      items: [
        {
          ...channelStat(1, 1, 1),
          ttft_samples_5m: 0,
          ttft_sum_ms_5m: 0,
          avg_ttft_ms_5m: null,
          response_samples_5m: 1,
          response_sum_ms_5m: 500,
          avg_response_ms_5m: 500,
        },
      ],
    })
    await userEvent.hover(
      screen.getByLabelText('Average streaming first-token time (5m): —')
    )
    expect(
      await screen.findByText(
        'No successful streaming measurements in the last 5 minutes.'
      )
    ).toBeVisible()
    expect(
      screen.getByText('Average non-streaming first-response time (5m): 500ms')
    ).toBeVisible()
  })

  test('shows the first completed sample even when collection starts in the snapshot second', async () => {
    mount([{ id: 1 }] as Channel[], {
      ...snapshot,
      collected_since: refreshedAt,
      items: [channelStat(1, 1, 1)],
    })
    const intervals = screen.getAllByRole('img')
    expect(intervals[5]).toHaveClass('bg-success')
    await userEvent.hover(intervals[5])
    expect(await screen.findByText('Successful attempts: 1 / 1')).toBeVisible()
    expect(screen.getByText('Partially collected interval')).toBeVisible()
    expect(screen.queryByText('Not collected yet')).not.toBeInTheDocument()
  })

  test('shows collection gaps distinctly from an observed interval with no requests', async () => {
    mount([{ id: 1 }] as Channel[], {
      ...snapshot,
      collected_since: refreshedAt - 900,
      items: [channelStat(1, 0, 0)],
    })
    const intervals = screen.getAllByRole('img')
    await userEvent.hover(intervals[0])
    expect(await screen.findByText('Not collected yet')).toBeVisible()
    expect(
      screen.queryByText('No requests in this interval')
    ).not.toBeInTheDocument()
    await userEvent.unhover(intervals[0])
    await userEvent.hover(intervals[5])
    expect(
      await screen.findByText('No requests in this interval')
    ).toBeVisible()
    expect(screen.queryByText('Not collected yet')).not.toBeInTheDocument()
  })

  test('requests the deduplicated current page and tag children in one batch', async () => {
    const client = createAppQueryClient(onInternalServerError)
    clients.push(client)
    const row = { id: 1 } as Channel
    const page = [row, { id: -1, children: [{ id: 2 }, row] }] as Channel[]
    render(
      <QueryClientProvider client={client}>
        <TooltipProvider>
          <ChannelRecentStatsProvider channels={page}>
            <ChannelRecentStatsCell channel={row} />
          </ChannelRecentStatsProvider>
        </TooltipProvider>
      </QueryClientProvider>
    )
    expect(
      await screen.findAllByRole('group', { name: 'Channel health (1h)' })
    ).toHaveLength(1)
    expect(getChannelRecentStats).toHaveBeenCalledExactlyOnceWith(
      expect.any(AbortSignal),
      [1, 2]
    )
  })

  test('renders card statistics once in the footer and omits the table column', () => {
    const client = createAppQueryClient(onInternalServerError)
    clients.push(client)
    client.setQueryData(['channel-recent-stats'], snapshot)
    const row = { id: 1 } as Channel
    render(
      <QueryClientProvider client={client}>
        <TooltipProvider>
          <ChannelRecentStatsProvider>
            <ChannelRowActionsLayoutContext.Provider value='card'>
              <ChannelRecentStatsCell channel={row} placement='column' />
              <ChannelRecentStatsCell channel={row} placement='footer' />
            </ChannelRowActionsLayoutContext.Provider>
          </ChannelRecentStatsProvider>
        </TooltipProvider>
      </QueryClientProvider>
    )
    expect(
      screen.getAllByRole('group', { name: 'Channel health (1h)' })
    ).toHaveLength(1)
    expect(
      screen.getByRole('group', { name: 'Channel health (1h)' })
    ).toHaveClass('h-5')
    expect(screen.getByText('90%')).toBeVisible()
  })

  test('does not present the initial unpublished cache as zero requests or epoch dates', async () => {
    mount([{ id: 1 }] as Channel[], {
      ...snapshot,
      ready: false,
      items: [],
      refreshed_at: 0,
    })
    expect(
      screen.getByRole('group', { name: 'Channel statistics are loading' })
    ).toBeVisible()
    expect(screen.queryByText('0%')).not.toBeInTheDocument()
    await userEvent.hover(screen.getAllByRole('img')[0])
    expect(
      (await screen.findAllByText('Channel statistics are loading')).length
    ).toBeGreaterThan(0)
    expect(screen.queryByText(/1970/)).not.toBeInTheDocument()
  })

  test('keeps cached values when refresh fails and marks them as delayed', async () => {
    vi.mocked(getChannelRecentStats).mockRejectedValue(
      Object.assign(new Error('temporarily unavailable'), {
        response: { status: 500 },
      })
    )
    const client = mount([{ id: 1 }] as Channel[], snapshot)
    await act(async () => {
      await client.invalidateQueries({ queryKey: ['channel-recent-stats'] })
    })
    expect(screen.getByText('90%')).toBeVisible()
    await waitFor(() =>
      expect(
        screen.getByRole('group', { name: 'Channel health (1h)' })
      ).toHaveAccessibleDescription(
        'Statistics are delayed; showing the last snapshot.'
      )
    )
  })

  test('shows unavailable rather than zero on initial fetch failure', async () => {
    vi.mocked(getChannelRecentStats).mockRejectedValue(
      Object.assign(new Error('temporarily unavailable'), {
        response: { status: 500 },
      })
    )
    mount([{ id: 1 }] as Channel[])
    expect(
      await screen.findByRole('group', {
        name: 'Channel statistics are unavailable',
      })
    ).toHaveTextContent('—')
  })

  test('marks an old cached snapshot as delayed immediately', () => {
    mount([{ id: 1 }] as Channel[], {
      ...snapshot,
      refreshed_at: Math.floor(Date.now() / 1000) - 120,
    })
    expect(
      screen.getByRole('group', { name: 'Channel health (1h)' })
    ).toHaveAccessibleDescription(
      'Statistics are delayed; showing the last snapshot.'
    )
  })

  test.each(['zhCN', 'zhTW', 'en', 'fr', 'ja', 'ru', 'vi', 'invalid_locale'])(
    'formats data safely in %s and follows language changes',
    async (language) => {
      mount([{ id: 1 }] as Channel[], snapshot)
      await act(async () => {
        await i18next.changeLanguage(language)
      })
      expect(screen.getByText('90%')).toBeVisible()
      await act(async () => {
        await i18next.changeLanguage('en')
      })
      expect(screen.getByLabelText('Success rate (1h): 90%')).toBeVisible()
    }
  )
})
