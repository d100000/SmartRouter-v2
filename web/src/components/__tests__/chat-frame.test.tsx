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

import { ChatFrame } from '../chat-frame'

it('provides an isolated chat embed and a safe full-client link using the same resolved URL', () => {
  const url = 'https://chat.example.test/?api_key=existing-test-key'
  render(<ChatFrame src={url} title='Chat preset: Example' />)
  const frame = screen.getByTitle('Chat preset: Example')
  expect(frame).toHaveAttribute('src', url)
  expect(frame.getAttribute('sandbox')?.split(' ')).toContain('allow-scripts')
  expect(frame.getAttribute('sandbox')?.split(' ')).toContain('allow-forms')
  expect(frame.getAttribute('sandbox')?.split(' ')).not.toContain(
    'allow-same-origin'
  )
  expect(frame).toHaveAttribute('referrerpolicy', 'no-referrer')
  const external = screen.getByRole('link', { name: 'Open in new tab' })
  expect(external).toHaveAttribute('href', url)
  expect(external).toHaveAttribute('target', '_blank')
  expect(external).toHaveAttribute('rel', 'noopener noreferrer')
  expect(external).toHaveAttribute('referrerpolicy', 'no-referrer')
  expect(
    screen.getByText(
      'Open in a new tab to use sign-in, saved chats, camera, or microphone.'
    )
  ).toBeVisible()
})
