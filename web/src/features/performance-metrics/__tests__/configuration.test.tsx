/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosError } from 'axios'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { savePerformanceHealthConfig } from '../api'
import { PerformanceConfigDialog } from '../components/performance-config-dialog'
import type { PerformanceHealthConfig } from '../types'

const config: PerformanceHealthConfig = {
  version: 4,
  preset_version: 'presets-v1',
  excluded_models: ['excluded-model'],
  excluded_companies: ['excluded-company'],
  excluded_groups: ['excluded-group'],
  companies: [
    {
      id: 'legacy-openai',
      name: 'Openai',
      status: 'active',
      slo: 97,
      owner: 'Manual owner',
      note: 'Manual note',
      groups: [
        {
          id: 'legacy-main',
          name: 'General',
          preset_key: 'openai-default',
          models: ['manual-alias'],
          enabled: true,
          slo: 97,
          min_samples: 10,
        },
      ],
    },
    {
      id: 'custom',
      name: 'Custom',
      status: 'active',
      slo: 95,
      groups: [
        {
          id: 'custom-other',
          name: 'Backup',
          models: [],
          enabled: true,
          slo: 95,
          min_samples: 100,
        },
      ],
    },
  ],
}
const presets: PerformanceHealthConfig = {
  version: 1,
  companies: [
    {
      id: 'moonshot',
      name: 'Kimi',
      groups: [
        { id: 'moonshot-default', name: 'Kimi models', models: ['kimi-k3'] },
      ],
    },
    {
      id: 'openai',
      name: 'Openai',
      groups: [
        {
          id: 'openai-default',
          name: 'Openai models',
          models: ['gpt-6-astra'],
        },
      ],
    },
    {
      id: 'anthropic',
      name: 'Claude',
      groups: [
        {
          id: 'anthropic-default',
          name: 'Claude models',
          models: ['claude-opus-4-8'],
        },
      ],
    },
  ],
}
let client: QueryClient | undefined
afterEach(() => client?.clear())

function mockConfigurationGet() {
  return vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: structuredClone(
        url === '/api/performance-health/companies' ? config : presets
      ),
    },
  }))
}

function renderConfiguration() {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const close = vi.fn()
  const saved = vi.fn()
  render(
    <QueryClientProvider client={client}>
      <PerformanceConfigDialog
        open
        onOpenChange={close}
        onSaved={saved}
        observedModels={['live-alias']}
        channelModels={['configured-no-traffic']}
      />
    </QueryClientProvider>
  )
  return { close, saved, client }
}

describe('performance configuration API', () => {
  it('rejects a failed business response instead of reporting configuration saved', async () => {
    vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: false, message: 'Configuration could not be saved' },
    })

    await expect(
      savePerformanceHealthConfig({ version: 4, companies: [] })
    ).rejects.toThrow('Configuration could not be saved')
  })
})

