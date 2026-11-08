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
import { expect, it } from 'vitest'

import { WebPreview, WebPreviewBody, WebPreviewConsole } from '../web-preview'

it('keeps preview scripts isolated from the host origin and prevents sandbox overrides', () => {
  const override = {
    title: 'Preview',
    sandbox: 'allow-scripts allow-same-origin',
  }
  render(
    <WebPreview defaultUrl='https://preview.example.test'>
      <WebPreviewBody {...override} />
    </WebPreview>
  )
  const frame = screen.getByTitle('Preview')
  expect(frame).toHaveAttribute('src', 'https://preview.example.test')
  expect(frame.getAttribute('sandbox')?.split(' ')).toContain('allow-scripts')
  expect(frame.getAttribute('sandbox')?.split(' ')).not.toContain(
    'allow-same-origin'
  )
  expect(frame).toHaveAttribute('referrerpolicy', 'no-referrer')
})

it('keeps repeated console events distinct when an earlier event is inserted', async () => {
  const user = userEvent.setup()
  const repeated = {
    level: 'log' as const,
    timestamp: new Date('2026-11-09T00:00:00Z'),
    message: 'Repeated event',
  }
  const earlier = {
    level: 'warn' as const,
    timestamp: new Date('2026-11-08T23:59:59Z'),
    message: 'Earlier event',
  }
  const { rerender } = render(
    <WebPreview>
      <WebPreviewConsole logs={[repeated, repeated]} />
    </WebPreview>
  )
  await user.click(screen.getByRole('button', { name: 'Console' }))
  expect(screen.getAllByText('Repeated event')).toHaveLength(2)
  rerender(
    <WebPreview>
      <WebPreviewConsole logs={[earlier, repeated, repeated]} />
    </WebPreview>
  )
  expect(screen.getAllByText('Repeated event')).toHaveLength(2)
  expect(screen.getByText('Earlier event')).toBeVisible()
})
