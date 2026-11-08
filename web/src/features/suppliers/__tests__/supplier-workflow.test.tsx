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

import type { NavGroup } from '@/components/layout/types'
import { api } from '@/lib/api'
import { formatFixedUSD } from '@/lib/currency'
import {
  filterRoleNavigation,
  canReadChannels,
  isSupplierRoute,
} from '@/lib/supplier-access'
import { useAuthStore } from '@/stores/auth-store'
import {
  DEFAULT_CURRENCY_CONFIG,
  useSystemConfigStore,
} from '@/stores/system-config-store'

import { saveSupplier, saveSupplyChannel, copySupplyChannel } from '../api'
import { ChannelLimitsDialog } from '../components/channel-limits-dialog'
import { SupplyChannelList } from '../components/channel-list'
import { SupplyLogs } from '../components/logs'
import { SupplyOverviewPanel } from '../components/overview'
import { SupplyFilters } from '../components/supply-filters'
import { SupplierWorkspace } from '../index'
import { supplierAccountSchema, supplierChannelSchema } from '../lib/forms'
import {
  channelLimitsSchema,
  limitsFormValues,
  limitsPayload,
} from '../lib/limits'
import type { ChannelLimits, SupplierAccount } from '../types'

let client: QueryClient
beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  useAuthStore
    .getState()
    .auth.setUser({ id: 7, role: 1, username: 'viewer', channel_viewer: true })
})
afterEach(() => {
  client.clear()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset('idle')
})
const limits: ChannelLimits = {
  config: {
    channel_id: 9,
    revision: 4,
    rpm: null,
    tpm: 600,
    daily_usd: '10.000000001',
    reconcile_seconds: 300,
    budget_ready: true,
  },
  usage: {
    rpm_used: 12,
    tpm_used: 200,
    tpm_reserved: 30,
    daily_used_usd: '5.1',
    daily_reserved_usd: '1.2',
    daily_remaining_usd: '3.700000001',
    budget_day: '2026-11-08',
    reset_at: 1794153600,
    reason: '',
  },
}
const channel = {
  id: 9,
  name: 'Supplier A channel',
  supplier_id: 'supplier-a',
  supplier_name: 'Supplier A',
  type: 1,
  base_url: 'https://api.example.com',
  models: ['model-a'],
  remark: '',
  priority: 1,
  version: 2,
  paused: false,
  status: 'available',
  validation_status: 'validated',
  has_key: true,
}
function withClient(element: React.ReactNode) {
  return render(
    <QueryClientProvider client={client}>{element}</QueryClientProvider>
  )
}

