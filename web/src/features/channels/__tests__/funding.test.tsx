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
import type { ReactNode } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { ChannelFundingBadge } from '../components/channel-funding'
import { aggregateChannelsByTag } from '../lib/channel-utils'
import { channelSchema } from '../types'

const channel = channelSchema.parse({
  id: 42,
  name: 'Funding fixture',
  type: 1,
  key: '',
  status: 1,
  created_time: 0,
  test_time: 0,
  response_time: 0,
  balance_updated_time: 0,
  tag: 'provider',
  channel_info: { is_multi_key: true, multi_key_size: 3 },
  funding_status: {
    status: 'balance_insufficient',
    affected_keys: 1,
    total_keys: 3,
    last_seen: 100,
  },
})
const event = {
  id: 7,
  revision: 3,
  state: 'confirmed',
  category: 'balance_insufficient',
  key_index: 1,
  key_alias: 'Backup',
  model: 'text-model',
  endpoint: '/v1/chat/completions',
  source: 'http',
  status_code: 429,
  error_code: 'credit_balance_exhausted',
  evidence: '<script>insufficient balance</script>',
  first_seen: 100,
  last_seen: 200,
  hits: 2,
  auto_disabled: true,
  probe_status: 'pending',
  probe_detail: '',
  next_probe_at: 300,
  last_probe_at: 0,
  resolution: '',
}
let client: QueryClient
function Wrapper({ children }: { children: ReactNode }) {
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>
}
beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'root', role: ROLE.SUPER_ADMIN })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        incidents: [event],
        deliveries: [
          {
            id: 1,
            incident_id: 7,
            kind: 'initial',
            status: 'failed',
            attempts: 2,
            next_attempt_at: 200,
            sent_at: 0,
          },
        ],
        monitoring_enabled: true,
      },
    },
  })
  vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { task_id: 'probe-1' } },
  })
})
afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
})

test('opens evidence on demand and uses event revision for probe and dismissal', async () => {
  const user = userEvent.setup()
  render(<ChannelFundingBadge channel={channel} />, { wrapper: Wrapper })
  expect(api.get).not.toHaveBeenCalled()
  expect(
    screen.getByText('Insufficient funding · 1/3 keys')
  ).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Funding status' }))
  expect(await screen.findByText(event.evidence)).toBeInTheDocument()
  expect(document.querySelector('script')).toBeNull()
  await user.click(screen.getByRole('button', { name: 'Check recovery now' }))
  await waitFor(() =>
    expect(api.post).toHaveBeenCalledWith(
      '/api/channel/42/funding/probe',
      { incident_id: 7, revision: 3, reason: '' },
      expect.anything()
    )
  )
  await user.click(screen.getByRole('button', { name: 'Clear funding marker' }))
  const confirmation = screen.getByRole('alertdialog')
  expect(
    within(confirmation).getByRole('button', { name: 'Clear funding marker' })
  ).toBeDisabled()
  await user.type(
    within(confirmation).getByLabelText('Reason'),
    'Reviewed account'
  )
  await user.click(
    within(confirmation).getByRole('button', { name: 'Clear funding marker' })
  )
  await waitFor(() =>
    expect(api.post).toHaveBeenCalledWith(
      '/api/channel/42/funding/dismiss',
      { incident_id: 7, revision: 3, reason: 'Reviewed account' },
      expect.anything()
    )
  )
  expect(
    vi
      .mocked(api.post)
      .mock.calls.every(([url]) => String(url).includes('/funding/'))
  ).toBe(true)
})

test('read-only admins see evidence without operational controls', async () => {
  useAuthStore
    .getState()
    .auth.setUser({
      id: 2,
      username: 'reader',
      role: ROLE.ADMIN,
      permissions: {
        admin_permissions: { channel: { read: true, operate: false } },
      },
    })
  render(<ChannelFundingBadge channel={channel} />, { wrapper: Wrapper })
  await userEvent.click(screen.getByRole('button', { name: 'Funding status' }))
  await screen.findByText(event.evidence)
  expect(
    screen.queryByRole('button', { name: 'Clear funding marker' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Check recovery now' })
  ).not.toBeInTheDocument()
})

test('loading failures expose retry and no event does not claim a healthy balance', async () => {
  vi.mocked(api.get).mockRejectedValueOnce(new Error('offline'))
  render(
    <ChannelFundingBadge channel={{ ...channel, funding_status: null }} />,
    { wrapper: Wrapper }
  )
  await userEvent.click(screen.getByRole('button', { name: 'Funding status' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Failed to load funding events'
  )
  vi.mocked(api.get).mockResolvedValue({
    data: {
      success: true,
      data: { incidents: [], deliveries: [], monitoring_enabled: false },
    },
  })
  await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('No active funding event')).toBeInTheDocument()
  expect(
    screen.getByText('Channel funding monitoring is disabled')
  ).toBeInTheDocument()
})

test('tag aggregation counts all keys without mutating channel summaries', () => {
  const healthy = {
    ...channel,
    id: 43,
    funding_status: null,
    channel_info: { ...channel.channel_info, multi_key_size: 2 },
  }
  const [aggregate] = aggregateChannelsByTag([channel, healthy])
  expect(aggregate.funding_status).toMatchObject({
    status: 'balance_insufficient',
    affected_keys: 1,
    total_keys: 5,
  })
  expect(channel.funding_status?.total_keys).toBe(3)
})
