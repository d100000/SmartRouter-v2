/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { PerformanceModelTable } from '../components/performance-model-table'
import type { PerformanceModelRow } from '../lib/model-status'

const columns = [
  { id: 'model', label: 'Model' },
  { id: 'company', label: 'Company' },
  { id: 'requests', label: 'Requests' },
  { id: 'success', label: 'Success rate' },
  { id: 'p95', label: 'P95 latency' },
  { id: 'incidents', label: 'Incidents' },
  { id: 'status', label: 'Status' },
] as const

function model(name: string, requests: number): PerformanceModelRow {
  return {
    model: name,
    companyId: 'manual',
    companyName: 'Manual company',
    groupName: 'Production',
    isAssigned: true,
    requests,
    successRate: 98,
    firstAttemptRate: 98,
    retryRecoveryRate: 0,
    p95LatencyMs: 400,
    avgLatencyMs: 200,
    p95TtftMs: 50,
    tps: 30,
    incidents: 0,
    series: [],
    effectiveSlo: 97,
    minSamples: 100,
    monitoringEnabled: true,
    configured: true,
  }
}

function renderTable(rows: PerformanceModelRow[], selectedModel?: string) {
  const select = vi.fn()
  render(
    <PerformanceModelTable
      rows={rows}
      columns={[...columns]}
      selectedModel={selectedModel}
      onSelectModel={select}
      locale='zhTW'
    />
  )
  return select
}

describe('performance model table', () => {
  it('distinguishes a scope with no channel traffic from a search without matches', async () => {
    renderTable([])
    expect(screen.getByText('No channel models or request data')).toBeVisible()
    expect(
      screen.getByText(
        'Configure a channel or show all preset models to review model ownership.'
      )
    ).toBeVisible()

    await userEvent
      .setup()
      .type(
        screen.getByRole('textbox', { name: 'Search models' }),
        'missing-model'
      )
    expect(screen.getByText('No models found')).toBeVisible()
  })

  it('paginates large catalogs by requests and filters across hidden group metadata', async () => {
    const rows = Array.from({ length: 22 }, (_, index) =>
      model(`model-${index}`, index + 100)
    )
    rows[0].groupName = 'Special group'
    renderTable(rows)
    const user = userEvent.setup()

    expect(screen.getAllByRole('row')).toHaveLength(21)
    expect(
      within(screen.getAllByRole('row')[1]).getByRole('button', {
        name: 'model-21',
      })
    ).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'model-0' })
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Go to next page' }))
    expect(screen.getByRole('button', { name: 'model-0' })).toBeVisible()
    await user.type(
      screen.getByPlaceholderText('Search models'),
      'Special group'
    )
    expect(await screen.findByRole('button', { name: 'model-0' })).toBeVisible()
    expect(screen.getAllByRole('row')).toHaveLength(2)
    expect(
      screen.getByRole('button', { name: 'Go to next page' })
    ).toBeDisabled()
    expect(
      screen.getByRole('button', { name: 'Go to previous page' })
    ).toBeDisabled()
  })

  it('selects a model with Enter and Space and copies its exact identifier', async () => {
    const select = renderTable(
      [model('custom/model-alias', 150)],
      'custom/model-alias'
    )
    const user = userEvent.setup()
    const button = screen.getByRole('button', { name: 'custom/model-alias' })

    expect(button).toHaveAttribute('aria-pressed', 'true')
    button.focus()
    await user.keyboard('{Enter} ')
    expect(select).toHaveBeenNthCalledWith(1, 'custom/model-alias')
    expect(select).toHaveBeenNthCalledWith(2, 'custom/model-alias')
    await user.click(screen.getByRole('button', { name: 'Copy to clipboard' }))
    expect(await navigator.clipboard.readText()).toBe('custom/model-alias')
  })

  it('keeps unavailable metrics unknown and labels configured models without requests', () => {
    const empty = model('waiting-for-traffic', 0)
    empty.successRate = Number.NaN
    empty.p95LatencyMs = Number.NaN
    empty.incidents = Number.NaN
    renderTable([empty])

    const row = screen.getAllByRole('row')[1]
    expect(within(row).getByText('No requests')).toBeVisible()
    expect(within(row).getAllByText('—')).toHaveLength(3)
    expect(within(row).queryByText('0.00%')).not.toBeInTheDocument()
    expect(screen.getAllByRole('columnheader')).toHaveLength(7)
  })

  it('sorts numeric metrics through the shared column header', async () => {
    renderTable([model('high', 900), model('low', 120)])
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Requests' }))
    await user.click(await screen.findByRole('menuitem', { name: 'Asc' }))

    expect(
      within(screen.getAllByRole('row')[1]).getByRole('button', { name: 'low' })
    ).toBeVisible()
  })
})
