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
import {
  focusManager,
  QueryClient,
  QueryClientProvider,
} from '@tanstack/react-query'
import {
  act,
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosError, AxiosHeaders, type AxiosAdapter } from 'axios'
import { StrictMode, type ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { Scheduling } from '../index'
import type { SchedulingSnapshot } from '../types'

const originalAdapter = api.defaults.adapter
let client: QueryClient
let snapshot: SchedulingSnapshot

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
  snapshot = {
    groups: ['default', 'premium'],
    models: ['model-a', 'model-b'],
    group: 'default',
    model: 'model-a',
    stream: true,
    scope: 'instance',
    started_at: 1727000000,
    refreshed_at: 1727000120,
    active: false,
    phase: 'cold',
    activation_requests: 12,
    activation_threshold: 50,
    config: {
      enabled: true,
      target_success_rate: 0.99,
      target_ttft_ms: 2000,
      default_capacity: 20,
      channel_overrides: [],
    },
    summary: {
      requests_30m: 12,
      attempts_30m: 14,
      success_rate_30m: 0.9,
      success_rate_5m: 0.8,
      avg_ttft_ms_5m: null,
      healthy_channels: 1,
      eligible_channels: 2,
      degraded_channels: 1,
      in_flight: 3,
      top_traffic_share: 0.8,
    },
    trend: [],
    channels: [
      {
        channel_id: 1,
        name: 'Primary',
        status: 1,
        route_state: 'degraded',
        traffic_share: 0.8,
        effective_weight: 0.25,
        configured_weight: 50,
        priority: 10,
        dispatches_30m: 10,
        success_rate_30m: 0.9,
        success_rate_5m: 0.8,
        in_flight: 3,
        capacity: 20,
        health_attainment: 0.6,
        health_score: 65,
        health_baseline: 80,
        recovery_limit: 65,
        ramp_successes: 20,
        ramp_progress: 1,
        ramp_limited: false,
        avg_ttft_ms_5m: null,
        quality_score: 52,
        selection_probability: 0.4,
        can_recover: true,
      },
      {
        channel_id: 2,
        name: 'Disabled upstream',
        status: 2,
        route_state: 'disabled',
        traffic_share: 0.2,
        effective_weight: 0,
        configured_weight: 30,
        priority: 1,
        dispatches_30m: 4,
        success_rate_30m: null,
        success_rate_5m: null,
        in_flight: 0,
        capacity: 20,
        health_attainment: null,
        health_score: 100,
        health_baseline: 100,
        recovery_limit: 100,
        ramp_successes: 0,
        ramp_progress: 0,
        ramp_limited: false,
        avg_ttft_ms_5m: null,
        quality_score: 80,
        selection_probability: 0,
        can_recover: false,
      },
    ],
  }
  vi.spyOn(api, 'get').mockImplementation(async (_url, config) => ({
    data: {
      success: true,
      data: {
        ...snapshot,
        group: config?.params?.group || snapshot.group,
        model: config?.params?.model || snapshot.model,
        stream: config?.params?.stream ?? true,
      },
    },
  }))
  vi.spyOn(api, 'post').mockResolvedValue({ data: { success: true } })
})

