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
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterAll, expect, test, vi } from 'vitest'

import type { UsageLog } from '../../data/schema'
import type { ChannelAffinityInfo } from '../../types'
import { useCommonLogsColumns } from '../columns/common-logs-columns'
import { UsageLogsProvider } from '../usage-logs-provider'

vi.mock('@/lib/lobe-icon', () => ({ getLobeIcon: () => null }))

vi.hoisted(() => {
  vi.stubGlobal('localStorage', {
    getItem: () => null,
    setItem: () => undefined,
    removeItem: () => undefined,
  })
})

afterAll(() => vi.unstubAllGlobals())

function makeLog(affinity: ChannelAffinityInfo): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: 1,
    type: 2,
    content: '',
    username: 'user',
    token_name: 'token',
    model_name: 'gpt-test',
    quota: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    use_time: 0,
    is_stream: false,
    channel: 16,
    channel_name: 'Bailian',
    token_id: 1,
    group: 'default',
    ip: '',
    other: JSON.stringify({ admin_info: { channel_affinity: affinity } }),
    request_id: 'req-1',
    upstream_request_id: '',
  }
}

function ChannelCell(props: { affinity: ChannelAffinityInfo }) {
  const table = useReactTable({
    data: [makeLog(props.affinity)],
    columns: useCommonLogsColumns(true, false),
    getCoreRowModel: getCoreRowModel(),
  })
  const cell = table
    .getRowModel()
    .rows[0].getAllCells()
    .find((item) => item.column.id === 'channel')
  if (!cell) throw new Error('An admin log list must have a channel column')
  return <>{flexRender(cell.column.columnDef.cell, cell.getContext())}</>
}

function renderChannel(affinity: ChannelAffinityInfo) {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <UsageLogsProvider>
        <ChannelCell affinity={affinity} />
      </UsageLogsProvider>
    </QueryClientProvider>
  )
}

test('a request that followed a channel affinity names that affinity by its ID', async () => {
  renderChannel({
    rule_name: 'all-models-trace',
    using_group: 'default',
    key_fp: '3fa9c21b',
  })

  await userEvent.hover(screen.getByText('#16'))

  const id = await screen.findByText('3fa9c21b')
  expect(id.parentElement).toHaveTextContent('Affinity ID: 3fa9c21b')
  expect(screen.getByText('Rule: all-models-trace')).toBeInTheDocument()
  expect(screen.getByText('Group: default')).toBeInTheDocument()
})

test('a log recorded without an affinity ID shows the rule and group only', async () => {
  renderChannel({ rule_name: 'all-models-trace', using_group: 'default' })

  await userEvent.hover(screen.getByText('#16'))

  expect(await screen.findByText('Rule: all-models-trace')).toBeInTheDocument()
  expect(screen.queryByText(/Affinity ID/)).not.toBeInTheDocument()
})
