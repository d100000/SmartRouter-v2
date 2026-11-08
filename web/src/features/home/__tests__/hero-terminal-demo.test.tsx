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
import { act, fireEvent, render, screen } from '@testing-library/react'
import type { FormEvent } from 'react'
import { afterEach, expect, it, vi } from 'vitest'

import { HeroTerminalDemo } from '../components/hero-terminal-demo'

afterEach(() => {
  vi.useRealTimers()
})

it('switches API examples without submitting the containing form', () => {
  vi.useFakeTimers()
  const onSubmit = vi.fn((event: FormEvent) => event.preventDefault())
  render(
    <form onSubmit={onSubmit}>
      <HeroTerminalDemo />
    </form>
  )

  fireEvent.click(screen.getByRole('button', { name: 'Responses' }))
  expect(onSubmit).not.toHaveBeenCalled()
  act(() => vi.runOnlyPendingTimers())
  expect(screen.getAllByText('/v1/responses').length).toBeGreaterThan(0)
  expect(screen.queryByText('/v1/chat/completions')).not.toBeInTheDocument()
})

it('keeps the API tab borders and selected hover color when using the shared button', () => {
  render(<HeroTerminalDemo />)

  const active = screen.getByRole('button', { name: 'Chat' })
  expect(active).toHaveClass(
    'h-auto',
    'rounded-none',
    'border-x-0',
    'border-t-0',
    'border-b-2'
  )
  expect(active).toHaveClass(
    'hover:text-emerald-600',
    'dark:hover:text-emerald-400'
  )
  expect(screen.getByRole('button', { name: 'Responses' })).toHaveClass(
    'hover:text-foreground/70'
  )
})