describe('performance configuration workflow', () => {
  it('keeps legacy zero SLO targets disabled, clears confirmation with empty inputs and allows explicitly confirmed zero', async () => {
    const legacy = structuredClone(config)
    legacy.companies[0].slo = 0
    const customGroup = legacy.companies[1].groups?.[0]
    if (customGroup) customGroup.slo = 0
    vi.spyOn(api, 'get').mockImplementation(async (url) => ({
      data: {
        success: true,
        data: url === '/api/performance-health/companies' ? legacy : presets,
      },
    }))
    const put = vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: true, data: { ...legacy, version: 5 } },
    })
    renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })
    const companyTarget = within(company).getByRole('checkbox', {
      name: 'Enable company SLO target',
    })
    expect(companyTarget).not.toBeChecked()
    expect(
      within(screen.getByRole('region', { name: 'Custom' })).getByRole(
        'checkbox',
        { name: 'Enable group SLO target' }
      )
    ).not.toBeChecked()

    await user.clear(
      within(company).getByRole('spinbutton', { name: 'Group SLO' })
    )
    expect(
      within(company).getByRole('checkbox', { name: 'Enable group SLO target' })
    ).not.toBeChecked()
    await user.click(companyTarget)
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(put).toHaveBeenCalled())
    const body = put.mock.calls[0][1] as PerformanceHealthConfig
    expect(body.companies[0]).toMatchObject({ slo: 0, slo_confirmed: true })
    expect(body.companies[0].groups?.[0].slo).toBeUndefined()
    expect(body.companies[0].groups?.[0].slo_confirmed).toBe(false)
  })

  it('labels channel candidates without requests separately from observed traffic', async () => {
    mockConfigurationGet()
    renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })
    const models = within(company).getByRole('combobox', { name: 'Models' })
    await user.type(models, 'configured-no-traffic')

    expect(await screen.findByText('Channel models')).toBeVisible()
    expect(screen.queryByText('Observed model')).not.toBeInTheDocument()
  })

  it('saves legacy null groups and model arrays after editing a company', async () => {
    const legacy = structuredClone(config)
    Object.assign(legacy.companies[1], { groups: null })
    Object.assign(legacy.companies[0].groups?.[0] ?? {}, { models: null })
    vi.spyOn(api, 'get').mockImplementation(async (url) => ({
      data: {
        success: true,
        data: url === '/api/performance-health/companies' ? legacy : presets,
      },
    }))
    const put = vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: true, data: { ...legacy, version: 5 } },
    })
    renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })
    await user.type(
      within(company).getByRole('textbox', { name: 'Company name' }),
      ' updated'
    )
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(put).toHaveBeenCalled())
    const body = put.mock.calls[0][1] as PerformanceHealthConfig
    expect(body.companies[0].groups?.[0].models).toEqual([])
    expect(body.companies[1].groups).toEqual([])
    expect(body.companies[0].slo).toBe(97)
  })

  it('confirms before disabling company and group monitoring', async () => {
    mockConfigurationGet()
    const put = vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: true, data: { ...config, version: 5 } },
    })
    renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })
    const groupEnabled = within(company).getByRole('checkbox', {
      name: 'Monitoring enabled',
    })
    const companyEnabled = within(company).getByRole('checkbox', {
      name: 'Company active',
    })

    await user.click(groupEnabled)
    expect(
      await screen.findByText('Disable model group monitoring?')
    ).toBeVisible()
    expect(groupEnabled).toBeChecked()
    await user.click(
      within(screen.getByRole('alertdialog')).getByRole('button', {
        name: 'Cancel',
      })
    )
    expect(groupEnabled).toBeChecked()
    await user.click(groupEnabled)
    await user.click(
      within(screen.getByRole('alertdialog')).getByRole('button', {
        name: 'Continue',
      })
    )
    expect(groupEnabled).not.toBeChecked()
    await user.click(companyEnabled)
    expect(await screen.findByText('Disable company monitoring?')).toBeVisible()
    expect(companyEnabled).toBeChecked()
    await user.click(
      within(screen.getByRole('alertdialog')).getByRole('button', {
        name: 'Continue',
      })
    )
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(put).toHaveBeenCalled())
    const body = put.mock.calls[0][1] as PerformanceHealthConfig
    expect(body.companies[0].status).toBe('disabled')
    expect(body.companies[0].groups?.[0].enabled).toBe(false)
    expect(body.companies[0].groups?.[0].models).toEqual(['manual-alias'])
  })

  it('keeps loading failures separate from empty configuration and prevents saving', async () => {
    vi.spyOn(api, 'get').mockImplementation(async (url) => {
      if (url === '/api/performance-health/companies') {
        throw new Error('Configuration unavailable')
      }
      return { data: { success: true, data: presets } }
    })
    const put = vi.spyOn(api, 'put')

    renderConfiguration()

    expect(await screen.findByText('Configuration unavailable')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Save changes' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Add company' })).toBeDisabled()
    expect(
      screen.queryByText('No company configuration available')
    ).not.toBeInTheDocument()
    expect(put).not.toHaveBeenCalled()
  })

  it('adds typed model IDs and saves the loaded version and all manual metadata', async () => {
    mockConfigurationGet()
    const put = vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: true, data: { ...config, version: 5 } },
    })
    const result = renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })
    const models = within(company).getByRole('combobox', { name: 'Models' })

    await user.type(models, 'extra-a,extra-b,')
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(put).toHaveBeenCalled())
    expect(put.mock.calls[0][0]).toBe('/api/performance-health/config')
    expect(put.mock.calls[0][1]).toMatchObject({
      version: 4,
      expected_config_version: 4,
      preset_version: 'presets-v1',
      excluded_models: ['excluded-model'],
      excluded_companies: ['excluded-company'],
      excluded_groups: ['excluded-group'],
      companies: [
        {
          id: 'legacy-openai',
          name: 'Openai',
          slo: 97,
          owner: 'Manual owner',
          note: 'Manual note',
          groups: [
            {
              id: 'legacy-main',
              preset_key: 'openai-default',
              models: ['manual-alias', 'extra-a', 'extra-b'],
              min_samples: 10,
            },
          ],
        },
        { id: 'custom', name: 'Custom' },
      ],
    })
    await waitFor(() => expect(result.saved).toHaveBeenCalled())
  })

  it('preserves unsaved edits when a new saved configuration arrives and asks before discarding', async () => {
    mockConfigurationGet()
    const result = renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })
    const name = within(company).getByRole('textbox', { name: 'Company name' })
    await user.clear(name)
    await user.type(name, 'My manual company')

    act(() =>
      result.client.setQueryData(['performance-health-companies'], {
        success: true,
        data: { ...config, version: 5 },
      })
    )

    expect(name).toHaveValue('My manual company')
    expect(
      await screen.findByText(
        'Configuration changed. Reload before saving again.'
      )
    ).toBeVisible()
    expect(screen.getByRole('button', { name: 'Save changes' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(await screen.findByText('Discard unsaved changes?')).toBeVisible()
    expect(result.close).not.toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: 'Continue' }))
    expect(result.close).toHaveBeenCalledWith(false)
  })

  it('keeps the draft and dialog open after a save version conflict', async () => {
    mockConfigurationGet()
    const error = new AxiosError('Conflict', '409', undefined, undefined, {
      data: { success: false, message: 'Configuration changed' },
      status: 409,
      statusText: 'Conflict',
      headers: {},
      config: { headers: {} },
    } as never)
    vi.spyOn(api, 'put').mockRejectedValue(error)
    const result = renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })
    const note = within(company).getByRole('textbox', { name: 'Note' })
    await user.clear(note)
    await user.type(note, 'Keep this edit')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    expect(
      await screen.findByText(
        'Configuration changed. Reload before saving again.'
      )
    ).toBeVisible()
    expect(note).toHaveValue('Keep this edit')
    expect(screen.getByRole('button', { name: 'Save changes' })).toBeDisabled()
    expect(result.close).not.toHaveBeenCalled()
    expect(result.saved).not.toHaveBeenCalled()
  })

  it('requires a fresh preview before applying preset additions and blocks preview with unsaved edits', async () => {
    mockConfigurationGet()
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValueOnce({
        data: {
          success: true,
          data: {
            config,
            expected_config_version: 4,
            preview_token: 'preview-token',
            added_companies: ['Kimi', 'Claude'],
            added_groups: ['moonshot-default'],
            added_models: ['kimi-k3'],
            conflicts: ['manual-alias'],
            skipped_models: ['excluded-model'],
          },
        },
      })
      .mockResolvedValueOnce({
        data: { success: true, data: { ...config, version: 5 } },
      })
    const result = renderConfiguration()
    const user = userEvent.setup()
    await screen.findByRole('region', { name: 'Openai' })
    expect(
      screen.queryByRole('button', { name: 'Apply preset additions' })
    ).not.toBeInTheDocument()

    await user.click(
      screen.getByRole('button', { name: 'Preview preset additions' })
    )
    const preview = await screen.findByRole('region', {
      name: 'Preset additions',
    })
    expect(within(preview).getByText(/Kimi, Claude/)).toBeVisible()
    expect(
      within(preview).getByText(/Manual assignments kept/)
    ).toHaveTextContent('manual-alias')
    await user.click(
      screen.getByRole('button', { name: 'Apply preset additions' })
    )
    await waitFor(() => expect(result.saved).toHaveBeenCalled())
    expect(post.mock.calls[1]).toEqual([
      '/api/performance-health/presets/apply',
      { expected_config_version: 4, preview_token: 'preview-token' },
    ])
  })

  it('inherits an existing company SLO when adding a group and protects preset preview while editing', async () => {
    mockConfigurationGet()
    const put = vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: true, data: { ...config, version: 5 } },
    })
    renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })

    await user.click(
      within(company).getByRole('button', { name: 'Add model group' })
    )

    expect(
      within(company).getAllByRole('spinbutton', { name: 'Group SLO' }).at(-1)
    ).toHaveValue(97)
    expect(
      screen.getByRole('button', { name: 'Preview preset additions' })
    ).toBeDisabled()
    await user.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(put).toHaveBeenCalled())
    const body = put.mock.calls[0][1] as PerformanceHealthConfig
    expect(body.companies[0].groups?.at(-1)).toMatchObject({
      slo: 97,
      min_samples: 100,
      enabled: true,
    })
  })

  it('confirms clearing assignments before deleting a model group', async () => {
    mockConfigurationGet()
    const put = vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: true, data: { ...config, version: 5 } },
    })
    renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })
    const models = within(company).getByRole('combobox', { name: 'Models' })
    // Long translated action labels must wrap inside narrow configuration dialogs.
    expect(
      within(company).getByRole('button', { name: 'Delete model group' })
        .parentElement
    ).toHaveClass('max-w-full', 'flex-wrap')

    await user.click(
      within(company).getByRole('button', { name: 'Delete model group' })
    )

    expect(
      await screen.findByText(
        'This clears 1 model assignments. Performance history is kept.'
      )
    ).toBeVisible()
    expect(models).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Continue' }))
    expect(
      within(company).queryByRole('combobox', { name: 'Models' })
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(put).toHaveBeenCalled())
    expect(
      (put.mock.calls[0][1] as PerformanceHealthConfig).companies[0].groups
    ).toEqual([])
  })

  it('moves selected models to one destination group without creating duplicate ownership', async () => {
    mockConfigurationGet()
    const put = vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: true, data: { ...config, version: 5 } },
    })
    renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })

    await user.click(
      within(company).getByRole('button', { name: 'Move models' })
    )
    await user.selectOptions(
      await screen.findByRole('combobox', { name: 'Destination model group' }),
      'custom-other'
    )
    const confirmation = screen.getByRole('alertdialog')
    await user.click(
      within(confirmation).getByRole('button', { name: 'Move models' })
    )
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(put).toHaveBeenCalled())
    const body = put.mock.calls[0][1] as PerformanceHealthConfig
    expect(body.companies[0].groups?.[0].models).toEqual([])
    expect(body.companies[1].groups?.[0].models).toEqual(['manual-alias'])
  })

  it('shows a model ownership validation error without submitting a duplicate assignment', async () => {
    mockConfigurationGet()
    const put = vi.spyOn(api, 'put')
    renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Custom' })
    await user.type(
      within(company).getByRole('combobox', { name: 'Models' }),
      'manual-alias,'
    )
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    expect(
      await screen.findByText('A model can belong to only one model group')
    ).toBeVisible()
    expect(put).not.toHaveBeenCalled()
  })

  it('keeps edited values open when the server returns a failed business response', async () => {
    mockConfigurationGet()
    vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: false, message: 'Saving was rejected' },
    })
    const result = renderConfiguration()
    const user = userEvent.setup()
    const company = await screen.findByRole('region', { name: 'Openai' })
    const name = within(company).getByRole('textbox', { name: 'Company name' })
    await user.clear(name)
    await user.type(name, 'Keep my company')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    expect(await screen.findByText('Saving was rejected')).toBeVisible()
    expect(name).toHaveValue('Keep my company')
    expect(result.close).not.toHaveBeenCalled()
    expect(result.saved).not.toHaveBeenCalled()
  })

  it('keeps save disabled while the saved configuration has not arrived', async () => {
    let resolveConfig: (value: {
      data: { success: boolean; data: PerformanceHealthConfig }
    }) => void = () => {}
    const response = new Promise<{
      data: { success: boolean; data: PerformanceHealthConfig }
    }>((resolve) => {
      resolveConfig = resolve
    })
    vi.spyOn(api, 'get').mockImplementation(async (url) =>
      url === '/api/performance-health/companies'
        ? response
        : { data: { success: true, data: presets } }
    )
    const put = vi.spyOn(api, 'put')
    renderConfiguration()

    expect(screen.getByRole('button', { name: 'Save changes' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Add company' })).toBeDisabled()
    expect(
      screen.queryByText('No company configuration available')
    ).not.toBeInTheDocument()
    await act(async () =>
      resolveConfig({ data: { success: true, data: config } })
    )
    expect(await screen.findByRole('region', { name: 'Openai' })).toBeVisible()
    expect(put).not.toHaveBeenCalled()
  })
})