describe('supplier limits contract', () => {
  it('keeps independent unlimited limits and decimal amounts without resetting counters', () => {
    expect(
      limitsPayload({ rpm: '', tpm: '600', daily_usd: '10.000000001' }, 4)
    ).toEqual({ revision: 4, rpm: null, tpm: 600, daily_usd: '10.000000001' })
    expect(limitsPayload({ rpm: '8', tpm: ' ', daily_usd: '' }, 4)).toEqual({
      revision: 4,
      rpm: 8,
      tpm: null,
      daily_usd: null,
    })
    expect(limitsFormValues(limits.config)).toEqual({
      rpm: '',
      tpm: '600',
      daily_usd: '10.000000001',
    })
    for (const invalid of ['0', '-1', '1.5', '9007199254740992']) {
      expect(
        channelLimitsSchema.safeParse({ rpm: invalid, tpm: '', daily_usd: '' })
          .success
      ).toBe(false)
    }
    expect(
      channelLimitsSchema.safeParse({ rpm: '', tpm: '', daily_usd: '0' })
        .success
    ).toBe(false)
  })
  it('renders a viewer limit dialog with no save action and safe endpoint only', async () => {
    const get = vi
      .spyOn(api, 'get')
      .mockResolvedValue({ data: { success: true, data: limits } })
    const put = vi.spyOn(api, 'put')
    withClient(
      <ChannelLimitsDialog
        channelId={9}
        channelName='Channel A'
        audience='viewer'
        onClose={vi.fn()}
      />
    )
    expect(await screen.findByLabelText('RPM')).toBeDisabled()
    expect(screen.getByLabelText('TPM')).toHaveValue('600')
    expect(
      screen.queryByLabelText('TPM reconciliation window (seconds)')
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Save' })
    ).not.toBeInTheDocument()
    expect(get).toHaveBeenCalledWith(
      '/api/channel/read-only/9/limits',
      expect.anything()
    )
    expect(put).not.toHaveBeenCalled()
  })
  it('submits supplier edits with the revision and nullable limits only', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: limits },
    })
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true, data: limits } })
    const user = userEvent.setup()
    withClient(
      <ChannelLimitsDialog
        channelId={9}
        channelName='Channel A'
        audience='supplier'
        onClose={vi.fn()}
      />
    )
    await user.clear(await screen.findByLabelText('TPM'))
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith('/api/supplier/channels/9/limits', {
        revision: 4,
        rpm: null,
        tpm: null,
        daily_usd: '10.000000001',
      })
    )
  })
})
it('viewer channels never expose write or key actions, and supplier channels use their own endpoint', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { items: [channel], total: 1 } },
  })
  const view = withClient(<SupplyChannelList mode='viewer' />)
  expect(
    (await screen.findAllByText('Supplier A channel')).length
  ).toBeGreaterThan(0)
  for (const action of ['Edit', 'Pause', 'Test', 'Add Channel']) {
    expect(
      screen.queryByRole('button', { name: action })
    ).not.toBeInTheDocument()
  }
  expect(get).toHaveBeenCalledWith('/api/channel/read-only/', expect.anything())
  view.unmount()
  withClient(<SupplyChannelList mode='supplier' templates={[]} />)
  expect(
    (await screen.findAllByRole('button', { name: 'Edit' })).length
  ).toBeGreaterThan(0)
  expect(get).toHaveBeenCalledWith('/api/supplier/channels/', expect.anything())
})
it('preserves ordinary user navigation for viewers while isolating supplier navigation', () => {
  const groups: NavGroup[] = [
    {
      id: 'console',
      title: 'Console',
      items: [{ title: 'Keys', url: '/keys' }],
    },
    {
      id: 'personal',
      title: 'Personal',
      items: [
        { title: 'Profile', url: '/profile' },
        { title: 'Wallet', url: '/wallet' },
      ],
    },
    {
      id: 'supply',
      title: 'Supply',
      items: [{ title: 'Supply', url: '/supplier' }],
    },
    {
      id: 'channel-viewer',
      title: 'Channels',
      items: [{ title: 'Channels', url: '/channels' }],
    },
    { id: 'admin', title: 'Admin', items: [{ title: 'Users', url: '/users' }] },
  ]
  const viewer = { id: 1, username: 'viewer', role: 1, channel_viewer: true }
  expect(filterRoleNavigation(groups, viewer).map((group) => group.id)).toEqual(
    ['console', 'personal', 'channel-viewer']
  )
  expect(
    filterRoleNavigation(groups, { ...viewer, role: 2 }).map((group) =>
      group.items.map((item) => 'url' in item && item.url)
    )
  ).toEqual([['/profile'], ['/supplier']])
  expect(canReadChannels(viewer)).toBe(true)
  expect(canReadChannels({ ...viewer, role: 2 })).toBe(false)
  expect(isSupplierRoute('/keys')).toBe(false)
  expect(isSupplierRoute('/supplier')).toBe(true)
})
it('serializes only authorized supplier settings fields', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: {} } })
  const supplier: SupplierAccount = {
    id: 'id',
    name: 'A',
    user_id: 0,
    username: 'secret',
    revision: 0,
    supply_enabled: true,
    login_enabled: false,
    allowed_types: [1],
    allowed_templates: ['1', 'plugin:video'],
    allowed_models: ['a'],
    group: 'default',
    max_channels: 20,
    cost_ratio: 1,
    pool_priority: 0,
    pool_weight: 1,
    max_output_tokens: 8192,
    max_input_tokens: 16384,
  }
  await saveSupplier(undefined, supplier, 'proof')
  const payload = post.mock.calls[0][1] as Record<string, unknown>
  expect(payload).not.toHaveProperty('id')
  expect(payload).not.toHaveProperty('username')
  expect(payload).not.toHaveProperty('login_enabled')
  expect(payload.allowed_templates).toEqual(['1', 'plugin:video'])
  expect(payload.max_input_tokens).toBe(16384)
  expect(post.mock.calls[0][2]).toMatchObject({
    headers: { 'X-Security-Proof': 'proof' },
    singleUseAuthorization: true,
  })
})
it.each(['en-US', 'zh-CN', 'zh-TW', 'fr-FR', 'ja-JP', 'ru-RU', 'vi-VN'])(
  'formats fixed USD safely in %s',
  (locale) => {
    expect(
      formatFixedUSD('10.000000001', {
        locale,
        digitsLarge: 9,
        abbreviate: false,
      })
    ).toContain('10')
    expect(formatFixedUSD(null, { locale })).toBe('-')
  }
)

