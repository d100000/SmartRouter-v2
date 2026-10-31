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
import { zodResolver } from '@hookform/resolvers/zod'
import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useForm } from 'react-hook-form'
import { describe, expect, test, vi } from 'vitest'

import { Form } from '@/components/ui/form'

import {
  availabilityValidationError,
  defaultAvailability,
  formatAvailabilityTime,
} from '../../lib/channel-availability'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
  type ChannelFormValues,
} from '../../lib/channel-form'
import { channelSchema } from '../../types'
import { ChannelAvailabilityEditor } from '../channel-availability-editor'

function Editor(props: {
  raw?: string
  disabled?: boolean
  onSave: (value: string) => void
}) {
  const form = useForm<ChannelFormValues>({
    resolver: zodResolver(channelFormSchema),
    defaultValues: {
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'Availability channel',
      key: 'fixture-key',
      models: 'fixture-model',
      availability: props.raw || '{}',
    },
  })
  return (
    <Form {...form}>
      <form
        onSubmit={form.handleSubmit((values) =>
          props.onSave(values.availability || '{}')
        )}
      >
        <ChannelAvailabilityEditor disabled={props.disabled} />
        <button type='submit'>Save</button>
      </form>
    </Form>
  )
}

describe('channel availability editor', () => {
  test('enables daily windows and preserves the explicit time zone through save and reload', async () => {
    const save = vi.fn()
    const user = userEvent.setup()
    render(<Editor onSave={save} />)
    expect(screen.queryByLabelText('Time zone')).not.toBeInTheDocument()
    await user.click(
      screen.getByRole('switch', { name: 'Limit channel availability' })
    )
    expect(screen.getByLabelText('Time zone')).toHaveValue('Asia/Shanghai')
    await user.click(screen.getByRole('button', { name: 'All day' }))
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(save).toHaveBeenCalledOnce()
    const raw = save.mock.calls[0][0] as string
    expect(JSON.parse(raw)).toMatchObject({
      enabled: true,
      mode: 'daily',
      timezone: 'Asia/Shanghai',
      rules: [{ start: '00:00', end: '24:00' }],
    })
    const values = { ...CHANNEL_FORM_DEFAULT_VALUES, availability: raw }
    const created = transformFormDataToCreatePayload(values).channel
    const updated = transformFormDataToUpdatePayload(values, 1)
    expect(created.availability).toBe(raw)
    expect(updated.availability).toBe(raw)
    const channel = channelSchema.parse({
      ...created,
      key: '',
      models: 'fixture-model',
      other: '',
      remark: '',
      id: 1,
      name: 'saved',
      created_time: 0,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
    })
    expect(transformChannelToFormDefaults(channel).availability).toBe(raw)
  })

  test('weekly presets and overnight windows retain the start-day selection', async () => {
    const save = vi.fn()
    const user = userEvent.setup()
    render(
      <Editor
        onSave={save}
        raw={JSON.stringify({ ...defaultAvailability(), enabled: true })}
      />
    )
    await user.click(screen.getByRole('button', { name: 'By weekday' }))
    await user.click(screen.getByRole('button', { name: 'Weekend' }))
    await user.clear(screen.getByLabelText('Start time'))
    await user.type(screen.getByLabelText('Start time'), '22:00')
    await user.clear(screen.getByLabelText('End time'))
    await user.type(screen.getByLabelText('End time'), '02:00')
    expect(screen.getByText('Ends on the next day')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(save).toHaveBeenCalledOnce()
    expect(JSON.parse(save.mock.calls[0][0])).toMatchObject({
      mode: 'weekly',
      rules: [{ weekdays: [6, 7], start: '22:00', end: '02:00' }],
    })
  })

  test('requires a valid date for a closed exception and supports a future date', async () => {
    const save = vi.fn()
    const user = userEvent.setup()
    render(
      <Editor
        onSave={save}
        raw={JSON.stringify({ ...defaultAvailability(), enabled: true })}
      />
    )
    await user.click(screen.getByRole('button', { name: 'Add date exception' }))
    expect(screen.getByRole('switch', { name: 'Closed all day' })).toBeChecked()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(save).not.toHaveBeenCalled()
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Date exceptions require a valid date'
    )
    fireEvent.change(screen.getByLabelText('Date', { exact: true }), {
      target: { value: '2030-12-01' },
    })
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(save).toHaveBeenCalledOnce()
    expect(JSON.parse(save.mock.calls[0][0]).exceptions).toMatchObject([
      { date: '2030-12-01', windows: [] },
    ])
  })

  test('an enabled schedule cannot be saved without any windows', async () => {
    const save = vi.fn()
    const user = userEvent.setup()
    render(
      <Editor
        onSave={save}
        raw={JSON.stringify({ ...defaultAvailability(), enabled: true })}
      />
    )
    await user.click(
      screen.getByRole('button', { name: 'Remove availability window 1' })
    )
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(save).not.toHaveBeenCalled()
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Availability requires 1 to 32 rules'
    )
    await user.click(
      screen.getByRole('switch', { name: 'Limit channel availability' })
    )
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(save).toHaveBeenCalledOnce()
    expect(JSON.parse(save.mock.calls[0][0]).enabled).toBe(false)
  })

  test('disabled editing prevents schedule changes and keyboard interaction toggles the enabled switch', async () => {
    const user = userEvent.setup()
    const save = vi.fn()
    const { rerender } = render(<Editor onSave={save} disabled />)
    expect(
      screen.getByRole('switch', { name: 'Limit channel availability' })
    ).toHaveAttribute('aria-disabled', 'true')
    rerender(<Editor onSave={save} />)
    screen.getByRole('switch', { name: 'Limit channel availability' }).focus()
    await user.keyboard('[Space]')
    expect(
      screen.getByRole('switch', { name: 'Limit channel availability' })
    ).toBeChecked()
    expect(
      within(
        screen.getByRole('group', { name: 'Channel availability' })
      ).getByLabelText('Start time')
    ).toHaveValue('09:00')
  })

  test('malformed saved configuration is visible and requires an explicit reset', async () => {
    const user = userEvent.setup()
    const save = vi.fn()
    render(<Editor onSave={save} raw='{broken' />)
    expect(
      screen.getByText('Invalid channel availability configuration')
    ).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(save).not.toHaveBeenCalled()
    await user.click(
      screen.getByRole('button', { name: 'Reset availability settings' })
    )
    expect(
      screen.getByRole('switch', { name: 'Limit channel availability' })
    ).not.toBeChecked()
  })
})

