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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { afterEach, expect, it, vi } from 'vitest'

import { Button } from '@/components/ui/button'

import {
  PromptInput,
  PromptInputAttachments,
  PromptInputProvider,
  PromptInputTextarea,
} from '../prompt-input'

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

it('reports a failed attachment read and retains the draft and attachment for retry', async () => {
  const user = userEvent.setup()
  vi.stubGlobal(
    'fetch',
    vi.fn().mockRejectedValue(new Error('Attachment read failed'))
  )
  vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:attachment')
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined)
  const notify = vi
    .spyOn(toast, 'error')
    .mockImplementation(() => 'notification')
  const submit = vi.fn()
  render(
    <PromptInputProvider initialInput='Keep my draft'>
      <PromptInput onSubmit={submit}>
        <PromptInputTextarea aria-label='Message' />
        <PromptInputAttachments>
          {(file) => <span>{file.filename}</span>}
        </PromptInputAttachments>
        <Button type='submit'>Send</Button>
      </PromptInput>
    </PromptInputProvider>
  )
  await user.upload(
    screen.getByLabelText('Upload files'),
    new File(['image'], 'photo.png', { type: 'image/png' })
  )
  await user.click(screen.getByRole('button', { name: 'Send' }))
  await waitFor(() =>
    expect(notify).toHaveBeenCalledWith('Attachment read failed')
  )
  expect(submit).not.toHaveBeenCalled()
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue(
    'Keep my draft'
  )
  expect(screen.getByText('photo.png')).toBeInTheDocument()
})