it('keeps procurement amounts in USD when consumer currency is CNY or tokens', () => {
  const previous = useSystemConfigStore.getState().config.currency
  try {
    for (const quotaDisplayType of ['CNY', 'TOKENS'] as const) {
      useSystemConfigStore.getState().setConfig({
        currency: {
          ...DEFAULT_CURRENCY_CONFIG,
          quotaDisplayType,
          usdExchangeRate: 7,
        },
      })
      expect(formatFixedUSD('10', { locale: 'en-US' })).toBe('$10')
    }
  } finally {
    useSystemConfigStore.getState().setConfig({ currency: previous })
  }
})

it('removes upstream credentials and proof headers from cached request errors', async () => {
  const secret = 'only-in-request-body'
  const error = Object.assign(new Error('Request failed'), {
    isAxiosError: true,
    config: { data: { key: secret }, headers: { 'X-Security-Proof': 'proof' } },
    response: {
      status: 400,
      data: { success: false, message: 'Invalid parameters' },
    },
  })
  vi.spyOn(api, 'post').mockRejectedValue(error)
  const result = await saveSupplyChannel(
    undefined,
    {
      name: 'A',
      template_id: '1',
      base_url: 'https://example.com',
      key: secret,
      models: ['a'],
      remark: '',
      priority: 0,
    },
    'proof'
  ).catch((failure: unknown) => failure)
  expect(result).toBeInstanceOf(Error)
  expect(result).not.toHaveProperty('cause')
  expect(result).not.toHaveProperty('config')
  expect(JSON.stringify(result)).not.toContain(secret)
})

it('keeps budget events unavailable to viewers and loads supplier-only events on demand', async () => {
  const get = vi.spyOn(api, 'get').mockImplementation(async (path) => ({
    data: {
      success: true,
      data: String(path).endsWith('budget-events')
        ? { items: [], total: 0 }
        : limits,
    },
  }))
  const user = userEvent.setup()
  const viewer = withClient(
    <ChannelLimitsDialog
      channelId={9}
      channelName='Channel A'
      audience='viewer'
      onClose={vi.fn()}
    />
  )
  await screen.findByLabelText('RPM')
  expect(
    screen.queryByRole('button', { name: 'Budget details' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Limit history' })
  ).not.toBeInTheDocument()
  viewer.unmount()
  withClient(
    <ChannelLimitsDialog
      channelId={9}
      channelName='Channel A'
      audience='supplier'
      onClose={vi.fn()}
    />
  )
  await user.click(
    await screen.findByRole('button', { name: 'Budget details' })
  )
  expect(await screen.findByText('No budget events')).toBeVisible()
  expect(get).toHaveBeenCalledWith(
    '/api/supplier/channels/9/budget-events',
    expect.objectContaining({
      params: { p: 1, page_size: 10, day: '2026-11-08' },
    })
  )
})
it('rejects an incomplete custom reporting range instead of silently querying all history', async () => {
  const onChange = vi.fn()
  const user = userEvent.setup()
  render(<SupplyFilters onChange={onChange} />)
  await user.click(screen.getByRole('combobox', { name: 'Time range' }))
  await user.click(screen.getByRole('option', { name: 'Custom Time Range' }))
  await user.click(screen.getByRole('button', { name: 'Filter' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Select a valid time range of up to 90 days.'
  )
  expect(onChange).not.toHaveBeenCalled()
})

it('creates a supplier channel for an administrator through its explicit owner route', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: channel } })
  const payload = {
    name: 'A',
    template_id: '1',
    base_url: 'https://example.com',
    key: 'write-once',
    models: ['a'],
    remark: '',
    priority: 0,
    limits: { rpm: null, tpm: 50, daily_usd: '10' },
  }
  await saveSupplyChannel(undefined, payload, 'proof', 'admin', 'supplier-a')
  expect(post).toHaveBeenCalledWith(
    '/api/suppliers/owners/supplier-a/channels',
    payload,
    expect.objectContaining({ singleUseAuthorization: true })
  )
})
it('copies a supplier channel without fetching or resubmitting its upstream key', async () => {
  const get = vi.spyOn(api, 'get')
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: { id: 10 } } })
  await copySupplyChannel(channel, 'Copy A', 'proof')
  expect(get).not.toHaveBeenCalled()
  expect(post).toHaveBeenCalledWith(
    '/api/supplier/channels/9/copy',
    { name: 'Copy A', version: 2 },
    expect.objectContaining({
      headers: { 'X-Security-Proof': 'proof' },
      singleUseAuthorization: true,
    })
  )
})