afterEach(() => {
  cleanup()
  client.clear()
  vi.useRealTimers()
  focusManager.setFocused(undefined)
  api.defaults.adapter = originalAdapter
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('routing dashboard interactions', () => {
  test.each([
    ['cold', 'Cold start'],
    ['transition', 'Routing transition'],
    ['dynamic', 'Mature dynamic routing'],
  ] as const)(
    'shows the backend learning stage for %s routing',
    async (phase, label) => {
      snapshot.phase = phase
      snapshot.active = phase !== 'cold'
      render(<Scheduling />, { wrapper: Wrapper })
      expect(await screen.findByText(label)).toBeVisible()
    }
  )

  test('new channel rollout and current health limits remain inspectable by keyboard', async () => {
    const user = userEvent.setup()
    snapshot.active = true
    snapshot.phase = 'dynamic'
    snapshot.channels[0].ramp_limited = true
    snapshot.channels[0].ramp_progress = 0.25
    snapshot.channels[0].ramp_successes = 5
    render(<Scheduling />, { wrapper: Wrapper })
    await user.click(await screen.findByRole('tab', { name: 'Groups' }))
    expect(screen.getByText('Gradual rollout: 25%')).toBeVisible()
    expect(
      screen.getByRole('columnheader', { name: 'Initial configured weight' })
    ).toBeVisible()
    const healthDetails = screen.getByRole('button', {
      name: 'Health details for Primary',
    })
    await user.tab()
    await user.tab()
    expect(healthDetails).toHaveFocus()
    expect(await screen.findByRole('tooltip')).toHaveTextContent(
      'Window baseline: 80%. Recovery limit: 65%.'
    )
    expect(healthDetails).toHaveAccessibleDescription(
      'Window baseline: 80%. Recovery limit: 65%.'
    )
  })

  test('shows activation progress and all channel metrics while retaining missing latency as a dash', async () => {
    const user = userEvent.setup()
    render(<Scheduling />, { wrapper: Wrapper })
    expect(
      await screen.findByText('Activation progress: 12 / 50 original requests')
    ).toBeVisible()
    await user.click(screen.getByRole('tab', { name: 'Groups' }))
    const table = screen.getByRole('table')
    expect(within(table).getAllByRole('columnheader')).toHaveLength(20)
    const row = within(table).getByText('Primary').closest('tr')
    expect(row).not.toBeNull()
    if (!row) throw new Error('Primary channel row not found')
    expect(within(row).getByText('40%')).toBeVisible()
    expect(within(row).getByText('—')).toBeVisible()
    expect(
      within(table).queryByRole('button', {
        name: 'Remove downweighting for Disabled upstream',
      })
    ).not.toBeInTheDocument()
    expect(table.parentElement).toHaveClass('overflow-x-auto')
    expect(within(row).getByText('Primary').closest('td')).toHaveClass(
      'sticky',
      'left-0'
    )
    expect(
      within(row)
        .getByRole('button', { name: 'Remove downweighting for Primary' })
        .closest('td')
    ).toHaveClass('sticky', 'right-0')
    expect(
      within(table).getByRole('columnheader', { name: 'Channel Name' })
    ).toHaveClass('sticky', 'left-0')
    expect(
      within(table).getByRole('columnheader', { name: 'Actions' })
    ).toHaveClass('sticky', 'right-0')
  })

  test('StrictMode remount loads a fresh snapshot instead of reusing the cancelled first request', async () => {
    vi.mocked(api.get).mockRestore()
    const transport = vi.fn<AxiosAdapter>(async (config) => ({
      data: { success: true, data: snapshot },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    }))
    api.defaults.adapter = transport
    render(
      <StrictMode>
        <QueryClientProvider client={client}>
          <Scheduling />
        </QueryClientProvider>
      </StrictMode>
    )
    expect(await screen.findByText('Native routing')).toBeVisible()
    expect(transport).toHaveBeenCalled()
    expect(
      screen.queryByText(
        'Unable to load routing health. Retry to get the latest channel state.'
      )
    ).not.toBeInTheDocument()
  })

  test('narrow-screen filters keep selected group and model labels readable in full-width rows', async () => {
    vi.stubGlobal('innerWidth', 390)
    render(<Scheduling />, { wrapper: Wrapper })
    const group = (await screen.findByText('default')).closest('button')
    const model = screen.getByText('model-a').closest('button')
    if (!group || !model) throw new Error('Filter triggers not found')
    expect(within(group).getByText('default')).not.toHaveClass('hidden')
    expect(within(model).getByText('model-a')).not.toHaveClass('hidden')
    expect(group).toHaveClass('w-full', 'min-w-0', 'max-w-full')
    expect(model).toHaveClass('w-full', 'min-w-0', 'max-w-full')
    expect(group.parentElement).toHaveClass('grid', 'grid-cols-1', 'sm:flex')
    vi.unstubAllGlobals()
  })

  test('an enabled channel without routing eligibility shows unavailable and cannot be recovered', async () => {
    const user = userEvent.setup()
    snapshot.channels = [
      {
        ...snapshot.channels[0],
        route_state: 'ineligible',
        effective_weight: 0,
        selection_probability: 0,
        can_recover: false,
      },
    ]
    render(<Scheduling />, { wrapper: Wrapper })
    await user.click(await screen.findByRole('tab', { name: 'Groups' }))
    const table = screen.getByRole('table')
    expect(within(table).getByText('Enabled')).toBeVisible()
    expect(within(table).getByText('Unavailable')).toBeVisible()
    expect(within(table).queryByText('Disabled')).not.toBeInTheDocument()
    expect(
      within(table).queryByRole('button', {
        name: 'Remove downweighting for Primary',
      })
    ).not.toBeInTheDocument()
  })

  test('switching stream mode fetches separate measurements and retains the selected group and model', async () => {
    const user = userEvent.setup()
    render(<Scheduling />, { wrapper: Wrapper })
    await screen.findByText('Native routing')
    await user.click(screen.getByRole('switch', { name: 'Streaming' }))
    await waitFor(() =>
      expect(api.get).toHaveBeenLastCalledWith(
        '/api/scheduling',
        expect.objectContaining({
          params: { group: 'default', model: 'model-a', stream: false },
        })
      )
    )
    expect(
      await screen.findByRole('switch', { name: 'Non-streaming' })
    ).not.toBeChecked()
  })

  test('later polling uses the resolved group and model even when the server default changes', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    focusManager.setFocused(true)
    render(<Scheduling />, { wrapper: Wrapper })
    await screen.findByText('model-a')
    await waitFor(() => expect(client.isFetching()).toBe(0))
    const callsBeforePoll = vi.mocked(api.get).mock.calls.length
    snapshot.model = 'model-b'
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15000)
    })
    await waitFor(() =>
      expect(vi.mocked(api.get).mock.calls.length).toBeGreaterThan(
        callsBeforePoll
      )
    )
    expect(api.get).toHaveBeenLastCalledWith(
      '/api/scheduling',
      expect.objectContaining({
        params: { group: 'default', model: 'model-a', stream: true },
      })
    )
    expect(screen.getByText('model-a')).toBeVisible()
  })

  test('polling preserves an explicitly selected model instead of adopting later defaults', async () => {
    const user = userEvent.setup()
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    focusManager.setFocused(true)
    render(<Scheduling />, { wrapper: Wrapper })
    await user.click(await screen.findByText('model-a'))
    await user.click(await screen.findByRole('option', { name: 'model-b' }))
    await screen.findByText('model-b')
    await waitFor(() => expect(client.isFetching()).toBe(0))
    const callsBeforePoll = vi.mocked(api.get).mock.calls.length
    snapshot.model = 'model-a'
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15000)
    })
    await waitFor(() =>
      expect(vi.mocked(api.get).mock.calls.length).toBeGreaterThan(
        callsBeforePoll
      )
    )
    expect(api.get).toHaveBeenLastCalledWith(
      '/api/scheduling',
      expect.objectContaining({
        params: { group: 'default', model: 'model-b', stream: true },
      })
    )
    expect(screen.getByText('model-b')).toBeVisible()
  })

  test('changing groups resolves that groups default model once and pins it for subsequent polls', async () => {
    const user = userEvent.setup()
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    focusManager.setFocused(true)
    render(<Scheduling />, { wrapper: Wrapper })
    await user.click(await screen.findByText('default'))
    snapshot.model = 'model-b'
    await user.click(await screen.findByRole('option', { name: 'premium' }))
    await waitFor(() =>
      expect(api.get).toHaveBeenCalledWith(
        '/api/scheduling',
        expect.objectContaining({
          params: { group: 'premium', model: '', stream: true },
        })
      )
    )
    await screen.findByText('model-b')
    await waitFor(() => expect(client.isFetching()).toBe(0))
    const callsBeforePoll = vi.mocked(api.get).mock.calls.length
    snapshot.model = 'model-a'
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15000)
    })
    await waitFor(() =>
      expect(vi.mocked(api.get).mock.calls.length).toBeGreaterThan(
        callsBeforePoll
      )
    )
    expect(api.get).toHaveBeenLastCalledWith(
      '/api/scheduling',
      expect.objectContaining({
        params: { group: 'premium', model: 'model-b', stream: true },
      })
    )
    expect(screen.getByText('premium')).toBeVisible()
    expect(screen.getByText('model-b')).toBeVisible()
  })

  test('an empty default route remains unresolved until polling discovers a model with channels', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    focusManager.setFocused(true)
    const channels = snapshot.channels
    snapshot.channels = []
    render(<Scheduling />, { wrapper: Wrapper })
    await screen.findByText('model-a')
    await waitFor(() => expect(client.isFetching()).toBe(0))
    expect(api.get).toHaveBeenCalledTimes(1)
    snapshot.model = 'model-b'
    snapshot.channels = channels
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15000)
    })
    await screen.findByText('model-b')
    await waitFor(() =>
      expect(api.get).toHaveBeenLastCalledWith(
        '/api/scheduling',
        expect.objectContaining({
          params: { group: 'default', model: 'model-b', stream: true },
        })
      )
    )
  })

  test('retry resolves fresh defaults when the selected model has been removed without repinning cached defaults', async () => {
    const user = userEvent.setup()
    render(<Scheduling />, { wrapper: Wrapper })
    await screen.findByText('model-a')
    await waitFor(() => expect(client.isFetching()).toBe(0))
    const error = new AxiosError('Selection unavailable')
    error.response = {
      status: 400,
      statusText: 'Bad Request',
      data: { code: 'scheduling_selection_unavailable' },
      headers: {},
      config: { headers: new AxiosHeaders() },
    }
    vi.mocked(api.get).mockRejectedValueOnce(error)
    await act(async () => {
      await client.refetchQueries({ queryKey: ['scheduling'], type: 'active' })
    })
    await screen.findByRole('button', { name: 'Retry' })
    snapshot.model = 'model-b'
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('model-b')).toBeVisible()
    await waitFor(() =>
      expect(api.get).toHaveBeenLastCalledWith(
        '/api/scheduling',
        expect.objectContaining({
          params: { group: 'default', model: 'model-b', stream: true },
        })
      )
    )
  })

  test.each([
    ['network', new Error('Network unavailable')],
    [
      'server',
      Object.assign(new AxiosError('Service unavailable'), {
        response: {
          status: 500,
          data: { code: 'scheduling_selection_unavailable' },
        },
      }),
    ],
    [
      'unrelated client',
      Object.assign(new AxiosError('Bad Request'), {
        response: { status: 400, data: { code: 'other_validation_error' } },
      }),
    ],
  ])(
    'retry preserves the selected model after a %s error',
    async (_name, error) => {
      const user = userEvent.setup()
      render(<Scheduling />, { wrapper: Wrapper })
      await user.click(await screen.findByText('model-a'))
      await user.click(await screen.findByRole('option', { name: 'model-b' }))
      await screen.findByText('model-b')
      await waitFor(() => expect(client.isFetching()).toBe(0))
      vi.mocked(api.get).mockRejectedValueOnce(error)
      await act(async () => {
        await client.refetchQueries({
          queryKey: ['scheduling'],
          type: 'active',
        })
      })
      await user.click(await screen.findByRole('button', { name: 'Retry' }))
      expect(await screen.findByText('model-b')).toBeVisible()
      expect(api.get).toHaveBeenLastCalledWith(
        '/api/scheduling',
        expect.objectContaining({
          params: { group: 'default', model: 'model-b', stream: true },
        })
      )
    }
  )

  test('recovery failure keeps the confirmation open and success refetches channel weights', async () => {
    const user = userEvent.setup()
    vi.mocked(api.post)
      .mockResolvedValueOnce({
        data: { success: false, message: 'Cooldown is active' },
      })
      .mockResolvedValueOnce({ data: { success: true } })
    render(<Scheduling />, { wrapper: Wrapper })
    await user.click(await screen.findByRole('tab', { name: 'Groups' }))
    await user.click(
      screen.getByRole('button', { name: 'Remove downweighting for Primary' })
    )
    expect(screen.getByRole('alertdialog')).toHaveTextContent(
      'Failure history, disabled status, cooldowns and capacity limits remain in effect. Poor recent performance can still keep the weight low.'
    )
    await user.click(screen.getByRole('button', { name: 'Recalculate weight' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Weight recalculation failed'
    )
    snapshot.channels[0].can_recover = false
    await user.click(screen.getByRole('button', { name: 'Recalculate weight' }))
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(api.post).toHaveBeenLastCalledWith('/api/scheduling/recover', {
      group: 'default',
      model: 'model-a',
      channel_id: 1,
    })
    expect(
      screen.queryByRole('button', { name: 'Remove downweighting for Primary' })
    ).not.toBeInTheDocument()
  })

  test('saving routing settings sends inherited values as null and keeps explicit zero channel weight', async () => {
    const user = userEvent.setup()
    render(<Scheduling />, { wrapper: Wrapper })
    await screen.findByText('Native routing')
    await user.click(
      screen.getByRole('button', { name: 'Routing configuration' })
    )
    const dialog = screen.getByRole('dialog')
    await user.type(
      within(dialog).getAllByRole('spinbutton', {
        name: 'Initial configured weight',
      })[0],
      '0'
    )
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(api.post).toHaveBeenCalledWith(
        '/api/scheduling/config',
        expect.objectContaining({
          group: 'default',
          model: 'model-a',
          target_success_rate: 0.99,
          channel_overrides: [
            { channel_id: 1, weight: 0, capacity: null, capacity_key: '' },
            { channel_id: 2, weight: null, capacity: null, capacity_key: '' },
          ],
        })
      )
    )
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
  })

  test('an invalid concurrency value stays in the form without saving and Escape closes the dialog', async () => {
    const user = userEvent.setup()
    render(<Scheduling />, { wrapper: Wrapper })
    await screen.findByText('Native routing')
    const openButton = screen.getByRole('button', {
      name: 'Routing configuration',
    })
    await user.click(openButton)
    const dialog = screen.getByRole('dialog')
    const capacity = within(dialog).getByRole('spinbutton', {
      name: 'Default safe concurrency',
    })
    await user.clear(capacity)
    await user.type(capacity, '0')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    expect(api.post).not.toHaveBeenCalled()
    expect(capacity).toBeInvalid()
    await user.keyboard('{Escape}')
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
  })

  test('a pending health request displays loading before any channel metrics', () => {
    vi.mocked(api.get).mockImplementation(() => new Promise(() => undefined))
    render(<Scheduling />, { wrapper: Wrapper })
    expect(screen.getByText('Loading...')).toBeVisible()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Routing configuration' })
    ).toBeDisabled()
  })

  test('read-only administrators can inspect channels without configuration or recovery actions', async () => {
    const user = userEvent.setup()
    useAuthStore.getState().auth.setUser({
      id: 2,
      username: 'observer',
      role: ROLE.ADMIN,
      permissions: { admin_permissions: { channel: { read: true } } },
    })
    render(<Scheduling />, { wrapper: Wrapper })
    await screen.findByText('Native routing')
    expect(
      screen.queryByRole('button', { name: 'Routing configuration' })
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Groups' }))
    expect(
      screen.queryByRole('button', { name: 'Remove downweighting for Primary' })
    ).not.toBeInTheDocument()
  })

  test('administrators without read permission see access denied without fetching channel data', async () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 3, username: 'restricted', role: ROLE.ADMIN })
    render(<Scheduling />, { wrapper: Wrapper })
    expect(screen.getByText('Access Forbidden')).toBeVisible()
    expect(api.get).not.toHaveBeenCalled()
  })

  test('a failed dashboard request offers retry and an empty result explains how to start', async () => {
    const user = userEvent.setup()
    vi.mocked(api.get).mockRejectedValueOnce(new Error('Service unavailable'))
    snapshot.groups = []
    snapshot.models = []
    snapshot.model = ''
    snapshot.channels = []
    render(<Scheduling />, { wrapper: Wrapper })
    expect(
      await screen.findByText(
        'Unable to load routing health. Retry to get the latest channel state.'
      )
    ).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(
      await screen.findByText('No channel routes configured')
    ).toBeVisible()
    expect(
      screen.getByRole('button', { name: 'Routing configuration' })
    ).toBeDisabled()
  })
})
