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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { PerformanceOverview } from '@/features/dashboard/components/models/performance-overview'
import { PerformanceHealthPanel } from '@/features/dashboard/components/overview/performance-health-panel'
import { ModelReliabilityDemo } from '@/features/performance-metrics/components/model-reliability-demo'
import { ModelDetailsPerformance } from '@/features/pricing/components/model-details-performance'
import { UptimeSparkline } from '@/features/pricing/components/model-details-uptime-sparkline'

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
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
})
afterEach(() => client.clear())

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

  it('shows explicit telemetry gaps in operational diagnostics', () => {
    client.setQueryData(['performance-health-summary', 24], {
      success: true,
      data: { summary: null, models: [], companies: [] },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelReliabilityDemo />
      </QueryClientProvider>
    )

    expect(screen.getByText('Error breakdown')).toBeVisible()
    expect(screen.getByText('Routing resilience')).toBeVisible()
    expect(screen.getByText('Latency tail')).toBeVisible()
    expect(screen.getAllByText('Not collected yet').length).toBeGreaterThan(0)
  })
})
