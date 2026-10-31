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
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosHeaders, type AxiosAdapter } from 'axios'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { ErrorAnalysis } from '../index'
import type { AnalysisConfig, AnalysisTask, Snapshot } from '../types'

const streamEvents = vi.hoisted(
  () => new Map<string, (event?: unknown) => void>()
)
vi.mock('@/lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/api')>()),
  getFreshAuthHeaders: async () => ({ 'New-Api-User': '1' }),
}))
vi.mock('sse.js', () => ({
  SSE: class {
    addEventListener(name: string, listener: (event?: unknown) => void) {
      streamEvents.set(name, listener)
    }
    stream() {}
    close() {}
  },
}))

const originalAdapter = api.defaults.adapter
let client: QueryClient
let config: AnalysisConfig
let snapshot: Snapshot
let tasks: AnalysisTask[]
let writes: { url?: string; data: Record<string, unknown> }[]
let reads: Record<string, unknown>[]

function Wrapper(props: { children: ReactNode }) {
  return (
    <QueryClientProvider client={client}>{props.children}</QueryClientProvider>
  )
}

beforeEach(() => {
  streamEvents.clear()
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  config = {
    version: 1,
    enabled: false,
    endpoint: 'https://example.com/v1/chat/completions',
    model: 'test-model',
    timezone: 'Asia/Shanghai',
    hour: 3,
    max_attempts: 3,
    timeout_seconds: 240,
    idle_seconds: 60,
    sample_limit: 60,
    report_language: 'en',
    api_key_configured: false,
  }
  snapshot = {
    filter: { start: 1000, end: 2000 },
    requests: 0,
    affected: 0,
    zero_output: 0,
    interrupted: 0,
    errors: 0,
    client_cancelled: 0,
    unknown_output: 0,
    recovered: 0,
    total_log_rows: 0,
    scanned_log_rows: 0,
    truncated: false,
    log_recording_enabled: true,
    error_recording_enabled: true,
    models: [],
    channels: [],
    reasons: [],
    timeline: [],
    logs: [],
  }
  tasks = []
  writes = []
  reads = []
  api.defaults.adapter = (async (request) => {
    let data: unknown = null
    if (request.method === 'put' || request.method === 'post') {
      const body = JSON.parse(request.data) as Record<string, unknown>
      writes.push({ url: request.url, data: body })
      if (request.url?.endsWith('/config')) {
        config = { ...config, ...body, api_key_configured: true }
        data = config
      } else {
        const task: AnalysisTask = {
          task_id: 'new-report',
          status: 'succeeded',
          payload: body as AnalysisTask['payload'],
          created_at: 3000,
          updated_at: 3001,
          error: '',
          state: null,
          result: {
            report: 'A new report',
            model: 'test',
            snapshot,
            sample_count: 0,
            attempts: [],
          },
        }
        tasks.unshift(task)
        data = { task, created: true }
      }
    } else if (request.url === '/api/error-analysis/config') {
      data = config
    } else if (request.url === '/api/error-analysis') {
      reads.push(request.params as Record<string, unknown>)
      data = { snapshot, log_total: snapshot.logs.length, offset: 0 }
    } else if (request.url === '/api/error-analysis/reports') {
      data = { items: tasks, total: tasks.length }
    } else {
      data = tasks.find((task) => request.url?.endsWith(task.task_id))
    }
    return {
      data: { success: true, data },
      status: 200,
      statusText: 'OK',
      headers: new AxiosHeaders(),
      config: request,
    }
  }) as AxiosAdapter
})

afterEach(() => {
  api.defaults.adapter = originalAdapter
  client.clear()
})

