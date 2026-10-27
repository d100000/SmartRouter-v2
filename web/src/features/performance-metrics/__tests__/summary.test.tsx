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
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { PerformanceOverview } from '@/features/dashboard/components/models/performance-overview'
import { PerformanceHealthPanel } from '@/features/dashboard/components/overview/performance-health-panel'
import { ModelReliabilityDemo } from '@/features/performance-metrics/components/model-reliability-demo'
import { ModelDetailsPerformance } from '@/features/pricing/components/model-details-performance'
import { UptimeSparkline } from '@/features/pricing/components/model-details-uptime-sparkline'
import { api } from '@/lib/api'

const chart = vi.hoisted(() =>
  vi.fn(
    (_props: {
      spec: { data: { id: string; values: Record<string, unknown>[] }[] }
    }) => null
  )
)
vi.mock('@visactor/react-vchart', () => ({ VChart: chart }))
vi.mock('@visactor/vchart', () => ({
  ThemeManager: { setCurrentTheme: vi.fn() },
}))

let client: QueryClient
beforeEach(() => {
  chart.mockClear()
  window.localStorage.removeItem('performance-health-model-columns-v1')
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
})
afterEach(() => {
  client.clear()
  vi.restoreAllMocks()
})

const summary = { success_rate: 99.01, avg_latency_ms: 1009, avg_tps: 5 }
const windowStart = 1789387200
const windowEnd = windowStart + 23 * 3600 + 1800
const groups = [
  {
    group: 'a',
    success_rate: 100,
    avg_latency_ms: 1000,
    avg_ttft_ms: 100,
    avg_tps: 5,
    series: [
      {
        ts: windowStart,
        avg_ttft_ms: 100,
        avg_latency_ms: 1000,
        success_rate: 100,
        avg_tps: 5,
      },
    ],
  },
  {
    group: 'b',
    success_rate: 0,
    avg_latency_ms: 2000,
    avg_ttft_ms: 0,
    avg_tps: 0,
    series: [
      {
        ts: windowStart,
        avg_ttft_ms: 0,
        avg_latency_ms: 2000,
        success_rate: 0,
        avg_tps: 0,
      },
    ],
  },
]

function renderModelDetails() {
  client.setQueryData(['perf-metrics', 'test-model'], {
    success: true,
    data: {
      model_name: 'test-model',
      summary,
      series: [
        {
          ts: windowStart,
          avg_ttft_ms: 100,
          avg_latency_ms: 1009,
          success_rate: 99.01,
          avg_tps: 5,
        },
      ],
      window_start: windowStart,
      window_end: windowEnd,
      groups,
    },
  })
  return render(
    <QueryClientProvider client={client}>
      <ModelDetailsPerformance
        model={{
          id: 1,
          model_name: 'test-model',
          quota_type: 0,
          model_ratio: 1,
          completion_ratio: 1,
          enable_groups: ['a', 'b'],
        }}
      />
    </QueryClientProvider>
  )
}