it('keeps the metrics compact on narrow screens and shows unknown token coverage explicitly', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        channel_count: 1,
        available_channels: 1,
        request_count: 1,
        success_count: 1,
        failure_count: 0,
        unknown_count: 0,
        input_tokens: 0,
        output_tokens: 0,
        usage_known_count: 0,
        avg_latency_ms: 100,
        daily_used_usd: null,
        budget_day: '2026-11-08',
        updated_at: 0,
        channels: [],
        coverage: {
          timezone: 'UTC',
          collected_since: null,
          limitations: ['image_video_quantities', 'async_completion'],
        },
      },
    },
  })
  withClient(<SupplyOverviewPanel mode='supplier' />)
  const metrics = await screen.findByRole('list', { name: 'Overview' })
  expect(metrics).toHaveClass('grid-cols-2')
  expect(screen.getByText('Input Tokens').parentElement).toHaveTextContent(
    'Unknown'
  )
  expect(screen.getByRole('status')).toHaveTextContent(
    'Asynchronous completion is not collected.'
  )
})

it.each([
  { status: 'available', validation_status: 'failed', expected: 'Available' },
  {
    status: 'cost_config_missing',
    validation_status: 'pending',
    expected: 'Waiting for cost configuration',
  },
  {
    status: 'daily_exhausted',
    validation_status: 'validated',
    expected: 'Daily budget exhausted',
  },
])(
  'shows the effective supply restriction independently of candidate validation: $status',
  async (state) => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: { items: [{ ...channel, ...state }], total: 1 },
      },
    })
    withClient(<SupplyChannelList mode='supplier' templates={[]} />)
    expect((await screen.findAllByText(state.expected)).length).toBeGreaterThan(
      0
    )
  }
)

it('keeps administrator limit settings read-only without channel write permission', async () => {
  useAuthStore.getState().auth.setUser({
    id: 7,
    role: 10,
    username: 'reader',
    permissions: {
      admin_permissions: { channel: { read: true, write: false } },
    },
  })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: limits },
  })
  withClient(
    <ChannelLimitsDialog
      channelId={9}
      channelName='Channel A'
      audience='admin'
      onClose={vi.fn()}
    />
  )
  expect(await screen.findByLabelText('RPM')).toBeDisabled()
  expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument()
})

it('discards an unused draft only after confirmation using its current version', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            ...channel,
            status: 'pending',
            validation_status: 'failed',
            has_validated_config: false,
          },
        ],
        total: 1,
      },
    },
  })
  const remove = vi
    .spyOn(api, 'delete')
    .mockResolvedValue({ data: { success: true } })
  const user = userEvent.setup()
  withClient(<SupplyChannelList mode='supplier' templates={[]} />)
  await user.click(
    (await screen.findAllByRole('button', { name: 'Discard draft' }))[0]
  )
  expect(remove).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Discard' }))
  await waitFor(() =>
    expect(remove).toHaveBeenCalledWith('/api/supplier/channels/9', {
      data: { version: 2 },
    })
  )
})

