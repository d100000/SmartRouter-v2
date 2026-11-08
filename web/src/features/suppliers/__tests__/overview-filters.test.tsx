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
import { act, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { createAppQueryClient } from '@/lib/query-client'
import { useAuthStore } from '@/stores/auth-store'

import { SupplyOverviewPanel } from '../components/overview'
import type { SupplyOverview } from '../types'

const overview: SupplyOverview = {
  channel_count: 1,
  available_channels: 1,
  request_count: 2,
  success_count: 2,
  failure_count: 0,
  unknown_count: 0,
  input_tokens: 20,
  output_tokens: 10,
  usage_known_count: 2,
  avg_latency_ms: 100,
  daily_used_usd: null,
  budget_day: '2026-11-09',
  updated_at: 0,
  channels: [],
}
const response = { data: { success: true, data: overview } }
let client: QueryClient

beforeEach(() => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  useAuthStore.getState().auth.setUser({
    id: 7,
    role: 2,
    username: 'supplier',
    supplier_id: 'supplier-a',
  })
})
afterEach(() => {
  client.clear()
  useAuthStore.getState().auth.reset('idle')
  vi.restoreAllMocks()
  vi.useRealTimers()
})

it('retains channel, model and date filters while loading and after a successful overview request', async () => {
  let complete!: (value: typeof response) => void
  const pending = new Promise<typeof response>((resolve) => {
    complete = resolve
  })
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValueOnce(response)
    .mockReturnValueOnce(pending)
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={client}>
      <SupplyOverviewPanel mode='supplier' />
    </QueryClientProvider>
  )
  await screen.findByRole('list', { name: 'Overview' })
  await user.type(screen.getByRole('textbox', { name: 'Channel ID' }), '9')
  await user.type(screen.getByRole('textbox', { name: 'Model' }), 'model-a')
  await user.click(screen.getByRole('combobox', { name: 'Time range' }))
  await user.click(screen.getByRole('option', { name: 'Last 24 hours' }))
  await user.click(screen.getByRole('button', { name: 'Filter' }))
  await screen.findByText('Loading...')
  expect(get).toHaveBeenLastCalledWith(
    '/api/supplier/overview',
    expect.objectContaining({
      params: expect.objectContaining({
        channel_id: 9,
        model: 'model-a',
        days: 1,
      }),
    })
  )
  expect(screen.getByRole('textbox', { name: 'Channel ID' })).toHaveValue('9')
  expect(screen.getByRole('textbox', { name: 'Model' })).toHaveValue('model-a')
  expect(
    screen.getByRole('combobox', { name: 'Time range' })
  ).toHaveTextContent('Last 24 hours')
  await user.type(screen.getByRole('textbox', { name: 'Model' }), '-draft')
  await act(async () => complete(response))
  await screen.findByRole('list', { name: 'Overview' })
  expect(screen.getByRole('textbox', { name: 'Channel ID' })).toHaveValue('9')
  expect(screen.getByRole('textbox', { name: 'Model' })).toHaveValue(
    'model-a-draft'
  )
  expect(
    screen.getByRole('combobox', { name: 'Time range' })
  ).toHaveTextContent('Last 24 hours')
})

it('retains a custom reporting range after HTTP 500 without redirecting and lets users change the failed filters', async () => {
  const onInternalServerError = vi.fn()
  client = createAppQueryClient(onInternalServerError)
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-11-09T12:00:00'))
  let fail!: (reason: Error) => void
  const pending = new Promise<typeof response>((_resolve, reject) => {
    fail = reject
  })
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValueOnce(response)
    .mockReturnValueOnce(pending)
    .mockResolvedValueOnce(response)
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={client}>
      <SupplyOverviewPanel mode='supplier' />
    </QueryClientProvider>
  )
  await screen.findByRole('list', { name: 'Overview' })
  await user.type(screen.getByRole('textbox', { name: 'Channel ID' }), '9')
  await user.type(screen.getByRole('textbox', { name: 'Model' }), 'model-a')
  await user.click(screen.getByRole('combobox', { name: 'Time range' }))
  await user.click(screen.getByRole('option', { name: 'Custom Time Range' }))
  await user.click(screen.getByRole('button', { name: 'Select start time' }))
  await user.click(
    within(screen.getByRole('grid')).getByRole('button', {
      name: /November 2nd, 2026/,
    })
  )
  await user.click(screen.getByRole('button', { name: 'Select end time' }))
  await user.click(
    within(screen.getByRole('grid')).getByRole('button', {
      name: /November 8th, 2026/,
    })
  )
  await user.click(screen.getByRole('button', { name: 'Filter' }))
  await screen.findByText('Loading...')
  expect(get).toHaveBeenLastCalledWith(
    '/api/supplier/overview',
    expect.objectContaining({
      params: expect.objectContaining({
        channel_id: 9,
        model: 'model-a',
        from: new Date('2026-11-02T00:00:00').getTime() / 1000,
        to: new Date('2026-11-08T00:00:00').getTime() / 1000,
      }),
    })
  )
  await act(async () =>
    fail(
      Object.assign(new Error('Overview unavailable'), {
        response: { status: 500 },
      })
    )
  )
  await screen.findByRole('button', { name: 'Retry' })
  expect(onInternalServerError).not.toHaveBeenCalled()
  expect(screen.getByRole('textbox', { name: 'Channel ID' })).toHaveValue('9')
  expect(screen.getByRole('textbox', { name: 'Model' })).toHaveValue('model-a')
  expect(
    screen.getByRole('combobox', { name: 'Time range' })
  ).toHaveTextContent('Custom Time Range')
  expect(screen.getByRole('button', { name: '2026-11-02' })).toBeVisible()
  expect(screen.getByRole('button', { name: '2026-11-08' })).toBeVisible()
  await user.clear(screen.getByRole('textbox', { name: 'Channel ID' }))
  await user.type(screen.getByRole('textbox', { name: 'Channel ID' }), '10')
  await user.click(screen.getByRole('button', { name: 'Filter' }))
  await screen.findByRole('list', { name: 'Overview' })
  expect(get).toHaveBeenLastCalledWith(
    '/api/supplier/overview',
    expect.objectContaining({
      params: expect.objectContaining({ channel_id: 10 }),
    })
  )
  expect(screen.getByRole('button', { name: '2026-11-02' })).toBeVisible()
  expect(
    screen.queryByRole('button', { name: 'Retry' })
  ).not.toBeInTheDocument()
})