describe('server performance summaries', () => {
  it.each([PerformanceHealthPanel, PerformanceOverview])(
    'uses the weighted server result across models',
    (Component) => {
      client.setQueryData(['perf-metrics-summary', 24], {
        success: true,
        data: {
          summary,
          models: groups.map((group) => ({
            ...group,
            model_name: group.group,
          })),
        },
      })
      render(
        <QueryClientProvider client={client}>
          <Component />
        </QueryClientProvider>
      )
      expect(screen.getByText('99.01%')).toBeVisible()
      expect(screen.queryByText('50.00%')).not.toBeInTheDocument()
      expect(screen.getByText('1.01s')).toBeVisible()
    }
  )

  it('shows the server summary and series incident count for model details instead of averaging groups', () => {
    renderModelDetails()
    expect(screen.getByText('99.01%')).toBeVisible()
    expect(screen.queryByText('50.00%')).not.toBeInTheDocument()
    expect(screen.getByText('1 incidents in the last 24 hours')).toBeVisible()
  })

  it('feeds the uptime chart from the backend series rather than group averages', async () => {
    renderModelDetails()
    await waitFor(() => expect(chart).toHaveBeenCalled())
    const specs = chart.mock.calls.map(([props]) => props.spec)
    const uptime = specs.find((value) => value.data[0].id === 'uptime')
    const uptimeValues = uptime?.data[0].values.map((value) => value.uptime)
    expect(uptimeValues).toContain(99.01)
    expect(uptimeValues).not.toContain(50)
  })

  it('shows each group success rate in its own sparkline label', () => {
    renderModelDetails()
    expect(screen.getByText('100.00%')).toBeVisible()
    expect(screen.getByText('0.00%')).toBeVisible()
  })

  it('displays the supplied group summary instead of averaging hourly percentages', () => {
    render(
      <UptimeSparkline
        overallSuccessRate={99.01}
        series={[
          {
            date: '2026-09-14T11:00:00Z',
            uptime_pct: 100,
            incidents: 0,
            outage_minutes: 0,
          },
          {
            date: '2026-09-14T12:00:00Z',
            uptime_pct: 0,
            incidents: 1,
            outage_minutes: 0,
          },
        ]}
      />
    )
    expect(screen.getByText('99.01%')).toBeVisible()
    expect(screen.queryByText('50.00%')).not.toBeInTheDocument()
  })

  it('keeps an empty summary unknown instead of inventing a zero success rate', () => {
    client.setQueryData(['perf-metrics-summary', 24], {
      success: true,
      data: { models: [], summary: null },
    })
    render(
      <QueryClientProvider client={client}>
        <PerformanceHealthPanel />
      </QueryClientProvider>
    )
    expect(screen.getAllByText('—')).toHaveLength(3)
    expect(screen.queryByText('0.00%')).not.toBeInTheDocument()
  })

  it('filters live model rows by company', async () => {
    const user = userEvent.setup()
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        summary,
        models: [
          {
            model_name: 'gpt-4.1',
            company_id: 'openai',
            company_name: 'OpenAI',
            group_name: 'general',
            request_count: 100,
            success_rate: 99.8,
            avg_latency_ms: 400,
            avg_tps: 12,
            recent_success_series: [
              { ts: windowStart, success_rate: 99.8 },
              { ts: windowStart + 3600, success_rate: 100 },
            ],
          },
          {
            model_name: 'claude-3-7-sonnet',
            company_id: 'anthropic',
            company_name: 'Anthropic',
            group_name: 'reasoning',
            request_count: 80,
            success_rate: 98.8,
            avg_latency_ms: 600,
            avg_tps: 8,
            recent_success_series: [
              { ts: windowStart, success_rate: 98.8 },
              { ts: windowStart + 3600, success_rate: 99 },
            ],
          },
        ],
        companies: [
          { id: 'openai', name: 'OpenAI' },
          { id: 'anthropic', name: 'Anthropic' },
        ],
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    const companySelect = screen.getByRole('combobox', { name: 'Company' })

    expect(screen.getAllByText('gpt-4.1').length).toBeGreaterThan(0)
    await user.selectOptions(companySelect, 'anthropic')

    expect(screen.getAllByText('claude-3-7-sonnet').length).toBeGreaterThan(0)
    expect(screen.queryByText('gpt-4.1')).not.toBeInTheDocument()
  })

  it('keeps unassigned models available when configured companies exist', async () => {
    const user = userEvent.setup()
    client.setQueryData(['performance-health-companies'], {
      success: true,
      data: {
        companies: [{ id: 'openai', name: 'OpenAI', groups: [] }],
      },
    })
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        summary,
        models: [
          {
            model_name: 'gpt-4.1',
            company_id: 'openai',
            company_name: 'OpenAI',
            request_count: 100,
            success_rate: 99.8,
            avg_latency_ms: 400,
            avg_tps: 12,
          },
          {
            model_name: 'unmapped-model',
            request_count: 10,
            success_rate: 95,
            avg_latency_ms: 700,
            avg_tps: 4,
          },
        ],
        companies: [{ id: 'unassigned', name: 'Unassigned' }],
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    const companySelect = screen.getByRole('combobox', { name: 'Company' })
    expect(
      screen.getByRole('option', { name: 'Unassigned' })
    ).toBeInTheDocument()
    await user.selectOptions(companySelect, 'unassigned')

    expect(screen.getAllByText('unmapped-model').length).toBeGreaterThan(0)
    expect(screen.queryByText('gpt-4.1')).not.toBeInTheDocument()
  })

  it('shows explicit telemetry gaps after opening advanced diagnostics', async () => {
    const user = userEvent.setup()
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: { summary: null, models: [], companies: [] },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    expect(screen.queryByText('Error breakdown')).not.toBeInTheDocument()
    await user.click(
      screen.getByRole('button', { name: 'Advanced diagnostics' })
    )
    expect(await screen.findByText('Error breakdown')).toBeVisible()
    expect(screen.getByText('Routing resilience')).toBeVisible()
    expect(screen.getByText('Latency tail')).toBeVisible()
    expect(screen.getAllByText('Not collected yet').length).toBeGreaterThan(0)
  })

  it('keeps company configuration reachable when performance loading fails', async () => {
    vi.spyOn(api, 'get').mockRejectedValue(new Error('Metrics unavailable'))
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    await waitFor(() =>
      expect(screen.getByText('Unable to load model reliability')).toBeVisible()
    )
    expect(
      screen.getByRole('button', { name: 'Configure models' })
    ).toBeVisible()
  })

  it('shows the full success range and low values in the trend', () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        window_start: windowStart,
        window_end: windowEnd,
        models: [
          {
            model_name: 'unreliable-model',
            request_count: 100,
            success_rate: 77,
            avg_latency_ms: 400,
            avg_tps: 12,
            recent_success_series: [
              { ts: windowStart, success_rate: 0 },
              { ts: windowStart + 12 * 3600, success_rate: 77 },
            ],
          },
        ],
        companies: [],
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    const trend = screen.getByRole('img', {
      name: '24-hour success rate curve',
    })
    expect(within(trend).getByText('0%')).toBeVisible()
    expect(trend).toHaveTextContent('77.00%')
  })

  it('keeps configured models without requests unknown instead of degraded', () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        summary: { success_rate: 0, avg_latency_ms: 0, avg_tps: 0 },
        companies: [],
        models: [
          {
            model_name: 'waiting-model',
            company_id: 'moonshot',
            company_name: 'Kimi',
            request_count: 0,
            success_rate: null,
            avg_latency_ms: null,
            avg_tps: null,
            effective_slo: 99.9,
            min_samples: 100,
          },
        ],
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    expect(screen.getByText('waiting-model')).toBeVisible()
    expect(screen.getByText('No requests')).toBeVisible()
    expect(screen.queryByText('0.00%')).not.toBeInTheDocument()
    expect(screen.queryByText('Degraded')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('columnheader', { name: 'P95 latency' })
    ).not.toBeInTheDocument()
  })

  it('uses the saved effective SLO instead of a fixed success threshold', () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        companies: [],
        models: [
          {
            model_name: 'manual-target-model',
            company_id: 'custom-openai',
            company_name: 'Openai',
            request_count: 100,
            success_rate: 98,
            avg_latency_ms: 400,
            avg_tps: 12,
            effective_slo: 97,
            min_samples: 100,
            monitoring_enabled: true,
          },
        ],
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    expect(screen.getByText('Meets target')).toBeVisible()
    expect(screen.queryByText('Degraded')).not.toBeInTheDocument()
  })

  it('keeps a company success rate unknown when traffic has no success metric', () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        companies: [{ id: 'custom', name: 'Custom company' }],
        models: [
          {
            model_name: 'unknown-result',
            company_id: 'custom',
            company_name: 'Custom company',
            request_count: 100,
            success_rate: null,
            avg_latency_ms: null,
            avg_tps: null,
          },
        ],
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    const company = screen.getByRole('button', {
      name: /Custom company.*Models with traffic/,
    })
    expect(within(company).getByText('—')).toBeVisible()
    expect(within(company).queryByText('0.00%')).not.toBeInTheDocument()
  })

  it('does not count legacy null model arrays as configured models', () => {
    client.setQueryData(['performance-health-companies'], {
      success: true,
      data: {
        version: 4,
        companies: [
          {
            id: 'legacy',
            name: 'Legacy',
            groups: [{ id: 'empty', name: 'Empty', models: null }],
          },
        ],
      },
    })
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: { summary: null, models: [], companies: [] },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    expect(
      within(
        screen.getByRole('group', { name: 'Configured models' })
      ).getByText('0')
    ).toBeVisible()
  })

  it('keeps optional columns selectable and applies them when browser storage is unavailable', async () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: { summary: null, models: [], companies: [] },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )
    const user = userEvent.setup()
    expect(
      screen.queryByRole('columnheader', { name: 'P95 latency' })
    ).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Fields' }))
    const dialog = screen.getByRole('dialog', { name: 'Model table columns' })
    const p95 = within(dialog).getByRole('checkbox', { name: 'P95 latency' })
    expect(p95).not.toBeChecked()
    await user.click(p95)
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('Storage blocked')
    })
    await user.click(within(dialog).getByRole('button', { name: 'Apply' }))

    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Model table columns' })
      ).not.toBeInTheDocument()
    )
    expect(
      screen.getByRole('columnheader', { name: 'P95 latency' })
    ).toBeVisible()
  })
  it('keeps legacy metrics visible without claiming unavailable collection health', () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        summary,
        generated_at: windowEnd,
        models: [
          {
            model_name: 'legacy-model',
            request_count: 100,
            success_rate: 99.01,
            avg_latency_ms: 1009,
            avg_tps: 5,
          },
        ],
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    expect(
      screen.getByRole('status', { name: 'Collection health' })
    ).toHaveTextContent('Collection status unavailable')
    expect(screen.getByRole('button', { name: 'legacy-model' })).toBeVisible()
    expect(
      screen.getByText(
        'The server does not report collection health. Existing metrics remain available.'
      )
    ).toBeVisible()
    expect(
      screen.getByRole('definition', { name: 'Latest sample' })
    ).toHaveTextContent('—')
  })

  it('shows durable acceptance, projection backlog and unpersisted risk independently of query refresh', () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        generated_at: windowEnd,
        summary: null,
        models: [],
        collection: {
          enabled: true,
          availability: 'partial',
          storage: 'database_outbox',
          target_visibility_seconds: 60,
          pending_events: 4,
          oldest_pending_at: windowStart + 10,
          last_sample_at: windowStart,
          last_accepted_at: windowStart + 20,
          last_projected_at: windowStart + 5,
          persisted_through: null,
          collector_heartbeat_at: windowStart + 30,
          unpersisted_events: 2,
          oldest_unpersisted_at: windowStart + 1,
          dropped_events: 1,
          stale_collectors: 1,
          local_unpersisted_events: 1,
          quarantined_events: 3,
        },
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    expect(
      screen.getByRole('alert', { name: 'Collection health' })
    ).toHaveTextContent('Collection may be incomplete')
    expect(
      screen
        .getByRole('definition', { name: 'Latest sample' })
        .querySelector('time')
    ).toHaveAttribute('dateTime', new Date(windowStart * 1000).toISOString())
    expect(
      screen
        .getByRole('definition', { name: 'Durably accepted at' })
        .querySelector('time')
    ).toHaveAttribute(
      'dateTime',
      new Date((windowStart + 20) * 1000).toISOString()
    )
    expect(
      screen.getByRole('definition', { name: 'Pending projection' })
    ).toHaveTextContent('4')
    expect(
      screen.getByRole('definition', { name: 'Unpersisted samples' })
    ).toHaveTextContent('2')
    expect(
      screen.getByRole('definition', { name: 'Dropped samples' })
    ).toHaveTextContent('1')
    expect(
      screen.getByRole('definition', { name: 'Stale collectors' })
    ).toHaveTextContent('1')
    expect(
      screen.getByRole('definition', { name: 'Quarantined samples' })
    ).toHaveTextContent('3')
    expect(
      screen.getByText('See the server logs for the quarantine reason.')
    ).toBeVisible()
    expect(
      screen.getByRole('definition', { name: 'Persistence watermark' })
    ).toHaveTextContent('Not available')
  })

  it('distinguishes disabled collection from delayed projection and keeps persisted history', async () => {
    const data = {
      generated_at: windowEnd,
      summary: null,
      models: [],
      collection: {
        enabled: false,
        availability: 'disabled',
        storage: 'database_outbox',
        target_visibility_seconds: 60,
        pending_events: 0,
        oldest_pending_at: null,
        last_sample_at: null,
        last_accepted_at: null,
        last_projected_at: null,
        persisted_through: null,
        collector_heartbeat_at: windowStart,
        unpersisted_events: 0,
        oldest_unpersisted_at: null,
        dropped_events: 0,
        stale_collectors: 0,
        local_unpersisted_events: 0,
      },
    }
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data,
    })
    const rootRoute = createRootRoute({
      component: () => (
        <QueryClientProvider client={client}>
          <ModelReliabilityDemo />
        </QueryClientProvider>
      ),
    })
    const router = createRouter({
      routeTree: rootRoute,
      history: createMemoryHistory({ initialEntries: ['/'] }),
    })
    await router.load()
    render(<RouterProvider router={router} />)

    expect(
      screen.getByRole('status', { name: 'Collection health' })
    ).toHaveTextContent('Collection is disabled')
    expect(
      screen.getByText(
        'New samples are not collected. Previously persisted data remains available.'
      )
    ).toBeVisible()
    expect(
      screen.getByText(
        'No sample timestamp is available. Older buckets may not include one.'
      )
    ).toBeVisible()
    expect(
      screen.getByRole('link', { name: 'Open collection settings' })
    ).toHaveAttribute('href', '/system-settings/operations/alerts')
  })

  it('shows a confirmed zero output rate while keeping unavailable output metrics unknown', () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        summary: {
          success_rate: 100,
          avg_latency_ms: 100,
          avg_tps: 0,
          output_metrics_available: true,
        },
        models: [
          {
            model_name: 'zero-output',
            request_count: 100,
            success_rate: 100,
            avg_latency_ms: 100,
            avg_tps: 0,
            output_metrics_available: true,
          },
          {
            model_name: 'unknown-output',
            request_count: 100,
            success_rate: 100,
            avg_latency_ms: 100,
            avg_tps: 0,
            output_metrics_available: false,
          },
        ],
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    expect(screen.getAllByText('0 t/s')).toHaveLength(2)
    const unknown = screen
      .getByRole('button', { name: 'unknown-output' })
      .closest('tr')
    expect(unknown).not.toHaveTextContent('0 t/s')
  })
  it('keeps the last successful metrics visible and marks them stale when refresh fails', async () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: {
        summary,
        generated_at: windowEnd,
        models: [
          {
            model_name: 'persisted-model',
            request_count: 100,
            success_rate: 99.01,
            avg_latency_ms: 1009,
            avg_tps: 5,
          },
        ],
      },
    })
    vi.spyOn(api, 'get').mockRejectedValue(new Error('Refresh failed'))
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )
    await userEvent
      .setup()
      .click(screen.getByRole('button', { name: 'Refresh' }))

    expect(
      await screen.findByText('Unable to refresh model reliability')
    ).toBeVisible()
    expect(
      screen.getByRole('status', { name: 'Cached metrics' })
    ).toHaveTextContent('The latest query failed.')
    expect(
      screen.getByRole('alert', { name: 'Collection health' })
    ).toHaveTextContent('Collection status unavailable')
    expect(
      screen.getByRole('button', { name: 'persisted-model' })
    ).toBeVisible()
    expect(
      screen.getByRole('button', { name: 'Configure models' })
    ).toBeVisible()
  })
})
