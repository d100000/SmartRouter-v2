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
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'

import {
  buildResourceTimeline,
  formatResourceBytes,
  type SystemResourceHistory,
  type SystemResourcePoint,
} from '../../../lib/system-resources'
import { SystemResourceMonitor } from '../system-resource-monitor'

let client: QueryClient
const now = Math.floor(Date.now() / 1000)
const point: SystemResourcePoint = {
  timestamp: now,
  cpu_percent: 37.5,
  memory_percent: 50,
  memory_used: 8 * 1024 ** 3,
  memory_total: 16 * 1024 ** 3,
  disk_percent: 25,
  disk_used: 25 * 1024 ** 3,
  disk_total: 100 * 1024 ** 3,
  network_receive: 1024 ** 2,
  network_send: 512 * 1024,
}
const history: SystemResourceHistory = {
  node: 'gateway-a',
  from: now - 43200,
  to: now,
  interval_seconds: 60,
  points: [{ ...point, timestamp: now - 60 }],
}

beforeEach(async () => {
  await i18next.changeLanguage('en')
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/performance/resources') {
      return {
        data: {
          success: true,
          data: { node: 'gateway-a', server_time: now, sample: point },
        },
      }
    }
    if (url === '/api/performance/resources/history') {
      return { data: { success: true, data: history } }
    }
    throw new Error(`Unexpected request: ${url}`)
  })
})

afterEach(async () => {
  cleanup()
  client.clear()
  vi.useRealTimers()
  await i18next.changeLanguage('en')
})

function renderMonitor() {
  return render(
    <QueryClientProvider client={client}>
      <SystemResourceMonitor />
    </QueryClientProvider>
  )
}

describe('server resource monitoring', () => {
  it('defaults to 12 hours, separates network directions and switches the range', async () => {
    const user = userEvent.setup()
    renderMonitor()
    expect(await screen.findByText('37.5%')).toBeInTheDocument()
    expect(screen.getByText('↓ 1 MiB/s')).toBeInTheDocument()
    expect(screen.getByText('Upload: 512 KiB/s')).toBeInTheDocument()
    await waitFor(() =>
      expect(api.get).toHaveBeenCalledWith(
        '/api/performance/resources/history',
        expect.objectContaining({ params: { node: 'gateway-a', hours: 12 } })
      )
    )
    await user.click(
      screen.getByRole('combobox', { name: 'Monitoring time range' })
    )
    await user.click(
      await screen.findByRole('option', { name: 'Last 24 hours' })
    )
    await waitFor(() =>
      expect(api.get).toHaveBeenCalledWith(
        '/api/performance/resources/history',
        expect.objectContaining({ params: { node: 'gateway-a', hours: 24 } })
      )
    )
  })

  it('polls live data every five seconds without reloading the history', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    renderMonitor()
    await screen.findByText('37.5%')
    await waitFor(() => expect(api.get).toHaveBeenCalledTimes(2))
    await act(() => vi.advanceTimersByTimeAsync(5000))
    await waitFor(() =>
      expect(
        vi
          .mocked(api.get)
          .mock.calls.filter(([url]) => url === '/api/performance/resources')
      ).toHaveLength(2)
    )
    expect(
      vi
        .mocked(api.get)
        .mock.calls.filter(
          ([url]) => url === '/api/performance/resources/history'
        )
    ).toHaveLength(1)
  })

  it('marks stale readings as paused and keeps language-sensitive numbers reactive', async () => {
    client.setQueryData(['system-resources', 'current'], {
      node: 'gateway-a',
      server_time: now,
      sample: { ...point, timestamp: now - 120 },
    })
    renderMonitor()
    expect(screen.getByRole('status')).toHaveTextContent('Updates paused')
    await act(() => i18next.changeLanguage('fr'))
    expect(screen.getByText('37,5%')).toBeInTheDocument()
  })

  it('shows an explicit error with retry when live sampling cannot be loaded', async () => {
    vi.mocked(api.get).mockRejectedValue(new Error('offline'))
    renderMonitor()
    expect(
      await screen.findByText('Unable to load server resources')
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument()
    expect(screen.queryByText('0%')).not.toBeInTheDocument()
  })

  it('uses server time so a different browser clock does not hide live samples', () => {
    client.setQueryData(['system-resources', 'current'], {
      node: 'gateway-a',
      server_time: now - 3600,
      sample: { ...point, timestamp: now - 3600 },
    })
    renderMonitor()
    expect(screen.getByRole('status')).toHaveTextContent(
      'Live · every 5 seconds'
    )
    expect(screen.getByText('37.5%')).toBeInTheDocument()
  })

  it('preserves gaps, excludes out-of-range points and never mixes nodes', () => {
    const gapHistory = {
      ...history,
      points: [
        { ...point, timestamp: now - 600 },
        { ...point, timestamp: now - 300 },
        { ...point, timestamp: now - 60 },
      ],
    }
    const current = { node: 'gateway-a', server_time: now, sample: point }
    const timeline = buildResourceTimeline(gapHistory, current, now - 400, now)
    expect(timeline.map((p) => [p.timestamp, p.cpu_percent])).toEqual([
      [now - 300, 37.5],
      [now - 240, null],
      [now - 60, 37.5],
      [now, 37.5],
    ])
    expect(
      buildResourceTimeline(
        gapHistory,
        { ...current, node: 'gateway-b' },
        now - 400,
        now
      )
    ).toEqual([point])
    expect(
      buildResourceTimeline(
        undefined,
        { ...current, sample: null },
        now - 43200,
        now
      )
    ).toEqual([])
  })

  it('formats byte quantities with every interface locale and distinguishes missing data from zero', () => {
    for (const language of [
      'zhCN',
      'zhTW',
      'en',
      'fr',
      'ja',
      'ru',
      'vi',
      'invalid',
    ]) {
      const locale = toIntlLocale(language)
      expect(formatResourceBytes(1024, locale)).toBe('1 KiB')
      expect(formatResourceBytes(0, locale)).toBe('0 B')
      expect(formatResourceBytes(null, locale)).toBe('—')
    }
    expect(formatResourceBytes(1.5 * 1024 ** 3, toIntlLocale('fr'))).toBe(
      '1,5 GiB'
    )
  })
})
