/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { MonitoringSettingsSection } from '../monitoring-settings-section'

function MonitoringFixture(props: { enabled?: boolean }) {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  return (
    <>
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        <MonitoringSettingsSection
          defaultValues={{
            'perf_metrics_setting.enabled': props.enabled ?? true,
            'perf_metrics_setting.flush_interval': 17,
            'perf_metrics_setting.bucket_time': 'hour',
            'perf_metrics_setting.retention_days': 0,
          }}
        />
      </SettingsPageProvider>
    </>
  )
}

afterEach(() => vi.restoreAllMocks())

describe('performance collection maintenance settings', () => {
  it('keeps the saved interval and its storage key while distinguishing maintenance from aggregation', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const client = new QueryClient({
      defaultOptions: { mutations: { retry: false } },
    })
    render(
      <QueryClientProvider client={client}>
        <MonitoringFixture />
      </QueryClientProvider>
    )
    const interval = screen.getByRole('spinbutton', {
      name: 'Maintenance interval (minutes)',
    })
    expect(interval).toHaveValue(17)
    expect(
      screen.getByText(
        'Terminal samples are stored before aggregation. Metrics are aggregated every 5 seconds.'
      )
    ).toBeVisible()
    expect(
      screen.queryByText('Flush interval (minutes)')
    ).not.toBeInTheDocument()
    const user = userEvent.setup()
    fireEvent.change(interval, { target: { value: '19' } })
    await user.click(screen.getByRole('button', { name: 'Save Changes' }))

    await waitFor(() =>
      expect(put).toHaveBeenCalledWith('/api/option/', {
        key: 'perf_metrics_setting.flush_interval',
        value: 19,
      })
    )
    expect(put).toHaveBeenCalledTimes(1)
    client.clear()
  })

  it('keeps retention maintenance editable when new sample collection is disabled', () => {
    const client = new QueryClient()
    render(
      <QueryClientProvider client={client}>
        <MonitoringFixture enabled={false} />
      </QueryClientProvider>
    )
    expect(
      screen.getByRole('spinbutton', { name: 'Maintenance interval (minutes)' })
    ).toBeEnabled()
    expect(
      screen.getByRole('spinbutton', { name: 'Retention days' })
    ).toBeEnabled()
    expect(
      screen.getByRole('switch', { name: 'Enable model performance metrics' })
    ).not.toBeChecked()
    client.clear()
  })
})