test.each([
  [
    { ...defaultAvailability(), enabled: true, timezone: 'Invalid/Timezone' },
    'Select a valid IANA time zone',
  ],
  [
    {
      ...defaultAvailability(),
      enabled: true,
      rules: [{ start: '12:00', end: '12:00' }],
    },
    'Availability start and end must differ; use 00:00 to 24:00 for all day',
  ],
  [
    {
      ...defaultAvailability(),
      enabled: true,
      exceptions: [{ date: '2026-02-30', windows: [] }],
    },
    'Date exceptions require a valid date and at most 8 time windows',
  ],
])(
  'invalid availability config %j reports the relevant validation message',
  (config, message) => {
    expect(availabilityValidationError(JSON.stringify(config))).toBe(message)
  }
)

test.each(['zhCN', 'zhTW', 'en', 'fr', 'ja', 'ru', 'vi'])(
  'availability time formatting supports interface language %s and the schedule time zone',
  (language) => {
    const timestamp = Date.parse('2026-11-01T01:00:00Z') / 1000
    expect(
      formatAvailabilityTime(timestamp, 'Asia/Shanghai', language)
    ).toContain('09:00')
    expect(formatAvailabilityTime(timestamp, 'UTC', language)).toContain(
      '01:00'
    )
  }
)
