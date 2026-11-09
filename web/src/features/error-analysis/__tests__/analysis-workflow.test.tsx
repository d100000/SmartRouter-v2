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
import { AxiosError, AxiosHeaders, type AxiosAdapter } from 'axios'
import i18next from 'i18next'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { createAppQueryClient } from '@/lib/query-client'

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
      } else if (request.url?.endsWith('/resume')) {
        const task = tasks.find((item) => request.url?.includes(item.task_id))
        if (!task) throw new Error('Unknown analysis task')
        task.status = 'running'
        task.error = ''
        if (task.state) {
          task.state.phase = 'analyzing'
          task.state.can_resume = false
        }
        data = { task, created: false }
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
  cleanup()
  api.defaults.adapter = originalAdapter
  client.clear()
})

describe('Error analysis workflow', () => {
  test.each([
    '/api/error-analysis/config',
    '/api/error-analysis',
    '/api/error-analysis/reports',
    '/api/error-analysis/reports/saved-report',
  ])(
    'keeps unsaved settings when %s refresh returns HTTP 500',
    async (failedPath) => {
      const onInternalServerError = vi.fn()
      client = createAppQueryClient(onInternalServerError)
      tasks = [
        {
          task_id: 'saved-report',
          status: 'succeeded',
          payload: { start: 1000, end: 2000 },
          created_at: 2000,
          updated_at: 2100,
          error: '',
          state: null,
          result: {
            report: 'Saved report',
            model: 'test-model',
            snapshot,
            sample_count: 0,
            attempts: [],
          },
        },
      ]
      const user = userEvent.setup()
      render(<ErrorAnalysis />, { wrapper: Wrapper })
      await screen.findByText('No requests in this time range')
      if (failedPath.includes('/reports')) {
        await user.click(
          screen.getByRole('tab', { name: 'AI analysis & history' })
        )
        await screen.findByText('Saved report')
      }
      await user.click(
        screen.getByRole('button', { name: 'Analysis settings' })
      )
      const dialog = screen.getByRole('dialog')
      await user.clear(within(dialog).getByLabelText('Model'))
      await user.type(within(dialog).getByLabelText('Model'), 'unsaved-model')
      await user.type(
        within(dialog).getByLabelText('API Key'),
        'test-analysis-key'
      )
      const adapter = api.defaults.adapter as AxiosAdapter
      api.defaults.adapter = async (request) => {
        if (request.url === failedPath) {
          throw new AxiosError(
            'Unavailable',
            'ERR_BAD_RESPONSE',
            request,
            undefined,
            {
              data: { success: false, message: 'Unavailable' },
              status: 500,
              statusText: 'Internal Server Error',
              headers: new AxiosHeaders(),
              config: request,
            }
          )
        }
        return adapter(request)
      }
      await act(async () =>
        client.refetchQueries({ queryKey: ['error-analysis'] })
      )
      expect(onInternalServerError).not.toHaveBeenCalled()
      expect(within(dialog).getByLabelText('Model')).toHaveValue(
        'unsaved-model'
      )
      expect(within(dialog).getByLabelText('API Key')).toHaveValue(
        'test-analysis-key'
      )
      expect(writes).toHaveLength(0)
    }
  )

  test('context settings reject a budget that leaves no room for input', async () => {
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await screen.findByText('No requests in this time range')
    await user.click(screen.getByRole('button', { name: 'Analysis settings' }))
    const dialog = screen.getByRole('dialog')
    const context = within(dialog).getByLabelText(
      'Model context limit (tokens)'
    )
    expect(context).toHaveValue(32768)
    expect(
      within(dialog).queryByLabelText('No-token timeout (seconds)')
    ).not.toBeInTheDocument()
    await user.clear(context)
    await user.type(context, '4096')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'The context limit must exceed the output reserve and safety margin.'
    )
    expect(writes).toHaveLength(0)
    await user.clear(context)
    await user.type(context, '32768')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
    expect(writes[0].data).toMatchObject({
      context_window_tokens: 32768,
      chunk_input_tokens: 8000,
      max_output_tokens: 2048,
      safety_margin_tokens: 4096,
      max_calls: 100,
    })
  })

  test('a resumable analysis keeps completed segments and resumes the same history entry', async () => {
    config.api_key_configured = true
    tasks = [
      {
        task_id: 'resumable-report',
        status: 'failed',
        payload: { start: 1000, end: 2000 },
        created_at: 2000,
        updated_at: 2100,
        error: 'analysis_call_budget_exhausted',
        state: {
          version: 2,
          phase: 'paused',
          attempt: 0,
          max_attempts: 3,
          updated_at: 2100,
          next_retry_at: 0,
          report: '',
          attempts: [],
          scanned_rows: 120,
          total_rows: 120,
          completed_chunks: 2,
          total_chunks: 5,
          current_step: 'Timeout investigation',
          can_resume: true,
          calls_used: 3,
          max_calls: 3,
        },
        result: null,
      },
    ]
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await user.click(screen.getByRole('tab', { name: 'AI analysis & history' }))
    const report = await screen.findByRole('region', {
      name: 'Analysis report',
    })
    expect(
      await within(report).findByText('Segments completed: 2 / 5')
    ).toBeVisible()
    expect(within(report).getByText('Timeout investigation')).toBeVisible()
    await user.click(
      within(report).getByRole('button', { name: 'Continue analysis' })
    )
    await waitFor(() =>
      expect(writes).toContainEqual({
        url: '/api/error-analysis/reports/resumable-report/resume',
        data: {},
      })
    )
    expect(tasks).toHaveLength(1)
    expect(await within(report).findByText('Analyzing segments')).toBeVisible()
    expect(within(report).getByText('Segments completed: 2 / 5')).toBeVisible()
    expect(
      within(report).getByRole('button', { name: 'Analyze again' })
    ).toBeDisabled()
  })

  test('collection with an unknown total shows counts without an invented percentage', async () => {
    tasks = [
      {
        task_id: 'collecting-report',
        status: 'running',
        payload: { start: 1000, end: 2000 },
        created_at: 2000,
        updated_at: 2100,
        error: '',
        result: null,
        state: {
          version: 2,
          phase: 'collecting',
          attempt: 0,
          max_attempts: 3,
          updated_at: 2100,
          next_retry_at: 0,
          report: '',
          attempts: [],
          scanned_rows: 120,
          total_rows: 0,
          total_known: false,
        },
      },
    ]
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await user.click(screen.getByRole('tab', { name: 'AI analysis & history' }))
    const report = await screen.findByRole('region', {
      name: 'Analysis report',
    })
    expect(
      await within(report).findByText('Log rows scanned: 120 / Unknown')
    ).toBeVisible()
    expect(within(report).queryByRole('progressbar')).not.toBeInTheDocument()
  })

  test('a partial versioned report shows its summary and missing comparison without claiming completion', async () => {
    tasks = [
      {
        task_id: 'partial-report',
        status: 'failed',
        payload: { start: 1000, end: 2000 },
        created_at: 2000,
        updated_at: 2100,
        error: '',
        state: null,
        result: {
          version: 2,
          summary: 'Timeouts remain concentrated on one channel.',
          report: '',
          model: 'test',
          snapshot: null,
          sample_count: 0,
          attempts: [],
          complete: false,
          comparison_available: false,
          sections: [
            {
              id: 'priority',
              title: 'Timeout investigation',
              content: 'Check upstream response times.',
            },
          ],
          coverage: {
            scanned_rows: 90,
            total_rows: 100,
            completed_chunks: 2,
            total_chunks: 3,
          },
        },
      },
    ]
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await user.click(screen.getByRole('tab', { name: 'AI analysis & history' }))
    const report = await screen.findByRole('region', {
      name: 'Analysis report',
    })
    expect(
      await within(report).findByText(
        'Timeouts remain concentrated on one channel.'
      )
    ).toBeVisible()
    expect(
      within(report).getByText(
        'This report covers only completed analysis. Unfinished segments are listed in the progress above.'
      )
    ).toBeVisible()
    expect(
      within(report).getByText(
        'Previous-period data is unavailable or incomplete; changes cannot be compared.'
      )
    ).toBeVisible()
    expect(
      await within(report).findByText('Check upstream response times.')
    ).toBeVisible()
    expect(
      within(report).queryByText('Exported full report')
    ).not.toBeInTheDocument()
    expect(
      within(report).queryByRole('button', { name: 'Copy to clipboard' })
    ).not.toBeInTheDocument()
  })

  test('period comparison uses each periods request denominator and labels the report timezone', async () => {
    tasks = [
      {
        task_id: 'compared-report',
        status: 'succeeded',
        payload: { start: 1000, end: 2000 },
        created_at: 2000,
        updated_at: 2100,
        error: '',
        state: null,
        result: {
          version: 2,
          summary: 'Traffic increased while the affected-request rate fell.',
          report: 'Full report',
          model: 'test',
          sample_count: 0,
          attempts: [],
          complete: true,
          timezone: 'Pacific/Honolulu',
          timeline_interval: 'day',
          comparison_available: true,
          snapshot: { ...snapshot, requests: 200, affected: 20 },
          comparison: {
            ...snapshot,
            filter: { start: 0, end: 1000 },
            requests: 100,
            affected: 15,
          },
        },
      },
    ]
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await user.click(screen.getByRole('tab', { name: 'AI analysis & history' }))
    const comparison = await screen.findByRole('region', {
      name: 'Period comparison',
    })
    const affected = within(comparison).getByRole('row', {
      name: /Affected requests/,
    })
    expect(within(affected).getByText('20')).toBeVisible()
    expect(within(affected).getByText('15')).toBeVisible()
    expect(within(affected).getByText('15% → 10%')).toBeVisible()
    expect(
      within(comparison).getByText('Timezone: Pacific/Honolulu')
    ).toBeVisible()
  })

  test('a saved report opens its captured evidence instead of querying live logs', async () => {
    const row = {
      id: 9,
      request_id: 'captured-request',
      upstream_request_id: '',
      created_at: 1500,
      model: 'test',
      channel: 1,
      is_stream: false,
      completion_tokens: 0,
      attempts: 1,
      categories: ['error'],
      diagnostic: {},
      error_message: 'Captured at analysis time',
    }
    tasks = [
      {
        task_id: 'captured-report',
        status: 'succeeded',
        payload: { start: 1000, end: 2000 },
        created_at: 2000,
        updated_at: 2100,
        error: '',
        state: null,
        result: {
          version: 2,
          summary: 'A request failed.',
          report: 'Full report',
          model: 'test',
          snapshot: { ...snapshot, logs: [] },
          sections: [
            {
              id: 'issue-1',
              title: 'A failed request',
              content: 'Inspect the saved request.',
              evidence_ids: ['captured-request'],
            },
          ],
          sample_count: 1,
          attempts: [],
          complete: true,
        },
      },
    ]
    const adapter = api.defaults.adapter as AxiosAdapter
    let capturedEvidenceRead = false
    api.defaults.adapter = async (request) => {
      if (
        request.url === '/api/error-analysis/reports/captured-report/evidence'
      ) {
        capturedEvidenceRead = true
        expect(request.params.request_id).toBe('captured-request')
        return {
          data: { success: true, data: row },
          status: 200,
          statusText: 'OK',
          headers: new AxiosHeaders(),
          config: request,
        }
      }
      return adapter(request)
    }
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await user.click(screen.getByRole('tab', { name: 'AI analysis & history' }))
    await user.click(
      await screen.findByRole('button', { name: 'captured-request' })
    )
    expect(
      await within(screen.getByRole('dialog')).findByText(
        'Captured at analysis time'
      )
    ).toBeVisible()
    expect(capturedEvidenceRead).toBe(true)
    expect(reads.every((read) => !read.details)).toBe(true)
  })

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
    expect(within(report).queryByText(/Elapsed:/)).not.toBeInTheDocument()
    expect(within(report).queryByText(/Output limit:/)).not.toBeInTheDocument()
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

  test('a completed report shows per-attempt timing, output limits and why a retry was needed', async () => {
    tasks = [
      {
        task_id: 'diagnosed-report',
        status: 'succeeded',
        payload: { start: 1000, end: 2000 },
        created_at: 2000,
        updated_at: 2600,
        error: '',
        state: null,
        result: {
          report: 'Complete diagnosis',
          complete: false,
          model: 'test-model',
          snapshot: null,
          snapshot_omitted: true,
          sample_count: 1,
          attempts: [
            {
              number: 1,
              started_at: 2000,
              finished_at: 2300,
              code: 'analysis_incomplete_report',
              status_code: 200,
              retryable: true,
              duration_ms: 282949,
              timeout_seconds: 600,
              idle_seconds: 60,
              max_output_tokens: 6000,
              finish_reason: 'length',
            },
            {
              number: 2,
              started_at: 2301,
              finished_at: 2600,
              code: 'completed',
              status_code: 200,
              retryable: false,
              duration_ms: 299000,
              timeout_seconds: 600,
              idle_seconds: 60,
              max_output_tokens: 12000,
              finish_reason: 'stop',
            },
          ],
        },
      },
    ]
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await user.click(screen.getByRole('tab', { name: 'AI analysis & history' }))
    const diagnostics = await screen.findByText('Model execution diagnostics')
    await user.click(diagnostics)
    const attempts = screen.getByRole('region', { name: 'Analysis report' })
    expect(
      within(attempts).queryByText('Partial analysis')
    ).not.toBeInTheDocument()
    expect(within(attempts).getByText('Elapsed: 282.949 seconds')).toBeVisible()
    expect(within(attempts).getAllByText('Timeout: 600 seconds')).toHaveLength(
      2
    )
    expect(
      within(attempts).getAllByText('No-token timeout: 60 seconds')
    ).toHaveLength(2)
    expect(
      within(attempts).getByText('Output limit: 6,000 tokens')
    ).toBeVisible()
    expect(
      within(attempts).getByText('Output limit: 12,000 tokens')
    ).toBeVisible()
    expect(
      within(attempts).getByText('Finish reason: Output token limit reached')
    ).toBeVisible()
    expect(within(attempts).getByText('Finish reason: Completed')).toBeVisible()
    expect(
      within(attempts).getByText(
        'Diagnostic samples could not be saved. The analysis report was preserved.'
      )
    ).toBeVisible()
    try {
      for (const [language, elapsed, limit] of [
        ['zhCN', '282.949', '12,000'],
        ['zhTW', '282.949', '12,000'],
        ['fr', '282,949', '12\u202f000'],
        ['ru', '282,949', '12\u00a0000'],
        ['ja', '282.949', '12,000'],
        ['vi', '282,949', '12.000'],
        ['not_a_locale', '282.949', '12,000'],
        ['en', '282.949', '12,000'],
      ]) {
        // Keep the labels stable while checking numbers after each language change.
        i18next.addResourceBundle(language, 'translation', {
          'Elapsed: {{seconds}} seconds': 'Elapsed: {{seconds}} seconds',
          'Output limit: {{tokens}} tokens': 'Output limit: {{tokens}} tokens',
        })
        await act(() => i18next.changeLanguage(language))
        expect(
          within(attempts).getByText(`Elapsed: ${elapsed} seconds`)
        ).toBeVisible()
        expect(
          within(attempts).getByText(`Output limit: ${limit} tokens`, {
            normalizer: (value) => value,
          })
        ).toBeVisible()
      }
    } finally {
      await act(() => i18next.changeLanguage('en'))
    }
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
    const onInternalServerError = vi.fn()
    client = createAppQueryClient(onInternalServerError)
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
        throw new AxiosError(
          'detail fetch unavailable',
          'ERR_BAD_RESPONSE',
          request,
          undefined,
          {
            data: { success: false, message: 'Unavailable' },
            status: 500,
            statusText: 'Internal Server Error',
            headers: new AxiosHeaders(),
            config: request,
          }
        )
      }
      return adapter(request)
    }
    const user = userEvent.setup()
    render(<ErrorAnalysis />, { wrapper: Wrapper })
    await screen.findByText('retry-detail')
    await user.click(screen.getByRole('button', { name: 'View details' }))
    const dialog = screen.getByRole('dialog')
    const retry = await within(dialog).findByRole('button', { name: 'Retry' })
    expect(onInternalServerError).not.toHaveBeenCalled()
    expect(
      within(dialog).queryByText('Recorded consumption quota')
    ).not.toBeInTheDocument()
    failDetails = false
    await user.click(retry)
    expect(await within(dialog).findByText('retry-detail')).toBeVisible()
  })
})
