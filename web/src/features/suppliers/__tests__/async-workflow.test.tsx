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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { SupplyAsyncMetricsPanel } from '../components/async-metrics'
import { ChannelEditor } from '../components/channel-editor'
import { ChannelLimitsDialog } from '../components/channel-limits-dialog'
import { SupplyLogs } from '../components/logs'
import { SupplyFilters } from '../components/supply-filters'
import { SupplyTaskStatus } from '../components/task-status'
import { supplierChannelTemplates } from '../lib/templates'
import type { SupplierAccount, SupplyAsyncMetrics, SupplyLog } from '../types'

let client: QueryClient
beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  useAuthStore
    .getState()
    .auth.setUser({ id: 7, role: 2, username: 'supplier-a', supplier_id: 'a' })
})
afterEach(() => {
  client.clear()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset('idle')
})
function withClient(element: React.ReactNode) {
  return render(
    <QueryClientProvider client={client}>{element}</QueryClientProvider>
  )
}
const task: SupplyLog = {
  id: 'own-task',
  channel_id: 9,
  channel_name: 'Own video channel',
  created_at: 1794120000,
  model: 'video-a',
  stage: 'task_submit',
  protocol: 'async_task',
  result: 'success',
  status_code: 202,
  duration_ms: 20,
  input_tokens: null,
  output_tokens: null,
  usage_source: 'unknown',
  correlation_id: 'supplier-local-task',
  task_status: 'queued',
  task_observed_at: 1794120001,
  task_completed_at: null,
  task_duration_ms: null,
  video_seconds: null,
  credits: null,
}
const metrics: SupplyAsyncMetrics = {
  submitted_count: 9,
  accepted_count: 6,
  rejected_count: 1,
  unknown_submission_count: 2,
  queued_count: 1,
  running_count: 1,
  succeeded_count: 2,
  failed_count: 1,
  unknown_count: 1,
  avg_submit_latency_ms: 20,
  avg_completion_ms: 45000,
}

describe('supplier asynchronous task outcomes', () => {
  it('keeps accepted submissions separate from confirmed task success and excludes unknowns from both rates', () => {
    render(<SupplyAsyncMetricsPanel metrics={metrics} />)
    const panel = within(
      screen.getByRole('region', { name: 'Asynchronous tasks' })
    )
    expect(
      panel.getByText('Submission acceptance rate').nextElementSibling
    ).toHaveTextContent('85.71%')
    expect(
      panel.getByText('Task success rate').nextElementSibling
    ).toHaveTextContent('66.67%')
    expect(
      panel.getByText('Queued tasks').nextElementSibling
    ).toHaveTextContent(/^1$/)
    expect(
      panel.getByText('Running tasks').nextElementSibling
    ).toHaveTextContent(/^1$/)
    expect(
      panel.getByText('Unknown task results').nextElementSibling
    ).toHaveTextContent(/^1$/)
  })
  it('shows insufficient samples when accepted tasks have no confirmed terminal results', () => {
    render(
      <SupplyAsyncMetricsPanel
        metrics={{
          ...metrics,
          succeeded_count: 0,
          failed_count: 0,
          avg_completion_ms: null,
        }}
      />
    )
    expect(
      screen.getByText('Task success rate').nextElementSibling
    ).toHaveTextContent('Insufficient samples')
    expect(
      screen.getByText('Average task completion time (ms)').nextElementSibling
    ).toHaveTextContent('Unknown')
  })
  it('does not label a queued task as successful after its submission was accepted', () => {
    render(<SupplyTaskStatus log={task} />)
    expect(screen.getByText('Queued')).toBeVisible()
    expect(screen.queryByText('Confirmed success')).not.toBeInTheDocument()
    expect(screen.queryByText('Task completed at')).not.toBeInTheDocument()
    expect(
      screen.getByText('Task completion time (ms)').nextElementSibling
    ).toHaveTextContent('Unknown')
  })
  it('shows a late confirmed result and observation time without changing the submission result', () => {
    const view = render(
      <SupplyTaskStatus log={{ ...task, task_status: 'unknown' }} />
    )
    expect(screen.getByText('Unknown task result')).toBeVisible()
    view.rerender(
      <SupplyTaskStatus
        log={{
          ...task,
          task_status: 'succeeded',
          task_completed_at: 1794120045,
          task_duration_ms: 45000,
        }}
      />
    )
    expect(screen.getByText('Confirmed success')).toBeVisible()
    expect(screen.getByText('Task completed at')).toBeVisible()
    expect(
      screen.getByText('Task completion time (ms)').nextElementSibling
    ).toHaveTextContent('45,000')
  })
  it('preserves exact credits and unknown video quantities independently of text tokens in the log table', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          items: [
            {
              ...task,
              credits: '9007199254740993.123456789',
              credit_usage_source: 'upstream_usage',
            },
          ],
          total: 1,
        },
      },
    })
    withClient(<SupplyLogs />)
    expect(
      (await screen.findAllByText('9007199254740993.123456789')).length
    ).toBeGreaterThan(0)
    expect(screen.getAllByText('Submission accepted').length).toBeGreaterThan(0)
    expect(screen.getAllByText('Queued').length).toBeGreaterThan(0)
    expect(screen.getAllByText('Upstream usage').length).toBeGreaterThan(0)
    expect(screen.queryByText('Confirmed success')).not.toBeInTheDocument()
  })
  it('filters task completion state independently and sends the filter to the scoped log query', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: { items: [], total: 0 } },
    })
    const user = userEvent.setup()
    withClient(<SupplyLogs />)
    await user.click(
      await screen.findByRole('combobox', { name: 'Task status' })
    )
    await user.click(screen.getByRole('option', { name: 'Confirmed failure' }))
    await user.click(screen.getByRole('button', { name: 'Filter' }))
    await waitFor(() =>
      expect(get).toHaveBeenCalledWith(
        '/api/supplier/logs',
        expect.objectContaining({
          params: expect.objectContaining({
            task_status: 'failed',
            result: undefined,
          }),
        })
      )
    )
  })
  it.each([
    ['Realtime generation', 'realtime_generation'],
    ['Realtime transcription', 'realtime_transcription'],
  ])(
    'filters %s separately from websocket connections',
    async (label, stage) => {
      const change = vi.fn()
      const user = userEvent.setup()
      render(<SupplyFilters onChange={change} />)
      await user.click(screen.getByRole('combobox', { name: 'Stage' }))
      await user.click(screen.getByRole('option', { name: label }))
      await user.click(screen.getByRole('button', { name: 'Filter' }))
      expect(change).toHaveBeenCalledWith(expect.objectContaining({ stage }))
    }
  )
  it('labels realtime transcription separately and preserves audio quantities without inventing text tokens', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          items: [
            {
              ...task,
              stage: 'realtime_transcription',
              protocol: 'realtime',
              task_status: '',
              audio_seconds: 12.5,
              audio_usage_source: 'upstream_usage',
            },
          ],
          total: 1,
        },
      },
    })
    withClient(<SupplyLogs />)
    expect(
      (await screen.findAllByText('Realtime transcription')).length
    ).toBeGreaterThan(0)
    expect(screen.getAllByText('12.5').length).toBeGreaterThan(0)
    expect(screen.queryByText('Realtime generation')).not.toBeInTheDocument()
    expect(screen.queryByText('Submission accepted')).not.toBeInTheDocument()
    expect(screen.queryByText('Queued')).not.toBeInTheDocument()
    const inputColumn = screen.getAllByRole('columnheader', {
      name: 'Input Tokens',
    })[0]
    const outputColumn = screen.getAllByRole('columnheader', {
      name: 'Output Tokens',
    })[0]
    const table = screen.getAllByRole('table')[0]
    const headers = within(table).getAllByRole('columnheader')
    const row = within(table).getAllByRole('row')[1]
    const cells = within(row).getAllByRole('cell')
    expect(cells[headers.indexOf(inputColumn)]).toHaveTextContent('-')
    expect(cells[headers.indexOf(outputColumn)]).toHaveTextContent('-')
  })
})

