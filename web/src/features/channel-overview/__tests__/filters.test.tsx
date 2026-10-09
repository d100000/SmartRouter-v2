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
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, test } from 'vitest'

import {
  OverviewFiltersBar,
  type OverviewFilters,
} from '../components/overview-filters'

function FilterFixture() {
  const [filters, setFilters] = useState<OverviewFilters>({
    dimension: 'channel',
    days: 7,
    object: '1',
  })
  return (
    <>
      <OverviewFiltersBar
        filters={filters}
        objects={[{ value: '1', label: 'Primary channel with a long name' }]}
        onChange={setFilters}
      />
      <output aria-label='Applied filters'>
        {`${filters.dimension}:${filters.days}:${filters.object}`}
      </output>
    </>
  )
}

describe('overview filters', () => {
  test('an object absent from bounded options is applied by ID only after Enter', async () => {
    const user = userEvent.setup()
    render(<FilterFixture />)
    const input = screen.getByRole('combobox', { name: 'Analysis Object' })
    await user.click(input)
    await user.clear(input)
    await user.type(input, '101')
    expect(screen.getByLabelText('Applied filters')).toHaveTextContent(
      'channel:7:1'
    )
    await user.keyboard('{Enter}')
    expect(screen.getByLabelText('Applied filters')).toHaveTextContent(
      'channel:7:101'
    )
  })

  test('changing the dimension clears its object while preserving the selected time range', async () => {
    const user = userEvent.setup()
    render(<FilterFixture />)
    await user.click(screen.getByRole('button', { name: '30 Days' }))
    expect(screen.getByRole('button', { name: '30 Days' })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await user.click(
      screen.getByRole('tab', { name: 'Upstream Key Analytics' })
    )
    expect(screen.getByLabelText('Applied filters')).toHaveTextContent(
      'key:30:'
    )
    expect(
      screen.getByRole('tab', { name: 'Upstream Key Analytics' })
    ).toHaveAttribute('aria-selected', 'true')
    expect(
      screen.getByRole('combobox', { name: 'Analysis Object' })
    ).toHaveValue('All')
  })

  test('choosing all objects clears the filter and keeps the selector within its responsive container', async () => {
    const user = userEvent.setup()
    render(<FilterFixture />)
    const input = screen.getByRole('combobox', { name: 'Analysis Object' })
    expect(input).toHaveValue('Primary channel with a long name')
    await user.click(screen.getByRole('button', { name: 'Analysis Object' }))
    await user.click(screen.getByRole('option', { name: 'All' }))
    expect(screen.getByLabelText('Applied filters')).toHaveTextContent(
      'channel:7:'
    )
    expect(input.closest('[data-slot="input-group"]')).toHaveClass('w-full')
    expect(screen.getByRole('tablist')).toHaveAccessibleName('Channel Overview')
  })
})