it('keeps validated supply copyable while a replacement fails and does not offer draft deletion', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            ...channel,
            validation_status: 'failed',
            has_validated_config: true,
          },
        ],
        total: 1,
      },
    },
  })
  withClient(<SupplyChannelList mode='supplier' templates={[]} />)
  for (const copy of await screen.findAllByRole('button', {
    name: 'Copy Channel',
  })) {
    expect(copy).toBeEnabled()
  }
  expect(
    screen.queryByRole('button', { name: 'Discard draft' })
  ).not.toBeInTheDocument()
})

it('accepts optional output ceilings and zero procurement cost while enforcing API form bounds', () => {
  const account = {
    name: 'A',
    user_id: '',
    allowed_types: ['1'],
    allowed_models: ['a'],
    group: 'default',
    max_channels: '20',
    cost_ratio: '0',
    pool_priority: '0',
    pool_capacity: '0',
    pool_weight: '1',
    max_output_tokens: '',
    supply_enabled: true,
  }
  expect(supplierAccountSchema.safeParse(account).success).toBe(true)
  expect(
    supplierAccountSchema.safeParse({ ...account, cost_ratio: '101' }).success
  ).toBe(false)
  const values = {
    ...channel,
    template_id: '1',
    key: '',
    priority: '1000',
    rpm: '',
    tpm: '',
    daily_usd: '',
  }
  expect(supplierChannelSchema.safeParse(values).success).toBe(true)
  for (const invalid of [
    { priority: '1001' },
    { remark: 'x'.repeat(256) },
    { base_url: 'https://example.com/?key=secret' },
    { base_url: 'https://user:password@example.com' },
  ]) {
    expect(
      supplierChannelSchema.safeParse({ ...values, ...invalid }).success
    ).toBe(false)
  }
})

it('shows independent supply restrictions together so midnight recovery cannot imply availability', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            ...channel,
            status: 'daily_exhausted',
            blocking_reasons: [
              'manual_paused',
              'authorization_removed',
              'daily_exhausted',
            ],
          },
        ],
        total: 1,
      },
    },
  })
  withClient(<SupplyChannelList mode='supplier' templates={[]} />)
  for (const label of [
    'Paused by an administrator',
    'Channel authorization withdrawn',
    'Daily budget exhausted',
  ]) {
    expect((await screen.findAllByText(label)).length).toBeGreaterThan(0)
  }
})

it('allows only administrators to set a finite TPM reconciliation window', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: limits },
  })
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true, data: limits } })
  const user = userEvent.setup()
  withClient(
    <ChannelLimitsDialog
      channelId={9}
      channelName='A'
      audience='admin'
      onClose={vi.fn()}
    />
  )
  const duration = await screen.findByLabelText(
    'TPM reconciliation window (seconds)'
  )
  await user.clear(duration)
  await user.type(duration, '0')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(
    await screen.findByText('Enter a whole number from 1 to 86400')
  ).toBeVisible()
  expect(put).not.toHaveBeenCalled()
  await user.clear(duration)
  await user.type(duration, '600')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/channel/9/limits', {
      revision: 4,
      rpm: null,
      tpm: 600,
      daily_usd: '10.000000001',
      reconcile_seconds: 600,
    })
  )
})

it('opens supplier operation history through the existing actor-scoped audit endpoint', async () => {
  useAuthStore.getState().auth.setUser({
    id: 7,
    username: 'supplier',
    role: 2,
    supplier_id: 'supplier-a',
  })
  const get = vi.spyOn(api, 'get').mockImplementation(async (path) => ({
    data: {
      success: true,
      data:
        path === '/api/supplier/self'
          ? {
              supplier: {
                id: 'supplier-a',
                name: 'Supplier A',
                supply_enabled: true,
              },
              templates: [],
            }
          : { items: [], total: 0 },
    },
  }))
  const user = userEvent.setup()
  withClient(<SupplierWorkspace />)
  await user.click(
    await screen.findByRole('tab', { name: 'Operation history' })
  )
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith('/api/audit/self', {
      params: { p: 1, page_size: 20 },
    })
  )
  expect(get).not.toHaveBeenCalledWith('/api/audit', expect.anything())
})

