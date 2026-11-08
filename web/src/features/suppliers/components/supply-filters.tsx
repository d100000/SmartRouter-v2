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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DateTimePicker } from '@/components/datetime-picker'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import type { SupplyLogFilters, SupplyStage, SupplyTaskStatus } from '../types'

export function SupplyFilters(props: {
  onChange: (value: SupplyLogFilters) => void
  logs?: boolean
  children?: React.ReactNode
}) {
  const { t } = useTranslation()
  const [filter, setFilter] = useState({
    channel: '',
    model: '',
    reference: '',
    result: 'all',
    stage: 'all' as SupplyStage | 'all',
    task: 'all' as SupplyTaskStatus | 'all',
    days: '7',
  })
  const [from, setFrom] = useState<Date>()
  const [to, setTo] = useState<Date>()
  const [error, setError] = useState(false)
  const stages = [
    { value: 'all', label: t('All stages') },
    { value: 'inference', label: t('Inference') },
    { value: 'embedding', label: t('Embedding') },
    { value: 'rerank', label: t('Rerank') },
    { value: 'image', label: t('Image') },
    { value: 'audio', label: t('Audio') },
    { value: 'task_submit', label: t('Task submission') },
    { value: 'realtime_generation', label: t('Realtime generation') },
    { value: 'realtime_transcription', label: t('Realtime transcription') },
  ]
  const ranges = [
    { value: '1', label: t('Last 24 hours') },
    { value: '7', label: t('Last 7 days') },
    { value: '30', label: t('Last 30 days') },
    { value: 'custom', label: t('Custom Time Range') },
  ]
  const results = [
    { value: 'all', label: t('All results') },
    { value: 'success', label: t('Success') },
    { value: 'failure', label: t('Failed') },
    { value: 'unknown', label: t('Unknown') },
  ]
  const taskStates = [
    { value: 'all', label: t('All task states') },
    { value: 'accepted', label: t('Accepted, awaiting task status') },
    { value: 'queued', label: t('Queued') },
    { value: 'running', label: t('Running') },
    { value: 'succeeded', label: t('Confirmed success') },
    { value: 'failed', label: t('Confirmed failure') },
    { value: 'unknown', label: t('Unknown task result') },
  ]
  return (
    <form
      className='space-y-3'
      onSubmit={(event) => {
        event.preventDefault()
        const custom = filter.days === 'custom'
        if (
          custom &&
          (!from ||
            !to ||
            to <= from ||
            to.getTime() - from.getTime() > 90 * 86400000)
        ) {
          setError(true)
          return
        }
        setError(false)
        const channel = Number(filter.channel)
        props.onChange({
          channel_id:
            Number.isSafeInteger(channel) && channel > 0 ? channel : undefined,
          model: filter.model.trim() || undefined,
          stage: filter.stage === 'all' ? undefined : filter.stage,
          correlation_id: props.logs
            ? filter.reference.trim() || undefined
            : undefined,
          result:
            props.logs && filter.result !== 'all' ? filter.result : undefined,
          task_status:
            props.logs && filter.task !== 'all' ? filter.task : undefined,
          ...(custom && from && to
            ? {
                from: Math.floor(from.getTime() / 1000),
                to: Math.floor(to.getTime() / 1000),
              }
            : { days: Number(filter.days) }),
        })
      }}
    >
      <div className='flex flex-wrap gap-2'>
        <Input
          className='w-32'
          aria-label={t('Channel ID')}
          placeholder={t('Channel ID')}
          inputMode='numeric'
          value={filter.channel}
          onChange={(event) =>
            setFilter({ ...filter, channel: event.target.value })
          }
        />
        <Input
          className='w-40'
          aria-label={t('Model')}
          placeholder={t('Model')}
          value={filter.model}
          onChange={(event) =>
            setFilter({ ...filter, model: event.target.value })
          }
        />
        <Select
          value={filter.days}
          onValueChange={(value) =>
            setFilter({ ...filter, days: value ?? '7' })
          }
          items={ranges}
        >
          <SelectTrigger aria-label={t('Time range')}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {ranges.map((item) => (
              <SelectItem key={item.value} value={item.value}>
                {item.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={filter.stage}
          onValueChange={(value) =>
            setFilter({
              ...filter,
              stage: (value ?? 'all') as SupplyStage | 'all',
            })
          }
          items={stages}
        >
          <SelectTrigger aria-label={t('Stage')}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {stages.map((item) => (
              <SelectItem key={item.value} value={item.value}>
                {item.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {props.logs && (
          <>
            <Input
              className='w-48'
              aria-label={t('Reference')}
              placeholder={t('Reference')}
              value={filter.reference}
              onChange={(event) =>
                setFilter({ ...filter, reference: event.target.value })
              }
            />
            <Select
              value={filter.result}
              onValueChange={(value) =>
                setFilter({ ...filter, result: value ?? 'all' })
              }
              items={results}
            >
              <SelectTrigger aria-label={t('Result')}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {results.map((item) => (
                  <SelectItem key={item.value} value={item.value}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select
              value={filter.task}
              onValueChange={(value) =>
                setFilter({
                  ...filter,
                  task: (value ?? 'all') as SupplyTaskStatus | 'all',
                })
              }
              items={taskStates}
            >
              <SelectTrigger aria-label={t('Task status')}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {taskStates.map((item) => (
                  <SelectItem key={item.value} value={item.value}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </>
        )}
        <Button type='submit' variant='outline'>
          {t('Filter')}
        </Button>
        {props.children}
      </div>
      {props.logs && (
        <p className='text-muted-foreground text-xs'>
          {t('Submission acceptance and task completion are separate results.')}
        </p>
      )}
      {filter.days === 'custom' && (
        <div className='grid gap-2 sm:grid-cols-2'>
          <DateTimePicker
            value={from}
            onChange={setFrom}
            placeholder={t('Select start time')}
          />
          <DateTimePicker
            value={to}
            onChange={setTo}
            placeholder={t('Select end time')}
          />
        </div>
      )}
      {error && (
        <p role='alert' className='text-destructive text-sm'>
          {t('Select a valid time range of up to 90 days.')}
        </p>
      )}
    </form>
  )
}
