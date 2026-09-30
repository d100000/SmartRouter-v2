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
import { act, render, screen, waitFor } from '@testing-library/react'
import i18next from 'i18next'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { TooltipProvider } from '@/components/ui/tooltip'
import { createAppQueryClient } from '@/lib/query-client'

import { getChannelRecentStats } from '../../api'
import type { Channel, ChannelRecentStats } from '../../types'
import {
  ChannelRecentStatsCell,
  ChannelRecentStatsProvider,
} from '../channel-recent-stats'

vi.mock('../../api', () => ({ getChannelRecentStats: vi.fn() }))

const onInternalServerError = vi.fn()

const snapshot: ChannelRecentStats = {
  ready: true,
  refreshed_at: Math.floor(Date.now() / 1000),
  window_seconds: 600,
  scope: 'instance',
  items: [
    { channel_id: 1, requests: 10, successes: 9, success_rate: 0.9 },
    { channel_id: 2, requests: 90, successes: 0, success_rate: 0 },
  ],
}

function mount(channels: Channel[], cached?: ChannelRecentStats) {
  const client = createAppQueryClient(onInternalServerError)
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

afterEach(async () => {
  expect(onInternalServerError).not.toHaveBeenCalled()
  vi.resetAllMocks()
  await i18next.changeLanguage('en')
})

describe('channel recent statistics', () => {
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
      screen.getAllByLabelText('Channel statistics are loading')
    ).toHaveLength(3)
    expect(getChannelRecentStats).toHaveBeenCalledTimes(1)
    await act(async () => resolve(snapshot))
    expect(await screen.findByText('90% / 10')).toBeVisible()
    expect(screen.getByText('0% / 90')).toBeVisible()
    expect(screen.getByText('— / 0')).toBeVisible()
  })

  test('weights tag success by request counts instead of averaging percentages', () => {
    const tag = {
      id: -1,
      children: [{ id: 1 }, { id: 2 }],
    } as unknown as Channel
    mount([tag], snapshot)
    expect(screen.getByText('9% / 100')).toBeVisible()
  })

  test('does not present the initial unpublished cache as zero requests', () => {
    mount([{ id: 1 }] as Channel[], {
      ...snapshot,
      ready: false,
      items: [],
      refreshed_at: 0,
    })
    expect(
      screen.getByLabelText('Channel statistics are loading')
    ).toBeVisible()
    expect(screen.queryByText('— / 0')).not.toBeInTheDocument()
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
    expect(screen.getByText('90% / 10')).toBeVisible()
    await waitFor(() =>
      expect(
        screen
          .getByLabelText('Success / Requests (10m): 90% / 10')
          .querySelector('svg')
      ).not.toBeNull()
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
      await screen.findByLabelText('Channel statistics are unavailable')
    ).toHaveTextContent('— / —')
  })

  test('marks an old cached snapshot as delayed immediately', () => {
    mount([{ id: 1 }] as Channel[], {
      ...snapshot,
      refreshed_at: Math.floor(Date.now() / 1000) - 120,
    })
    expect(
      screen
        .getByLabelText('Success / Requests (10m): 90% / 10')
        .querySelector('svg')
    ).not.toBeNull()
  })

  test.each(['zhCN', 'zhTW', 'en', 'fr', 'ja', 'ru', 'vi', 'invalid_locale'])(
    'formats data safely in %s and follows language changes',
    async (language) => {
      mount([{ id: 1 }] as Channel[], snapshot)
      await act(async () => {
        await i18next.changeLanguage(language)
      })
      expect(screen.getByText('90% / 10')).toBeVisible()
      await act(async () => {
        await i18next.changeLanguage('en')
      })
      expect(
        screen.getByLabelText('Success / Requests (10m): 90% / 10')
      ).toBeVisible()
    }
  )
})