const supplier: SupplierAccount = {
  id: 'a',
  name: 'Supplier A',
  user_id: 7,
  supply_enabled: true,
  login_enabled: true,
  allowed_types: [1, 61],
  allowed_templates: ['1', 'plugin:video-a'],
  allowed_models: ['text-a', 'video-a'],
  max_channels: 10,
  group: 'default',
  revision: 1,
  cost_ratio: null,
  pool_priority: 1,
  pool_weight: 1,
  max_output_tokens: 100,
}
describe('server-authorized supplier templates', () => {
  it('authorizes a specific plugin template without enabling every installed plugin of type 61', () => {
    const templates = supplierChannelTemplates(supplier, [
      {
        id: '1',
        name: 'Text',
        type: 1,
        models: ['text-a'],
        protocols: ['http'],
        tpm_applicable: true,
      },
      {
        id: 'plugin:video-a',
        name: 'Video A',
        type: 61,
        models: ['video-a'],
        protocols: ['async_task'],
        tpm_applicable: false,
      },
      {
        id: 'plugin:video-b',
        name: 'Video B',
        type: 61,
        models: ['video-b'],
        protocols: ['async_task'],
        tpm_applicable: false,
      },
    ])
    expect(templates.map((template) => template.id)).toEqual([
      '1',
      'plugin:video-a',
    ])
    expect(templates[1].tpm_applicable).toBe(false)
  })
  it('keeps the plugin template identity and disables inapplicable TPM when creating a media channel', () => {
    withClient(
      <ChannelEditor
        templates={[
          {
            id: 'plugin:video-a',
            name: 'Video A',
            type: 61,
            models: ['video-a'],
            protocols: ['async_task'],
            tpm_applicable: false,
          },
        ]}
        onClose={vi.fn()}
      />
    )
    expect(
      screen.getByRole('combobox', { name: 'Channel template' })
    ).toHaveTextContent('Video A')
    expect(screen.getByLabelText('TPM')).toBeDisabled()
    expect(screen.getByLabelText('TPM')).toHaveAttribute(
      'placeholder',
      'Not applicable'
    )
    expect(
      screen.getByText(
        'Asynchronous models become available only after a test task is confirmed successful.'
      )
    ).toBeVisible()
  })
  it('does not silently clear an existing TPM value when editing another limit on a media-only channel', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          config: {
            channel_id: 9,
            revision: 4,
            rpm: null,
            tpm: 600,
            daily_usd: null,
            reconcile_seconds: 300,
            budget_ready: true,
            tpm_applicable: false,
          },
          usage: {
            rpm_used: 0,
            tpm_used: 0,
            tpm_reserved: 0,
            daily_used_usd: null,
            daily_reserved_usd: null,
            daily_remaining_usd: null,
            budget_day: '2026-11-08',
            reset_at: 0,
            reason: '',
          },
        },
      },
    })
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true, data: {} } })
    const user = userEvent.setup()
    withClient(
      <ChannelLimitsDialog
        channelId={9}
        channelName='Video'
        audience='supplier'
        onClose={vi.fn()}
      />
    )
    expect(await screen.findByLabelText('TPM')).toBeDisabled()
    await user.type(screen.getByLabelText('RPM'), '8')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith('/api/supplier/channels/9/limits', {
        revision: 4,
        rpm: 8,
        tpm: 600,
        daily_usd: null,
      })
    )
  })
})
