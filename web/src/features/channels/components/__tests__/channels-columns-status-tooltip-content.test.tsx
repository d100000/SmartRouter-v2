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
import { expect, test } from 'vitest'

import { channelSchema, type Channel } from '../../types'
import { useChannelsColumns } from '../channels-columns'
import { ChannelsProvider } from '../channels-provider'

function ExampleAutoDisabledStatusCell(props: { channel: Channel }) {
  const table = useReactTable({
    data: [props.channel],
    columns: useChannelsColumns({ enableSelection: false }),
    getCoreRowModel: getCoreRowModel(),
  })
  const cell = table
    .getRowModel()
    .rows[0]?.getAllCells()
    .find((item) => item.column.id === 'status')

  return cell ? flexRender(cell.column.columnDef.cell, cell.getContext()) : null
}

test('keeps the long string inside the status tooltip is wrapped when show', async () => {
  const reason = '114514'.repeat(40)
  const channelItem = channelSchema.parse({
    id: 1,
    type: 1,
    key: 'test-key',
    name: 'Test-Channel-Item',
    status: 3,
    created_time: 1,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    other_info: JSON.stringify({ status_reason: reason }),
  })
  const queryClient = new QueryClient()
  const user = userEvent.setup()

  render(
    <QueryClientProvider client={queryClient}>
      <ChannelsProvider>
        <ExampleAutoDisabledStatusCell channel={channelItem} />
      </ChannelsProvider>
    </QueryClientProvider>
  )

  await user.hover(screen.getByText('Auto Disabled'))

  expect(await screen.findByText(reason, { exact: false })).toHaveClass(
    'wrap-anywhere'
  )
})

test('scheduled closure displays its own status and the next opening in the channel time zone', async () => {
  const channel = channelSchema.parse({
    id: 2,
    type: 1,
    key: '',
    name: 'Scheduled',
    status: 1,
    effective_status: 4,
    created_time: 0,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    availability_state: {
      enabled: true,
      open: false,
      timezone: 'Asia/Shanghai',
      evaluated_at: 0,
      preview_days: 8,
      next_change_at: Date.parse('2026-11-01T01:00:00Z') / 1000,
    },
  })
  const queryClient = new QueryClient()
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={queryClient}>
      <ChannelsProvider>
        <ExampleAutoDisabledStatusCell channel={channel} />
      </ChannelsProvider>
    </QueryClientProvider>
  )
  await user.hover(screen.getByText('Outside availability hours'))
  expect(
    await screen.findByText('Time zone: Asia/Shanghai')
  ).toBeInTheDocument()
  expect(
    await screen.findByText(/Available again at .*09:00/)
  ).toBeInTheDocument()
  queryClient.clear()
})
