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
import type { KeyboardEvent } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import { Label } from '@/components/ui/label'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'

export type OverviewDimension = 'channel' | 'group' | 'key' | 'supplier'

export interface OverviewFilters {
  dimension: OverviewDimension
  days: 7 | 30
  object: string
}

export interface OverviewObjectOption {
  value: string
  label: string
}

export function OverviewFiltersBar(props: {
  filters: OverviewFilters
  objects: OverviewObjectOption[]
  onChange: (filters: OverviewFilters) => void
}) {
  const { t } = useTranslation()
  const dimensions: Array<{ id: OverviewDimension; title: string }> = [
    { id: 'channel', title: t('Channel Analytics') },
    { id: 'group', title: t('Group Analytics') },
    { id: 'key', title: t('Upstream Key Analytics') },
    { id: 'supplier', title: t('Upstream Supplier Analytics') },
  ]

  return (
    <div className='min-w-0 space-y-4'>
      <Tabs
        value={props.filters.dimension}
        onValueChange={(dimension) =>
          props.onChange({
            ...props.filters,
            dimension: dimension as OverviewDimension,
            object: '',
          })
        }
      >
        <div className='overflow-x-auto pb-1'>
          <TabsList variant='line' aria-label={t('Channel Overview')}>
            {dimensions.map((dimension) => (
              <TabsTrigger key={dimension.id} value={dimension.id}>
                {dimension.title}
              </TabsTrigger>
            ))}
          </TabsList>
        </div>
      </Tabs>
      <div className='grid min-w-0 gap-3 rounded-lg border p-3 sm:flex sm:flex-wrap sm:items-stretch'>
        <div className='flex flex-col gap-1.5'>
          <Label>{t('Time Range')}</Label>
          <div
            className='flex flex-1 gap-2'
            role='group'
            aria-label={t('Time Range')}
          >
            <Button
              size='default'
              className='h-auto min-h-8'
              variant={props.filters.days === 7 ? 'default' : 'outline'}
              aria-pressed={props.filters.days === 7}
              onClick={() => props.onChange({ ...props.filters, days: 7 })}
            >
              {t('7 Days')}
            </Button>
            <Button
              size='default'
              className='h-auto min-h-8'
              variant={props.filters.days === 30 ? 'default' : 'outline'}
              aria-pressed={props.filters.days === 30}
              onClick={() => props.onChange({ ...props.filters, days: 30 })}
            >
              {t('30 Days')}
            </Button>
          </div>
        </div>
        <div className='flex min-w-0 flex-col gap-1.5 sm:w-72'>
          <Label htmlFor='overview-object'>{t('Analysis Object')}</Label>
          <Combobox
            id='overview-object'
            aria-label={t('Analysis Object')}
            options={[{ value: '', label: t('All') }, ...props.objects]}
            value={props.filters.object}
            onValueChange={(object) =>
              props.onChange({ ...props.filters, object: object ?? '' })
            }
            placeholder={t('All')}
            searchPlaceholder={t('Search name or ID, then press Enter')}
            onKeyDown={(event: KeyboardEvent<HTMLInputElement>) => {
              if (event.key !== 'Enter' || event.nativeEvent.isComposing) return
              const object = event.currentTarget.value.trim()
              if (!object || object.length > 191) return
              if (
                props.filters.dimension === 'channel' &&
                !/^[1-9]\d*$/.test(object)
              ) {
                return
              }
              const knownOption = props.objects.some(
                (option) =>
                  option.value.toLowerCase() === object.toLowerCase() ||
                  option.label.toLowerCase() === object.toLowerCase()
              )
              if (
                knownOption ||
                object.toLowerCase() === t('All').toLowerCase()
              ) {
                return
              }
              event.preventDefault()
              props.onChange({ ...props.filters, object })
            }}
            className='w-full min-w-0'
          />
        </div>
      </div>
    </div>
  )
}
