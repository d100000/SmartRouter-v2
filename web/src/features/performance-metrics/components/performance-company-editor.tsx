/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import type { FieldErrors } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { MultiSelect, type Option } from '@/components/multi-select'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Field, FieldError, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'

import type { PerformanceHealthCompany } from '../types'

type ModelGroup = NonNullable<PerformanceHealthCompany['groups']>[number]

export function PerformanceCompanyEditor(props: {
  company: PerformanceHealthCompany
  options: Option[]
  errors?: FieldErrors<PerformanceHealthCompany>
  disabled: boolean
  onChange: (company: PerformanceHealthCompany) => void
  onDelete: () => void
  onAddGroup: () => void
  onDeleteGroup: (group: ModelGroup) => void
  onMoveModels: (group: ModelGroup) => void
}) {
  const { t } = useTranslation()
  const company = props.company
  const updateGroup = (id: string, patch: Partial<ModelGroup>) => {
    props.onChange({
      ...company,
      groups: (company.groups ?? []).map((group) =>
        group.id === id ? { ...group, ...patch } : group
      ),
    })
  }

  return (
    <section
      aria-label={company.name}
      className='space-y-4 border-b pb-5 last:border-b-0'
    >
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <label className='flex items-center gap-2 text-sm'>
          <Checkbox
            checked={company.status !== 'disabled' && company.status !== 0}
            disabled={props.disabled}
            onCheckedChange={(checked) =>
              props.onChange({
                ...company,
                status: checked ? 'active' : 'disabled',
              })
            }
          />
          {t('Company active')}
        </label>
        <Button
          type='button'
          size='sm'
          variant='ghost'
          disabled={props.disabled}
          onClick={props.onDelete}
        >
          {t('Delete company')}
        </Button>
      </div>
      <div className='grid gap-3 sm:grid-cols-[minmax(0,1fr)_8rem]'>
        <Field data-invalid={Boolean(props.errors?.name)}>
          <FieldLabel htmlFor={`company-${company.id}-name`}>
            {t('Company name')}
          </FieldLabel>
          <Input
            id={`company-${company.id}-name`}
            value={company.name}
            disabled={props.disabled}
            aria-invalid={Boolean(props.errors?.name)}
            onChange={(event) =>
              props.onChange({ ...company, name: event.target.value })
            }
          />
          <FieldError>
            {props.errors?.name?.message ? t(props.errors.name.message) : null}
          </FieldError>
        </Field>
        <Field data-invalid={Boolean(props.errors?.slo)}>
          <FieldLabel htmlFor={`company-${company.id}-slo`}>
            {t('Company SLO')}
          </FieldLabel>
          <Input
            id={`company-${company.id}-slo`}
            type='number'
            min={0}
            max={100}
            step='0.01'
            value={company.slo ?? ''}
            disabled={props.disabled}
            aria-invalid={Boolean(props.errors?.slo)}
            onChange={(event) =>
              props.onChange({
                ...company,
                slo:
                  event.target.value === ''
                    ? undefined
                    : Number(event.target.value),
                slo_confirmed:
                  event.target.value === '' ? false : company.slo_confirmed,
              })
            }
          />
          <FieldError>
            {props.errors?.slo?.message ? t(props.errors.slo.message) : null}
          </FieldError>
        </Field>
      </div>
      <label className='flex items-center gap-2 text-sm'>
        <Checkbox
          checked={company.slo_confirmed ?? (company.slo ?? 0) > 0}
          disabled={props.disabled || company.slo === undefined}
          onCheckedChange={(checked) =>
            props.onChange({ ...company, slo_confirmed: Boolean(checked) })
          }
        />
        {t('Enable company SLO target')}
      </label>
      <div className='grid gap-3 sm:grid-cols-2'>
        <Field>
          <FieldLabel htmlFor={`company-${company.id}-owner`}>
            {t('Owner')}
          </FieldLabel>
          <Input
            id={`company-${company.id}-owner`}
            value={company.owner ?? ''}
            disabled={props.disabled}
            onChange={(event) =>
              props.onChange({ ...company, owner: event.target.value })
            }
          />
        </Field>
        <Field>
          <FieldLabel htmlFor={`company-${company.id}-note`}>
            {t('Note')}
          </FieldLabel>
          <Textarea
            id={`company-${company.id}-note`}
            rows={1}
            value={company.note ?? ''}
            disabled={props.disabled}
            onChange={(event) =>
              props.onChange({ ...company, note: event.target.value })
            }
          />
        </Field>
      </div>
      {(company.groups ?? []).map((group, index) => {
        const errors = props.errors?.groups?.[index]
        return (
          <fieldset
            key={group.id}
            className='min-w-0 space-y-3 border-l-2 pl-3'
          >
            <legend className='mb-2 text-sm font-medium break-all'>
              {group.name}
            </legend>
            <div className='flex flex-wrap items-center justify-between gap-2'>
              <label className='flex items-center gap-2 text-sm'>
                <Checkbox
                  checked={group.enabled !== false}
                  disabled={props.disabled}
                  onCheckedChange={(checked) =>
                    updateGroup(group.id, { enabled: Boolean(checked) })
                  }
                />
                {t('Monitoring enabled')}
              </label>
              <div className='flex max-w-full flex-wrap gap-1'>
                <Button
                  type='button'
                  variant='ghost'
                  size='sm'
                  disabled={props.disabled || group.models.length === 0}
                  onClick={() => props.onMoveModels(group)}
                >
                  {t('Move models')}
                </Button>
                <Button
                  type='button'
                  variant='ghost'
                  size='sm'
                  disabled={props.disabled}
                  onClick={() => props.onDeleteGroup(group)}
                >
                  {t('Delete model group')}
                </Button>
              </div>
            </div>
            <Field data-invalid={Boolean(errors?.name)}>
              <FieldLabel htmlFor={`group-${group.id}-name`}>
                {t('Model group name')}
              </FieldLabel>
              <Input
                id={`group-${group.id}-name`}
                value={group.name}
                disabled={props.disabled}
                aria-invalid={Boolean(errors?.name)}
                onChange={(event) =>
                  updateGroup(group.id, { name: event.target.value })
                }
              />
              <FieldError>
                {errors?.name?.message ? t(errors.name.message) : null}
              </FieldError>
            </Field>
            <Field data-invalid={Boolean(errors?.models)}>
              <FieldLabel htmlFor={`group-${group.id}-models`}>
                {t('Models')}
              </FieldLabel>
              <MultiSelect
                id={`group-${group.id}-models`}
                aria-label={t('Models')}
                aria-invalid={Boolean(errors?.models)}
                options={props.options}
                selected={group.models}
                onChange={(models) => updateGroup(group.id, { models })}
                disabled={props.disabled}
                allowCreate
                placeholder={t('Select models or add custom ones')}
                maxVisibleChips={8}
              />
              <FieldError>
                {errors?.models?.message ? t(errors.models.message) : null}
              </FieldError>
            </Field>
            <div className='grid gap-3 sm:grid-cols-2'>
              <Field data-invalid={Boolean(errors?.slo)}>
                <FieldLabel htmlFor={`group-${group.id}-slo`}>
                  {t('Group SLO')}
                </FieldLabel>
                <Input
                  id={`group-${group.id}-slo`}
                  type='number'
                  min={0}
                  max={100}
                  step='0.01'
                  value={group.slo ?? ''}
                  disabled={props.disabled}
                  aria-invalid={Boolean(errors?.slo)}
                  onChange={(event) =>
                    updateGroup(group.id, {
                      slo:
                        event.target.value === ''
                          ? undefined
                          : Number(event.target.value),
                      slo_confirmed:
                        event.target.value === '' ? false : group.slo_confirmed,
                    })
                  }
                />
                <FieldError>
                  {errors?.slo?.message ? t(errors.slo.message) : null}
                </FieldError>
              </Field>
              <Field data-invalid={Boolean(errors?.min_samples)}>
                <FieldLabel htmlFor={`group-${group.id}-samples`}>
                  {t('Minimum samples')}
                </FieldLabel>
                <Input
                  id={`group-${group.id}-samples`}
                  type='number'
                  min={0}
                  max={1000000000}
                  step={1}
                  value={group.min_samples ?? 100}
                  disabled={props.disabled}
                  aria-invalid={Boolean(errors?.min_samples)}
                  onChange={(event) =>
                    updateGroup(group.id, {
                      min_samples: Number(event.target.value),
                    })
                  }
                />
                <FieldError>
                  {errors?.min_samples?.message
                    ? t(errors.min_samples.message)
                    : null}
                </FieldError>
              </Field>
            </div>
            <label className='flex items-center gap-2 text-sm'>
              <Checkbox
                checked={group.slo_confirmed ?? (group.slo ?? 0) > 0}
                disabled={props.disabled || group.slo === undefined}
                onCheckedChange={(checked) =>
                  updateGroup(group.id, { slo_confirmed: Boolean(checked) })
                }
              />
              {t('Enable group SLO target')}
            </label>
          </fieldset>
        )
      })}
      <Button
        type='button'
        variant='outline'
        size='sm'
        disabled={props.disabled || (company.groups?.length ?? 0) >= 100}
        onClick={props.onAddGroup}
      >
        {t('Add model group')}
      </Button>
    </section>
  )
}
