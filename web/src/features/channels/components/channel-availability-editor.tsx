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
import { nanoid } from 'nanoid'
import { useId } from 'react'
import { useController, useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'

import {
  availabilityValidationError,
  defaultAvailability,
  readAvailability,
  type AvailabilityConfig,
  type AvailabilityWindow,
} from '../lib/channel-availability'
import type { ChannelFormValues } from '../lib/channel-form'

const WEEKDAYS = [
  'Monday',
  'Tuesday',
  'Wednesday',
  'Thursday',
  'Friday',
  'Saturday',
  'Sunday',
] as const
const ALL_DAYS = [1, 2, 3, 4, 5, 6, 7]

function AvailabilityWindowFields(props: {
  value: AvailabilityWindow
  onChange: (window: AvailabilityWindow) => void
  overnight?: boolean
}) {
  const { t } = useTranslation()
  const id = useId()
  return (
    <FieldGroup className='grid gap-3 sm:grid-cols-2'>
      <Field>
        <FieldLabel htmlFor={`${id}-start`}>{t('Start time')}</FieldLabel>
        <Input
          id={`${id}-start`}
          value={props.value.start}
          placeholder='09:00'
          maxLength={5}
          onChange={(event) =>
            props.onChange({ ...props.value, start: event.target.value })
          }
        />
      </Field>
      <Field>
        <FieldLabel htmlFor={`${id}-end`}>{t('End time')}</FieldLabel>
        <Input
          id={`${id}-end`}
          value={props.value.end}
          placeholder='24:00'
          maxLength={5}
          onChange={(event) =>
            props.onChange({ ...props.value, end: event.target.value })
          }
        />
        {props.overnight && props.value.end < props.value.start && (
          <FieldDescription>{t('Ends on the next day')}</FieldDescription>
        )}
      </Field>
    </FieldGroup>
  )
}

export function ChannelAvailabilityEditor(props: { disabled?: boolean }) {
  const { t } = useTranslation()
  const id = useId()
  const form = useFormContext<ChannelFormValues>()
  const { field, fieldState } = useController({
    control: form.control,
    name: 'availability',
  })
  const config = readAvailability(field.value)
  const error =
    availabilityValidationError(field.value) || fieldState.error?.message
  const update = (value: AvailabilityConfig) => {
    field.onChange(JSON.stringify(value))
    field.onBlur()
  }
  if (!config) {
    return (
      <FieldSet disabled={props.disabled}>
        <FieldLegend>{t('Channel availability')}</FieldLegend>
        <Alert variant='destructive'>
          <AlertDescription>
            {t('Invalid channel availability configuration')}
          </AlertDescription>
        </Alert>
        <Button
          type='button'
          variant='outline'
          onClick={() => update(defaultAvailability())}
        >
          {t('Reset availability settings')}
        </Button>
      </FieldSet>
    )
  }
  return (
    <FieldSet
      disabled={props.disabled}
      aria-describedby={`${id}-help`}
      aria-invalid={Boolean(error)}
      className='border-border/60 rounded-lg border p-4'
    >
      <FieldLegend className='sr-only'>{t('Channel availability')}</FieldLegend>
      <Field orientation='horizontal'>
        <FieldLabel htmlFor={`${id}-enabled`}>
          {t('Limit channel availability')}
        </FieldLabel>
        <Switch
          id={`${id}-enabled`}
          checked={config.enabled}
          disabled={props.disabled}
          ref={field.ref}
          name={field.name}
          onCheckedChange={(enabled) =>
            update({
              ...config,
              enabled,
              rules: config.rules.length
                ? config.rules
                : defaultAvailability().rules,
            })
          }
        />
      </Field>
      <FieldDescription id={`${id}-help`}>
        {t(
          'Only new requests within these hours may use this channel. Active responses finish normally; manual and automatic disabling still apply.'
        )}
      </FieldDescription>
      {config.enabled && (
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor={`${id}-timezone`}>{t('Time zone')}</FieldLabel>
            <Input
              id={`${id}-timezone`}
              list={`${id}-zones`}
              value={config.timezone}
              placeholder='Asia/Shanghai'
              onChange={(event) =>
                update({ ...config, timezone: event.target.value })
              }
            />
            <datalist id={`${id}-zones`}>
              {[
                'Asia/Shanghai',
                'UTC',
                'Asia/Tokyo',
                'Asia/Singapore',
                'America/New_York',
                'America/Los_Angeles',
                'Europe/London',
                'Europe/Berlin',
              ].map((zone) => (
                <option key={zone} value={zone} />
              ))}
            </datalist>
            <FieldDescription>
              {t(
                'Use an IANA time zone. Hours follow its local clock, including daylight saving changes.'
              )}
            </FieldDescription>
          </Field>
          <Field>
            <FieldLabel id={`${id}-mode`}>{t('Repeat schedule')}</FieldLabel>
            <ToggleGroup
              aria-labelledby={`${id}-mode`}
              value={[config.mode]}
              variant='outline'
              disabled={props.disabled}
              onValueChange={(values) => {
                const mode = values[0]
                if (mode === 'daily' || mode === 'weekly') {
                  update({
                    ...config,
                    mode,
                    rules: config.rules.map((rule) => ({
                      ...rule,
                      weekdays: rule.weekdays?.length
                        ? rule.weekdays
                        : ALL_DAYS,
                    })),
                  })
                }
              }}
            >
              <ToggleGroupItem value='daily'>{t('Every day')}</ToggleGroupItem>
              <ToggleGroupItem value='weekly'>
                {t('By weekday')}
              </ToggleGroupItem>
            </ToggleGroup>
          </Field>
          <FieldDescription>
            {t(
              'Use HH:MM. Start is inclusive; end is exclusive. Use 00:00–24:00 for all day. Overlapping windows are combined.'
            )}
          </FieldDescription>
          {config.rules.map((rule, index) => (
            <FieldSet key={rule.id} className='bg-muted/30 rounded-md p-3'>
              <FieldLegend variant='label'>
                {t('Availability window {{number}}', { number: index + 1 })}
              </FieldLegend>
              {config.mode === 'weekly' && (
                <Field>
                  <FieldLabel id={`${id}-days-${index}`}>
                    {t('Weekdays')}
                  </FieldLabel>
                  <ToggleGroup
                    multiple
                    aria-labelledby={`${id}-days-${index}`}
                    value={(rule.weekdays || []).map(String)}
                    variant='outline'
                    size='sm'
                    spacing={1}
                    className='flex-wrap'
                    disabled={props.disabled}
                    onValueChange={(days) =>
                      update({
                        ...config,
                        rules: config.rules.map((item, position) =>
                          position === index
                            ? { ...item, weekdays: days.map(Number) }
                            : item
                        ),
                      })
                    }
                  >
                    {WEEKDAYS.map((day, dayIndex) => (
                      <ToggleGroupItem key={day} value={String(dayIndex + 1)}>
                        {t(day)}
                      </ToggleGroupItem>
                    ))}
                  </ToggleGroup>
                  <div className='flex flex-wrap gap-2'>
                    {(
                      [
                        { label: 'Every day', days: ALL_DAYS },
                        { label: 'Working days', days: [1, 2, 3, 4, 5] },
                        { label: 'Weekend', days: [6, 7] },
                      ] as const
                    ).map((preset) => (
                      <Button
                        key={preset.label}
                        type='button'
                        variant='ghost'
                        size='sm'
                        onClick={() =>
                          update({
                            ...config,
                            rules: config.rules.map((item, position) =>
                              position === index
                                ? { ...item, weekdays: [...preset.days] }
                                : item
                            ),
                          })
                        }
                      >
                        {t(preset.label)}
                      </Button>
                    ))}
                  </div>
                </Field>
              )}
              <AvailabilityWindowFields
                value={rule}
                overnight
                onChange={(window) =>
                  update({
                    ...config,
                    rules: config.rules.map((item, position) =>
                      position === index ? { ...item, ...window } : item
                    ),
                  })
                }
              />
              <div className='flex flex-wrap gap-2'>
                <Button
                  type='button'
                  variant='ghost'
                  size='sm'
                  onClick={() =>
                    update({
                      ...config,
                      rules: config.rules.map((item, position) =>
                        position === index
                          ? { ...item, start: '00:00', end: '24:00' }
                          : item
                      ),
                    })
                  }
                >
                  {t('All day')}
                </Button>
                <Button
                  type='button'
                  variant='ghost'
                  size='sm'
                  aria-label={t('Remove availability window {{number}}', {
                    number: index + 1,
                  })}
                  onClick={() =>
                    update({
                      ...config,
                      rules: config.rules.filter(
                        (_, position) => position !== index
                      ),
                    })
                  }
                >
                  {t('Remove')}
                </Button>
              </div>
            </FieldSet>
          ))}
          <Button
            type='button'
            variant='outline'
            disabled={props.disabled || config.rules.length >= 32}
            onClick={() =>
              update({
                ...config,
                rules: [
                  ...config.rules,
                  {
                    id: nanoid(),
                    start: '09:00',
                    end: '18:00',
                    weekdays: ALL_DAYS,
                  },
                ],
              })
            }
          >
            {t('Add availability window')}
          </Button>
          <FieldSet>
            <FieldLegend variant='label'>{t('Date exceptions')}</FieldLegend>
            <FieldDescription>
              {t(
                'An exception replaces all hours on that local date, including overnight hours from the previous day. Exception windows stay within the selected date.'
              )}
            </FieldDescription>
            {config.exceptions.map((exception, index) => (
              <FieldSet
                key={exception.id}
                className='bg-muted/30 rounded-md p-3'
              >
                <FieldLegend variant='label'>
                  {t('Date exception {{number}}', { number: index + 1 })}
                </FieldLegend>
                <Field>
                  <FieldLabel htmlFor={`${id}-date-${index}`}>
                    {t('Date')}
                  </FieldLabel>
                  <Input
                    type='date'
                    id={`${id}-date-${index}`}
                    value={exception.date}
                    onChange={(event) =>
                      update({
                        ...config,
                        exceptions: config.exceptions.map((item, position) =>
                          position === index
                            ? { ...item, date: event.target.value }
                            : item
                        ),
                      })
                    }
                  />
                </Field>
                <Field orientation='horizontal'>
                  <FieldLabel htmlFor={`${id}-closed-${index}`}>
                    {t('Closed all day')}
                  </FieldLabel>
                  <Switch
                    id={`${id}-closed-${index}`}
                    checked={exception.windows.length === 0}
                    disabled={props.disabled}
                    onCheckedChange={(closed) =>
                      update({
                        ...config,
                        exceptions: config.exceptions.map((item, position) =>
                          position === index
                            ? {
                                ...item,
                                windows: closed
                                  ? []
                                  : [
                                      {
                                        id: nanoid(),
                                        start: '09:00',
                                        end: '18:00',
                                      },
                                    ],
                              }
                            : item
                        ),
                      })
                    }
                  />
                </Field>
                {exception.windows.map((window, windowIndex) => (
                  <FieldSet key={window.id}>
                    <AvailabilityWindowFields
                      value={window}
                      onChange={(value) =>
                        update({
                          ...config,
                          exceptions: config.exceptions.map((item, position) =>
                            position === index
                              ? {
                                  ...item,
                                  windows: item.windows.map(
                                    (current, currentIndex) =>
                                      currentIndex === windowIndex
                                        ? value
                                        : current
                                  ),
                                }
                              : item
                          ),
                        })
                      }
                    />
                    <Button
                      type='button'
                      size='sm'
                      variant='ghost'
                      aria-label={t('Remove exception window {{number}}', {
                        number: windowIndex + 1,
                      })}
                      onClick={() =>
                        update({
                          ...config,
                          exceptions: config.exceptions.map((item, position) =>
                            position === index
                              ? {
                                  ...item,
                                  windows: item.windows.filter(
                                    (_, currentIndex) =>
                                      currentIndex !== windowIndex
                                  ),
                                }
                              : item
                          ),
                        })
                      }
                    >
                      {t('Remove')}
                    </Button>
                  </FieldSet>
                ))}
                {exception.windows.length > 0 && (
                  <Button
                    type='button'
                    variant='outline'
                    disabled={props.disabled || exception.windows.length >= 8}
                    onClick={() =>
                      update({
                        ...config,
                        exceptions: config.exceptions.map((item, position) =>
                          position === index
                            ? {
                                ...item,
                                windows: [
                                  ...item.windows,
                                  {
                                    id: nanoid(),
                                    start: '09:00',
                                    end: '18:00',
                                  },
                                ],
                              }
                            : item
                        ),
                      })
                    }
                  >
                    {t('Add availability window')}
                  </Button>
                )}
                <Button
                  type='button'
                  variant='ghost'
                  size='sm'
                  aria-label={t('Remove date exception {{number}}', {
                    number: index + 1,
                  })}
                  onClick={() =>
                    update({
                      ...config,
                      exceptions: config.exceptions.filter(
                        (_, position) => position !== index
                      ),
                    })
                  }
                >
                  {t('Remove date exception')}
                </Button>
              </FieldSet>
            ))}
            <Button
              type='button'
              variant='outline'
              disabled={props.disabled || config.exceptions.length >= 100}
              onClick={() =>
                update({
                  ...config,
                  exceptions: [
                    ...config.exceptions,
                    { id: nanoid(), date: '', windows: [] },
                  ],
                })
              }
            >
              {t('Add date exception')}
            </Button>
          </FieldSet>
        </FieldGroup>
      )}
      {error && (
        <Alert variant='destructive' role='alert'>
          <AlertDescription>{t(error)}</AlertDescription>
        </Alert>
      )}
    </FieldSet>
  )
}
