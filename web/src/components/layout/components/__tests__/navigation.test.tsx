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
import { expect, it } from 'vitest'

import { NavLinkList } from '../nav-link-item'

it('keeps keyboard focus on the same destination when navigation links are reordered', () => {
  const docs = {
    title: 'Docs',
    href: 'https://docs.example.test',
    external: true,
  }
  const help = {
    title: 'Help',
    href: 'https://help.example.test',
    external: true,
  }
  const { rerender } = render(<NavLinkList links={[docs, help]} />)
  screen.getByRole('link', { name: 'Docs' }).focus()
  rerender(<NavLinkList links={[help, docs]} />)
  expect(screen.getByRole('link', { name: 'Docs' })).toHaveFocus()
  expect(screen.getByRole('link', { name: 'Docs' })).toHaveAttribute(
    'href',
    docs.href
  )
})

it('keeps repeated navigation destinations distinct when another link is inserted', () => {
  const docs = {
    title: 'Docs',
    href: 'https://docs.example.test',
    external: true,
  }
  const help = {
    title: 'Help',
    href: 'https://help.example.test',
    external: true,
  }
  const { rerender } = render(<NavLinkList links={[docs, { ...docs }]} />)
  screen.getAllByRole('link', { name: 'Docs' })[1].focus()
  rerender(<NavLinkList links={[help, docs, { ...docs }]} />)
  expect(screen.getAllByRole('link', { name: 'Docs' })).toHaveLength(2)
  expect(screen.getAllByRole('link', { name: 'Docs' })[1]).toHaveFocus()
  expect(screen.getByRole('link', { name: 'Help' })).toHaveAttribute(
    'href',
    help.href
  )
})