it('shows recorded media quantities and their sources without converting unknown usage into zero', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            id: 'own-reference',
            channel_id: 9,
            channel_name: 'A',
            created_at: 1794120000,
            model: 'image-a',
            stage: 'image',
            result: 'success',
            status_code: 200,
            error_category: 'none',
            duration_ms: 20,
            input_tokens: null,
            output_tokens: null,
            usage_source: 'upstream',
            correlation_id: 'own-reference',
            upstream_request_id: 'vendor-request-123',
            image_count: 2,
            image_usage_source: 'upstream_payload',
            audio_seconds: null,
            audio_usage_source: '',
          },
        ],
        total: 1,
      },
    },
  })
  const user = userEvent.setup()
  withClient(<SupplyLogs />)
  expect(
    (await screen.findAllByText('Upstream response')).length
  ).toBeGreaterThan(0)
  expect(screen.getAllByText('Actual').length).toBeGreaterThan(0)
  expect(screen.getAllByText('Unknown').length).toBeGreaterThan(0)
  expect(
    screen.getByRole('columnheader', { name: 'Error category' })
  ).toBeVisible()
  await user.click(
    (
      await screen.findAllByRole('button', { name: 'Copy Upstream request ID' })
    )[0]
  )
  expect(await navigator.clipboard.readText()).toBe('vendor-request-123')
})

it('displays supplier-scoped logical counts, recorded media coverage, and HTTP error totals', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        channel_count: 1,
        available_channels: 1,
        request_count: 3,
        logical_request_count: 2,
        success_count: 2,
        failure_count: 1,
        unknown_count: 0,
        input_tokens: 10,
        output_tokens: 4,
        avg_latency_ms: 20,
        daily_used_usd: '1',
        budget_day: '2026-11-08',
        updated_at: 1794120000,
        last_called_at: 1794120000,
        channels: [],
        image_count: 4,
        image_usage_known_count: 2,
        audio_seconds: null,
        audio_usage_known_count: 0,
        timeout_count: 0,
        rate_limited_count: 1,
        upstream_error_count: 0,
        request_error_count: 0,
        auth_error_count: 0,
        unknown_error_count: 0,
        coverage: {
          timezone: 'UTC',
          collected_since: null,
          limitations: ['video_quantities', 'async_completion'],
        },
      },
    },
  })
  withClient(<SupplyOverviewPanel mode='supplier' />)
  const metrics = within(await screen.findByRole('list', { name: 'Overview' }))
  expect(
    metrics.getByText('Logical requests reached').parentElement
  ).toHaveTextContent('2')
  expect(metrics.getByText('Recorded images').parentElement).toHaveTextContent(
    '4'
  )
  expect(metrics.getByText('Recorded images').parentElement).toHaveTextContent(
    'Recorded samples: 2'
  )
  expect(
    metrics.getByText('Audio duration (seconds)').parentElement
  ).toHaveTextContent('Unknown')
  expect(
    screen
      .getByText('Rate limited (HTTP 429)')
      .parentElement?.querySelector('dd')
  ).toHaveTextContent(/^1$/)
  expect(
    metrics.getByText('Last upstream call').parentElement
  ).not.toHaveTextContent('No traffic yet')
  expect(screen.getByRole('status')).toHaveTextContent(
    'Video quantities are not collected.'
  )
})

it.each([
  { validation_status: 'waiting', label: 'Waiting for task completion' },
  { validation_status: 'result_unknown', label: 'Validation result unknown' },
])(
  'shows $validation_status without marking an asynchronous probe as passed',
  async (state) => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          items: [
            {
              ...channel,
              validation_status: state.validation_status,
              has_validated_config: false,
            },
          ],
          total: 1,
        },
      },
    })
    withClient(<SupplyChannelList mode='supplier' templates={[]} />)
    expect((await screen.findAllByText(state.label)).length).toBeGreaterThan(0)
    expect(
      screen.queryByRole('button', { name: 'Discard draft' })
    ).not.toBeInTheDocument()
    if (state.validation_status === 'waiting') {
      for (const button of screen.getAllByRole('button', { name: 'Test' })) {
        expect(button).toBeDisabled()
      }
    }
  }
)