describe('Error analysis workflow', () => {
  test('SSE disconnection falls back to polling and exposes a stalled queue and worker failure', async () => {
    const now = Math.floor(Date.now() / 1000)
    tasks = [
      {
        task_id: 'queued-report',
        status: 'pending',
        payload: { start: now - 86400, end: now },
        created_at: now - 180,
        updated_at: now - 180,
        error: '',
        state: null,
        result: null,
      },
    ]
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await user.click(screen.getByRole('tab', { name: 'AI analysis & history' }))
    expect(
      await screen.findByText(
        'Analysis is still queued. Check that a master node is running and can access the task database.'
      )
    ).toBeVisible()
    await waitFor(() => expect(streamEvents.has('error')).toBe(true))
    act(() => streamEvents.get('error')?.())
    expect(
      screen.getByText(
        'Live connection interrupted. Progress is refreshed automatically; the background task continues.'
      )
    ).toBeVisible()
    tasks[0] = { ...tasks[0], status: 'failed', error: 'task lease expired' }
    expect(
      await screen.findByText(
        'The worker stopped responding. Start a new analysis to recover.',
        {},
        { timeout: 8000 }
      )
    ).toBeVisible()
  })
  test('an unconfigured model disables analysis and empty data has a recovery hint', async () => {
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    expect(
      await screen.findByText('No requests in this time range')
    ).toBeVisible()
    expect(
      screen.getByRole('button', { name: 'Start analysis' })
    ).toBeDisabled()
    expect(
      screen.getByText('Check log recording or select another time range.')
    ).toBeVisible()
  })

  test('settings reject an insecure endpoint and preserve an empty key field', async () => {
    config.api_key_configured = true
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await screen.findByText('No requests in this time range')
    await user.click(screen.getByRole('button', { name: 'Analysis settings' }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByLabelText('API Key')).toHaveValue('')
    const endpoint = within(dialog).getByLabelText('Chat completions endpoint')
    await user.clear(endpoint)
    await user.type(endpoint, 'http://private.example/v1/chat/completions')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'Use an HTTPS'
    )
    expect(writes).toHaveLength(0)
    await user.clear(endpoint)
    await user.type(endpoint, 'https://example.com/v1/chat/completions')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
    expect(writes[0].data.api_key).toBe('')
    expect(writes[0].data.version).toBe(1)
  })

  test('a failed historic report exposes the model failure and reruns the original time range', async () => {
    config.api_key_configured = true
    snapshot.error_recording_enabled = false
    tasks = [
      {
        task_id: 'old-report',
        status: 'failed',
        payload: { start: 1000, end: 2000, model: 'historic-model' },
        created_at: 2000,
        updated_at: 2001,
        error: 'analysis_idle_timeout',
        state: {
          phase: 'failed',
          attempt: 3,
          max_attempts: 3,
          updated_at: 2001,
          next_retry_at: 0,
          report: '',
          attempts: [],
        },
        result: {
          report: 'Partial diagnosis',
          model: 'test-model',
          snapshot,
          sample_count: 1,
          attempts: [
            {
              number: 1,
              started_at: 2000,
              finished_at: 2001,
              code: 'analysis_idle_timeout',
              status_code: 0,
              retryable: true,
            },
          ],
        },
      },
    ]
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await user.click(screen.getByRole('tab', { name: 'AI analysis & history' }))
    expect(
      await screen.findByText(
        'Any text below is an incomplete draft. A new analysis creates a separate history entry.'
      )
    ).toBeVisible()
    expect(
      screen.getAllByText(
        'The model stopped producing tokens. Check provider latency or increase the no-token timeout.'
      ).length
    ).toBeGreaterThan(0)
    const report = screen.getByRole('region', { name: 'Analysis report' })
    expect(
      within(report).getByText('Model: historic-model · Channel ID: All')
    ).toBeVisible()
    expect(within(report).getByText('Analysis model: test-model')).toBeVisible()
    expect(
      within(report).getByText(
        'Request error logging is disabled. Enable ERROR_LOG_ENABLED on gateway nodes to include ordinary request failures.'
      )
    ).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Analyze again' }))
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0].data).toEqual({
      start: 1000,
      end: 2000,
      model: 'historic-model',
    })
    expect(tasks).toHaveLength(2)
    expect(tasks[1].task_id).toBe('old-report')
  })

  test('limited coverage and missing output evidence remain visible with diagnostic details', async () => {
    snapshot.truncated = true
    snapshot.total_log_rows = 100
    snapshot.scanned_log_rows = 50
    snapshot.unknown_output = 1
    snapshot.logs = [
      {
        id: 1,
        request_id: 'diagnostic-request',
        upstream_request_id: 'upstream-trace',
        created_at: 1000,
        model: 'legacy-model',
        channel: 5,
        is_stream: true,
        completion_tokens: 0,
        attempts: 1,
        categories: ['unknown_output'],
        diagnostic: { end_reason: 'eof', received_chunks: 0 },
      },
    ]
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    expect(
      await screen.findByText(
        'Only the latest 50 of 100 log rows were scanned. Narrow the time range for complete statistics.'
      )
    ).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'View details' }))
    const dialog = screen.getByRole('dialog')
    expect(await within(dialog).findByText('diagnostic-request')).toBeVisible()
    expect(
      within(dialog).getByRole('button', { name: 'Copy to clipboard' })
    ).toBeEnabled()
    expect(within(dialog).getByText(/"end_reason": "eof"/)).toBeVisible()
  })
  test('exact request search disables unrelated filters and clears the prior category', async () => {
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await screen.findByText('No requests in this time range')
    await user.selectOptions(
      screen.getByLabelText('Category'),
      'settlement_risk'
    )
    await user.type(
      screen.getByLabelText('Request ID'),
      '  request-across-days  '
    )
    expect(screen.getByLabelText('Time range')).toBeDisabled()
    expect(screen.getByLabelText('Model')).toBeDisabled()
    expect(screen.getByLabelText('Channel ID')).toBeDisabled()
    await user.click(screen.getByRole('button', { name: 'Apply filters' }))
    await waitFor(() =>
      expect(reads.at(-1)).toMatchObject({
        request_id: 'request-across-days',
        category: '',
        offset: 0,
      })
    )
    expect(screen.getByLabelText('Category')).toHaveValue('')
    await user.clear(screen.getByLabelText('Request ID'))
    expect(screen.getByLabelText('Time range')).toBeEnabled()
  })

  test('request details load on demand and show full upstream errors, usage and settlement evidence', async () => {
    const longMessage = `upstream tool transport failed ${'x'.repeat(2000)} end-of-error`
    snapshot.settlement_risk = 1
    snapshot.logs = [
      {
        id: 9,
        request_id: 'billed-failed',
        upstream_request_id: 'provider-9',
        created_at: 1000,
        model: 'test',
        channel: 7,
        is_stream: true,
        prompt_tokens: 118353,
        completion_tokens: 27,
        attempts: 1,
        log_type: 2,
        quota: 23763,
        recorded_quota: 23763,
        categories: ['settlement_risk'],
        diagnostic: { response_status: 'failed', status_code: 502 },
        evidence: {
          version: 1,
          upstream: [
            {
              attempt: 1,
              channel: 7,
              request_id: 'provider-9',
              status_code: 200,
              error_body: longMessage,
              usage: {
                usage: { prompt_tokens: 118353, completion_tokens: 27 },
              },
            },
          ],
          local: {
            state: 'confirmed',
            confirmed_quota: 23763,
            requested_quota: 23763,
            pre_consumed_quota: 10000,
          },
          downstream: {
            status_code: 200,
            completed: false,
            failed: true,
            write_failed: false,
            capture_incomplete: false,
          },
        },
      },
    ]
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await screen.findByText('billed-failed')
    expect(reads.every((read) => !read.details)).toBe(true)
    await user.click(screen.getByRole('button', { name: 'View details' }))
    const dialog = screen.getByRole('dialog')
    expect(await within(dialog).findByText(longMessage)).toBeVisible()
    expect(reads.at(-1)).toMatchObject({
      request_id: 'billed-failed',
      details: true,
    })
    expect(
      within(dialog).getAllByText('Funding settlement confirmed')[0]
    ).toBeVisible()
    expect(within(dialog).getByText(/"prompt_tokens": 118353/)).toBeVisible()
    expect(within(dialog).getByText(longMessage)).toHaveAttribute(
      'tabindex',
      '0'
    )
    expect(within(dialog).getByText(longMessage)).toHaveClass(
      'overflow-auto',
      'whitespace-pre-wrap'
    )
    expect(
      within(dialog).getByText(
        'Provider usage is not a billing receipt. Downstream delivery does not confirm downstream settlement.'
      )
    ).toBeVisible()
  })
  test('a failed detail fetch shows retry and does not substitute the incomplete list record', async () => {
    snapshot.logs = [
      {
        id: 5,
        request_id: 'retry-detail',
        upstream_request_id: '',
        created_at: 1000,
        model: 'test',
        channel: 1,
        is_stream: false,
        completion_tokens: 0,
        attempts: 1,
        categories: ['error'],
        diagnostic: {},
      },
    ]
    const adapter = api.defaults.adapter as AxiosAdapter
    let failDetails = true
    api.defaults.adapter = async (request) => {
      if (
        request.url === '/api/error-analysis' &&
        request.params?.details &&
        failDetails
      ) {
        throw new Error('detail fetch unavailable')
      }
      return adapter(request)
    }
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await screen.findByText('retry-detail')
    await user.click(screen.getByRole('button', { name: 'View details' }))
    const dialog = screen.getByRole('dialog')
    const retry = await within(dialog).findByRole('button', { name: 'Retry' })
    expect(
      within(dialog).queryByText('Recorded consumption quota')
    ).not.toBeInTheDocument()
    failDetails = false
    await user.click(retry)
    expect(await within(dialog).findByText('retry-detail')).toBeVisible()
  })
})
