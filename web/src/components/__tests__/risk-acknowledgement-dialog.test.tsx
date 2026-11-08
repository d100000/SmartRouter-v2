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
import { expect, it, vi } from 'vitest'

import { RiskAcknowledgementDialog } from '../risk-acknowledgement-dialog'

it('retains typed confirmation and focus when static explanation segments are inserted', async () => {
  const user = userEvent.setup()
  const input = {
    type: 'input' as const,
    text: 'I agree',
    placeholder: 'Type agreement',
  }
  const props = {
    open: true,
    title: 'Confirm operation',
    onOpenChange: vi.fn(),
    onConfirm: vi.fn(),
    confirmText: 'Confirm',
  }
  const { rerender } = render(
    <RiskAcknowledgementDialog {...props} requiredTextParts={[input]} />
  )
  await user.type(screen.getByPlaceholderText('Type agreement'), 'I agree')
  expect(screen.getByRole('button', { name: 'Confirm' })).toBeEnabled()
  rerender(
    <RiskAcknowledgementDialog
      {...props}
      requiredTextParts={[{ type: 'static', text: 'Confirmation: ' }, input]}
    />
  )
  expect(screen.getByPlaceholderText('Type agreement')).toHaveFocus()
  expect(screen.getByPlaceholderText('Type agreement')).toHaveValue('I agree')
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  expect(props.onConfirm).toHaveBeenCalledOnce()
})

it('keeps repeated confirmation segments distinct when unrelated text is inserted', async () => {
  const user = userEvent.setup()
  const repeated = {
    type: 'input' as const,
    text: 'Agree',
    placeholder: 'Confirm agreement',
  }
  const props = {
    open: true,
    title: 'Confirm repeated statements',
    onOpenChange: vi.fn(),
    onConfirm: vi.fn(),
    confirmText: 'Confirm',
  }
  const { rerender } = render(
    <RiskAcknowledgementDialog
      {...props}
      requiredTextParts={[repeated, { ...repeated }]}
    />
  )
  const inputs = screen.getAllByPlaceholderText('Confirm agreement')
  await user.type(inputs[0], 'Agree')
  await user.type(inputs[1], 'Agree')
  rerender(
    <RiskAcknowledgementDialog
      {...props}
      requiredTextParts={[
        { type: 'static', text: 'Statements: ' },
        repeated,
        { ...repeated },
      ]}
    />
  )
  const remainingInputs = screen.getAllByPlaceholderText('Confirm agreement')
  expect(remainingInputs).toHaveLength(2)
  expect(remainingInputs[0]).toHaveValue('Agree')
  expect(remainingInputs[1]).toHaveValue('Agree')
  expect(remainingInputs[1]).toHaveFocus()
  expect(screen.getByRole('button', { name: 'Confirm' })).toBeEnabled()
})
